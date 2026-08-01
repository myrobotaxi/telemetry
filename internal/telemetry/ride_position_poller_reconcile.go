package telemetry

// MYR-394 — the reconcile: make the in-memory registry agree with the database.
//
// The event seams are the fast path and the database is the truth. Three things
// the seams cannot do on their own, all of which have bitten this codebase
// before (the MYR-176 / MYR-266 reconcile history):
//
//  1. A RESTART loses every poller. Rides that were live when the process died
//     are still live when it comes back, and nothing would ever poll them again.
//  2. A DROPPED EVENT leaks a poller. The bus is drop-OLDEST under backpressure
//     (internal/events/subscriber.go), so a terminal transition CAN go missing —
//     and the poller it was supposed to stop would then run until the process
//     ends, quietly spending Fleet API budget on a finished ride.
//  3. A ride that ended while this process was down leaves a row the seams will
//     never mention again.
//
// So the pass is symmetric — adopt what is missing, reap what is orphaned — and
// it runs both at startup and on a slow cadence. It is the mechanism that makes
// "a poller must not survive its ride" true rather than merely intended.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// Reconcile makes the registry match the database exactly once.
//
// Returns the number of pollers adopted and reaped. A LIST failure returns the
// error with both counts zero and — deliberately — changes NOTHING: a database
// blip must not be read as "no rides are active" and tear down every live
// poller on the box.
func (p *RidePositionPoller) Reconcile(ctx context.Context) (adopted, reaped int, err error) {
	if !p.cfg.Enabled {
		return 0, 0, nil
	}

	// Ask for ONE MORE ROW than the registry can hold. That extra row carries
	// no poller; it is the truncation detector. Reaping is a decision made
	// against the ABSENCE of a vehicle from this list, and an absence only
	// means "the ride ended" when the list is complete — see below.
	listCtx, cancel := context.WithTimeout(ctx, p.cfg.ReconcileTimeout)
	targets, err := p.deps.Rides.ListActiveRideTargets(listCtx, p.cfg.MaxActive+1)
	cancel()
	if err != nil {
		return 0, 0, fmt.Errorf("ride position poller: list active rides: %w", err)
	}
	truncated := len(targets) > p.cfg.MaxActive
	if truncated {
		targets = targets[:p.cfg.MaxActive]
	}

	// Vehicle -> the SET of rides currently justifying a poller for it. Not
	// first-row-wins: a car legitimately carrying two open rides must keep its
	// poller when the first of them ends, and collapsing to one row here was
	// the mirror image of the teardown gap startPoll/stopPoll now close — the
	// pass would cancel the newer ride's poller and re-adopt the older one.
	wanted := make(map[string]map[string]struct{}, len(targets))
	for _, t := range targets {
		if t.VehicleID == "" || t.RideRequestID == "" {
			continue
		}
		if wanted[t.VehicleID] == nil {
			wanted[t.VehicleID] = make(map[string]struct{}, 1)
		}
		wanted[t.VehicleID][t.RideRequestID] = struct{}{}
	}

	// A TRUNCATED LIST IS NOT AUTHORITATIVE, for exactly the reason a failed
	// LIST is not: an incomplete answer must never be read as "these rides
	// ended". The pass becomes adopt-only.
	//
	// The failure this prevents is not hypothetical. Nothing terminates an
	// abandoned ride, so never-ending rows accumulate and — with the six-hour
	// age bound as the only thing draining them — could still fill MaxActive
	// during a bad enough spell. Once they do, every pass returns a full page,
	// and reaping against it would stop the pollers that the ride.accepted /
	// ride.due seams had just started for genuinely live rides, five minutes
	// after each one began, with no cap headroom left to re-adopt them.
	if truncated {
		p.logger.Warn("ride position poll: active-ride list hit the cap, reaping skipped this pass",
			slog.Int("max_active", p.cfg.MaxActive),
			slog.Int("active", p.ActiveCount()))
	} else {
		reaped = p.reapOrphans(wanted)
	}
	adopted = p.adoptMissing(wanted)

	if adopted > 0 || reaped > 0 {
		p.logger.Info("ride position poll reconciled",
			slog.Int("adopted", adopted),
			slog.Int("reaped", reaped),
			slog.Int("active", p.ActiveCount()))
	}
	return adopted, reaped, nil
}

// reapOrphans stops every poller whose vehicle is no longer mid-ride, and every
// poller running for a set of rides that has NOTHING in common with what the
// database says is live on that car.
//
// The second case matters: if a car finished ride A and started ride B while
// this process missed both events, the registry would hold a poller keyed to a
// completed ride. Reaping it lets adoptMissing put the right one back, so the
// registry converges in one pass rather than staying subtly wrong until restart.
//
// DISJOINT, not unequal. Where the two sets merely disagree — the car is
// carrying rides {A, B}, the handle remembers {A} — the poller is still
// justified by A, so cancelling and immediately re-adopting it would open a gap
// in a live rider's tracking to fix a bookkeeping difference. The set is
// re-synced in place instead.
func (p *RidePositionPoller) reapOrphans(wanted map[string]map[string]struct{}) int {
	type orphan struct {
		vehicleID string
		reason    string
	}

	p.mu.Lock()
	orphans := make([]orphan, 0, len(p.active))
	resync := make(map[string]map[string]struct{})
	for vehicleID, handle := range p.active {
		wantedRides, stillWanted := wanted[vehicleID]
		switch {
		case !stillWanted:
			orphans = append(orphans, orphan{vehicleID, "reconcile.ride_ended"})
		case !ridesOverlap(wantedRides, handle.rides):
			orphans = append(orphans, orphan{vehicleID, "reconcile.ride_replaced"})
		default:
			resync[vehicleID] = wantedRides
		}
	}
	p.mu.Unlock()

	for vehicleID, rides := range resync {
		p.syncRides(vehicleID, rides)
	}

	// Cancellation happens OUTSIDE the lock, via the ordinary stopPoll path, so
	// the identity checks that protect against a concurrent start apply here
	// too and there is exactly one place that removes a registry entry.
	for _, o := range orphans {
		p.stopPoll(o.vehicleID, "", o.reason)
	}
	return len(orphans)
}

// ridesOverlap reports whether the two ride sets share at least one id.
func ridesOverlap(a, b map[string]struct{}) bool {
	for id := range a {
		if _, ok := b[id]; ok {
			return true
		}
	}
	return false
}

// adoptMissing starts a poller for every wanted vehicle that has none, and adds
// any ride the database knows about that an existing poller does not.
func (p *RidePositionPoller) adoptMissing(wanted map[string]map[string]struct{}) int {
	adopted := 0
	for vehicleID, rides := range wanted {
		_, running := p.PollingRides(vehicleID)
		before := p.ActiveCount()
		for _, rideRequestID := range sortedRideIDs(rides) {
			p.startPoll(vehicleID, rideRequestID, "reconcile.adopt")
		}
		// startPoll refuses silently when the cap is hit or the component is
		// stopping, so the count — not the call — is what proves adoption. A
		// car that was already being polled has joined rides to an existing
		// handle, which is a correction rather than an adoption.
		if !running && p.ActiveCount() > before {
			adopted++
		}
	}
	return adopted
}

// sortedRideIDs makes adoption order deterministic, so which ride id lands in
// the "started" log line does not depend on map iteration.
func sortedRideIDs(rides map[string]struct{}) []string {
	ids := make([]string, 0, len(rides))
	for id := range rides {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// RunReconcileLoop re-runs Reconcile on a jittered cadence until ctx is
// cancelled. Started with a bare `go` by cmd/ wiring, matching the reservation
// sweeper and the Live Activity ticker.
//
// There is no startup pass here — cmd/ runs the first Reconcile inline and
// synchronously, so that a restart has already re-adopted its live rides before
// the HTTP server starts accepting the requests that ask where those cars are.
func (p *RidePositionPoller) RunReconcileLoop(ctx context.Context) {
	if !p.cfg.Enabled {
		return
	}
	p.logger.Info("ride position poll reconcile loop started",
		slog.Duration("interval", p.cfg.ReconcileInterval))

	for {
		timer := time.NewTimer(jitterDuration(p.cfg.ReconcileInterval, p.cfg.JitterFraction))
		select {
		case <-ctx.Done():
			timer.Stop()
			p.logger.Info("ride position poll reconcile loop stopped")
			return
		case <-timer.C:
		}
		if _, _, err := p.Reconcile(ctx); err != nil {
			// Non-fatal by design: the registry simply stays as it was and the
			// next tick tries again. See Reconcile on why a failed LIST must
			// never be read as "no rides are active".
			p.logger.Warn("ride position poll reconcile failed (non-fatal)",
				slog.String("error", err.Error()))
		}
	}
}
