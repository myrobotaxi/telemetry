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

	listCtx, cancel := context.WithTimeout(ctx, p.cfg.ReconcileTimeout)
	targets, err := p.deps.Rides.ListActiveRideTargets(listCtx, p.cfg.MaxActive)
	cancel()
	if err != nil {
		return 0, 0, fmt.Errorf("ride position poller: list active rides: %w", err)
	}

	wanted := make(map[string]ActiveRideTarget, len(targets))
	for _, t := range targets {
		if t.VehicleID == "" {
			continue
		}
		// First row wins. The list is ordered oldest-updated first, and the
		// per-vehicle accept guard means a second open ride on one car is
		// close to impossible — but if it ever happens, polling the older one
		// is both deterministic and the one more likely to be under way.
		if _, dup := wanted[t.VehicleID]; !dup {
			wanted[t.VehicleID] = t
		}
	}

	reaped = p.reapOrphans(wanted)
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
// poller now running for the WRONG ride on a car that is.
//
// The second case matters: if a car finished ride A and started ride B while
// this process missed both events, the registry would hold a poller keyed to a
// completed ride. Reaping it lets adoptMissing put the right one back, so the
// registry converges in one pass rather than staying subtly wrong until restart.
func (p *RidePositionPoller) reapOrphans(wanted map[string]ActiveRideTarget) int {
	type orphan struct {
		vehicleID string
		reason    string
	}

	p.mu.Lock()
	orphans := make([]orphan, 0, len(p.active))
	for vehicleID, handle := range p.active {
		target, stillWanted := wanted[vehicleID]
		switch {
		case !stillWanted:
			orphans = append(orphans, orphan{vehicleID, "reconcile.ride_ended"})
		case target.RideRequestID != handle.rideRequestID:
			orphans = append(orphans, orphan{vehicleID, "reconcile.ride_replaced"})
		}
	}
	p.mu.Unlock()

	// Cancellation happens OUTSIDE the lock, via the ordinary stopPoll path, so
	// the identity checks that protect against a concurrent start apply here
	// too and there is exactly one place that removes a registry entry.
	for _, o := range orphans {
		p.stopPoll(o.vehicleID, "", o.reason)
	}
	return len(orphans)
}

// adoptMissing starts a poller for every wanted vehicle that has none.
func (p *RidePositionPoller) adoptMissing(wanted map[string]ActiveRideTarget) int {
	adopted := 0
	for vehicleID, target := range wanted {
		if _, running := p.PollingRide(vehicleID); running {
			continue
		}
		before := p.ActiveCount()
		p.startPoll(vehicleID, target.RideRequestID, "reconcile.adopt")
		// startPoll refuses silently when the cap is hit or the component is
		// stopping, so the count — not the call — is what proves adoption.
		if p.ActiveCount() > before {
			adopted++
		}
	}
	return adopted
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
