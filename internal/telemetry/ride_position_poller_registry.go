package telemetry

// MYR-394 — the poller REGISTRY: the per-vehicle handle, and the guarded
// accessors every other file in this component reaches it through.
//
// Split out of ride_position_poller.go to keep that file inside the 300-line
// cap, and it is a coherent split rather than an arbitrary one: everything here
// is about what the registry HOLDS and how it is read under the lock, while the
// parent file owns the component's construction and lifetime. The one rule that
// spans both: activePoll.rides and activePoll.deadline are guarded by
// RidePositionPoller.mu, so nothing outside this file touches them directly.

import (
	"context"
	"sort"
	"strings"
	"time"
)

// activePoll is one running poller's handle: what it polls, and the lever that
// stops it.
type activePoll struct {
	// rides is the SET of ride ids currently justifying this vehicle's poller,
	// and it is a set rather than a single id because the registry is keyed by
	// VEHICLE while the lifecycle events that open and close pollers are keyed
	// by RIDE. Two open rides on one car are reachable — activeInstantRide
	// deliberately does not count a dispatched-but-unstarted reservation as
	// "busy", and the MYR-383 window gate only refuses rides whose instants
	// fall within ±W — and with a single stored id the two halves were not
	// symmetric: the second ride got no handle, and the FIRST ride's terminal
	// event then cancelled the poller the second one was relying on. The set
	// makes the rule "poll while ANY ride on this car is live" instead of
	// "poll while the ride that happened to arrive first is live".
	//
	// Guarded by RidePositionPoller.mu — startPoll and stopPoll mutate it, and
	// the poll goroutine reads it only through rideLabel.
	rides map[string]struct{}

	// deadline is the wall-clock moment this poller stops regardless of what
	// the ride table says (see RidePositionPollConfig.MaxLifetime). Extended
	// whenever a new ride joins the set: a fresh ride is fresh justification.
	// Guarded by RidePositionPoller.mu.
	deadline time.Time

	// vin is the cached VIN, touched ONLY by the poll goroutine that owns this
	// handle, so it needs no lock.
	vin string

	cancel context.CancelFunc
}

// hasRide reports set membership. The caller must hold p.mu.
func (h *activePoll) hasRide(rideRequestID string) bool {
	_, ok := h.rides[rideRequestID]
	return ok
}

// rideIDs returns the handle's rides, sorted so logs and assertions are
// deterministic. The caller must hold p.mu.
func (h *activePoll) rideIDs() []string {
	ids := make([]string, 0, len(h.rides))
	for id := range h.rides {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// setNow replaces the lifetime clock. Test hook only.
func (p *RidePositionPoller) setNow(fn func() time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.now = fn
}

// clock reads the injected clock under the lock.
func (p *RidePositionPoller) clock() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.now()
}

// ActiveCount reports how many vehicles are currently being polled.
func (p *RidePositionPoller) ActiveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.active)
}

// PollingRides reports which rides a vehicle is currently polled for, sorted,
// and whether it is polled at all. Exported so the reconcile's callers and the
// tests can observe the registry; there is no other way in from outside.
//
// It returns a SET rather than one id because one car can legitimately be
// carrying two open rides — see activePoll.rides.
func (p *RidePositionPoller) PollingRides(vehicleID string) ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.active[vehicleID]
	if !ok {
		return nil, false
	}
	return h.rideIDs(), true
}

// rideLabel renders a handle's ride set for a log line. Takes the lock, so the
// poll goroutine calls it once per cycle rather than per log statement.
func (p *RidePositionPoller) rideLabel(handle *activePoll) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(handle.rideIDs(), ",")
}

// lifetimeExceeded reports whether this poller has outlived MaxLifetime.
//
// It is the DEFENSIVE half of the poll-lifetime ceiling; the authoritative half
// is the age bound inside pollableRidePredicate, which stops the reconcile from
// ever re-adopting a ride this old. This check exists because a poller can also
// be opened by a bus seam between reconcile passes, and nothing in this system
// automatically terminates a ride: a rider no-show leaves the row at `arrived`
// forever, and without a ceiling the loop would read that car's position every
// 25 seconds until the process died.
func (p *RidePositionPoller) lifetimeExceeded(handle *activePoll, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !handle.deadline.IsZero() && !now.Before(handle.deadline)
}

// pollStreamFresh reports whether the car is streaming right now, per the
// MYR-300 per-VIN recency window. The poll yields when it is.
func (p *RidePositionPoller) pollStreamFresh(vin string) bool {
	if p.deps.Streams == nil {
		return false
	}
	_, fresh := p.deps.Streams.LastStreamAt(vin)
	return fresh
}
