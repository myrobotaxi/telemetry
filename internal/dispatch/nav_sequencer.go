// Per-vehicle nav-push ordering (MYR-526). The car's navigation destination is
// a SINGLE last-write-wins resource, but the two nav legs of one ride are
// dispatched by two independent bus deliveries onto a shared worker pool with
// nothing ordering them. This file is what orders them.

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// navSlotTTL is how long a vehicle's leg high-water mark is remembered
	// after its last push. It must comfortably outlive the window in which a
	// STALE lower leg can still arrive — the reachable case is a reservation
	// whose sweeper claims leg 1 after the ride was picked up past due and
	// already started (MYR-376 lets that happen), which is minutes, not hours.
	// The mark cannot simply be dropped when the last hold is released: that is
	// exactly when a late leg 1 shows up, and a forgotten mark lets it through.
	navSlotTTL = 6 * time.Hour
	// navPruneInterval bounds how often the expiry sweep walks the map, so a
	// busy dispatcher does not re-scan on every push. The map holds one small
	// struct per vehicle dispatched within the TTL — the fleet, not the history.
	navPruneInterval = time.Minute
)

// legOrder is a nav leg's monotonic position WITHIN ONE RIDE. The car's nav
// must end up on the highest-ordered leg that has been reached, whatever order
// the pushes happen to complete in.
type legOrder int

const (
	legOrderPickup  legOrder = 1 // leg 1 — on accept (or at `scheduledFor`)
	legOrderDropoff legOrder = 2 // leg 2 — on the rider's start
)

// errNavSuperseded is returned by navSequencer.acquire when a HIGHER-ordered
// leg of the SAME ride has already reached the car. The caller must not push:
// doing so would drag the car's nav BACKWARDS onto a target the ride has
// already left behind.
var errNavSuperseded = errors.New("dispatch: nav leg superseded by a later leg of the same ride")

// navSequencer serializes nav pushes PER VEHICLE and enforces monotonic leg
// order PER RIDE.
//
// THE DEFECT IT CLOSES (MYR-526, live 2026-08-09). Leg 1 (pickup, on accept)
// and leg 2 (dropoff, on the rider's start) are separate ride.accepted /
// ride.started deliveries handed to the same bounded worker pool. Each claims
// its own exactly-once latch and then runs resolve → command, and NOTHING
// ordered the two Tesla calls. The claim timestamps are stamped BEFORE that
// work, so a leg-1 push that stalls — an asleep car costs the executor a wake
// plus up to three 2s-spaced attempts, and the dispatcher retries transient
// failures on top of that — can easily still be in flight 10-20 seconds after
// it claimed. When the rider taps Start inside that window (12.4s in the
// incident), leg 2's dropoff share reaches the car FIRST and leg 1's pickup
// share lands AFTER it, overwriting the destination the ride had just moved to.
//
// Both legs then record `sent`, because both genuinely were: Tesla accepted
// each share. The ride row, the app, and the audit line all agree the drop-off
// was pushed, while the dash sits at the kerb showing "At Destination / End
// Trip" for the PICKUP. That is precisely the shape that made the incident
// unreadable from the outside, and no amount of typed errors or retries on the
// command itself would have caught it — there was no error.
//
// THE GUARANTEE. For one vehicle, at most one nav push is in flight at a time,
// and a push whose leg has been overtaken never reaches the car:
//
//   - Pushes for a vehicle SERIALIZE on a per-vehicle gate, so two legs can no
//     longer interleave and finish out of order.
//   - A higher leg of the SAME ride arriving while a lower one is in flight
//     CANCELS it, so leg 2 is never stuck queueing behind leg 1's wake ladder
//     (which is the exact case that produced the incident — leg 2 must be able
//     to overtake, it just must not be overtaken BACK).
//   - A leg that finds a higher leg of its own ride already delivered is
//     refused outright with errNavSuperseded and records `skipped` /
//     `nav_superseded` rather than silently dialing a stale destination.
//
// Supersession is keyed by RIDE, never by vehicle: a NEW ride's pickup (order
// 1) must not be refused because the PREVIOUS ride's dropoff (order 2) was the
// last thing this car was told. Only the ordering gate is per vehicle.
//
// The sequencer is in-process, which is sufficient: both legs of a ride are
// dispatched by the same server, and a process that dies mid-leg leaves the
// claimed-but-unresolved shape the startup reconciler already owns.
type navSequencer struct {
	mu    sync.Mutex
	slots map[string]*navSlot
	// now is the clock, injectable so the retention sweep is testable without
	// sleeping. Nil is time.Now.
	now func() time.Time
	// lastPrune is when the expiry sweep last ran.
	lastPrune time.Time
}

// navSlot is one vehicle's ordering state. `gate` is the serialization lock (a
// capacity-1 channel rather than a sync.Mutex so a waiter can abandon it on
// ctx cancellation instead of blocking past its deadline). Every other field is
// guarded by navSequencer.mu.
type navSlot struct {
	gate chan struct{}
	// refs counts goroutines between entering acquire and completing release,
	// so the slot is reclaimed only when nobody can still be holding it.
	refs int
	// cur is the hold currently in flight for this vehicle, if any.
	cur *navHold
	// lastRide / lastOrder are the highest leg DELIVERED (attempted against
	// Tesla) for the most recent ride to use this vehicle.
	lastRide  string
	lastOrder legOrder
	// touchedAt is when this slot was last used, for the retention sweep.
	touchedAt time.Time
}

// navHold is one goroutine's exclusive right to push nav to a vehicle. Release
// it exactly once, with defer, as soon as the Tesla call has resolved.
type navHold struct {
	seq       *navSequencer
	slot      *navSlot
	vehicleID string
	rideID    string
	order     legOrder
	// cancel stops this leg's work when a higher leg of the same ride takes
	// over. Guarded by seq.mu, as are the two fields below.
	cancel     context.CancelFunc
	superseded bool
	released   bool
}

// newNavSequencer builds an empty sequencer on the wall clock.
func newNavSequencer() *navSequencer {
	return &navSequencer{slots: make(map[string]*navSlot), now: time.Now}
}

// clock reads the sequencer's time source.
func (s *navSequencer) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// acquire takes the vehicle's nav gate for one leg. cancel is this leg's own
// cancellation func, invoked if a higher leg of the same ride supersedes it
// mid-flight; pass the cancel of the ctx the caller will use for its Tesla work.
//
// It returns errNavSuperseded when a higher leg of the same ride already
// reached the car (the caller must NOT push), or ctx's error when the wait for
// the gate was cut short. On success the caller MUST Release the hold.
func (s *navSequencer) acquire(
	ctx context.Context, vehicleID, rideID string, order legOrder, cancel context.CancelFunc,
) (*navHold, error) {
	s.mu.Lock()
	s.pruneLocked()
	slot := s.slots[vehicleID]
	if slot == nil {
		slot = &navSlot{gate: make(chan struct{}, 1)}
		s.slots[vehicleID] = slot
	}
	slot.refs++
	slot.touchedAt = s.clock()
	// Take over from an in-flight LOWER leg of the same ride. Without this, leg
	// 2 would queue behind leg 1's wake-and-retry ladder (up to OverallTimeout)
	// — correct in ordering but useless in practice, since the rider is already
	// moving. Cancelling leg 1 is also the honest outcome: its destination has
	// been superseded, so completing it would only push a stale target.
	if cur := slot.cur; cur != nil && cur.rideID == rideID && cur.order < order {
		cur.superseded = true
		if cur.cancel != nil {
			cur.cancel()
		}
	}
	s.mu.Unlock()

	select {
	case slot.gate <- struct{}{}:
	case <-ctx.Done():
		s.abandon(vehicleID, slot)
		return nil, fmt.Errorf("dispatch: waiting for the %s nav gate: %w", vehicleID, ctx.Err())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if slot.lastRide == rideID && slot.lastOrder >= order {
		// A later leg of THIS ride already reached the car; pushing now would
		// walk the destination backwards.
		<-slot.gate
		slot.refs--
		return nil, errNavSuperseded
	}
	h := &navHold{seq: s, slot: slot, vehicleID: vehicleID, rideID: rideID, order: order, cancel: cancel}
	slot.cur = h
	return h, nil
}

// Superseded reports whether a higher leg of the same ride took over while this
// hold was in flight. A leg that fails AND was superseded failed because we
// stopped it, and must be recorded as superseded rather than as a Tesla
// failure or a bare cancellation.
func (h *navHold) Superseded() bool {
	h.seq.mu.Lock()
	defer h.seq.mu.Unlock()
	return h.superseded
}

// Release gives up the vehicle's nav gate and records this leg as the highest
// one delivered for its ride. Idempotent.
func (h *navHold) Release() {
	s := h.seq
	s.mu.Lock()
	defer s.mu.Unlock()
	if h.released {
		return
	}
	h.released = true
	if h.slot.cur == h {
		h.slot.cur = nil
	}
	// A superseded leg never reached the car, so it must not raise the
	// high-water mark and refuse the leg that superseded it.
	if !h.superseded && (h.slot.lastRide != h.rideID || h.slot.lastOrder < h.order) {
		h.slot.lastRide = h.rideID
		h.slot.lastOrder = h.order
	}
	h.slot.refs--
	h.slot.touchedAt = s.clock()
	<-h.slot.gate // never blocks: this goroutine holds the gate
}

// abandon drops a reference taken by an acquire that never got the gate. A slot
// that never delivered anything carries no high-water mark worth keeping, so it
// goes immediately rather than waiting out the retention window.
func (s *navSequencer) abandon(vehicleID string, slot *navSlot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot.refs--
	if slot.refs <= 0 && slot.lastRide == "" {
		delete(s.slots, vehicleID)
	}
}

// pruneLocked drops idle slots whose high-water mark has aged out, at most once
// per navPruneInterval. Caller holds s.mu.
//
// Reclamation is deliberately NOT "delete when the last hold is released": the
// released moment is precisely when a stale lower leg can still turn up, and a
// forgotten mark would let it push the car back to an abandoned target.
func (s *navSequencer) pruneLocked() {
	now := s.clock()
	if !s.lastPrune.IsZero() && now.Sub(s.lastPrune) < navPruneInterval {
		return
	}
	s.lastPrune = now
	for id, slot := range s.slots {
		if slot.refs <= 0 && now.Sub(slot.touchedAt) > navSlotTTL {
			delete(s.slots, id)
		}
	}
}
