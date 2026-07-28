// One sweep pass and the per-reservation decision (MYR-179). Split from
// reservation_sweeper.go so the type/config/loop stay separate from the policy.

package dispatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// codeReservationExpiredVehicleBusy is recorded in dispatch_error when a
// reservation came due, waited out the whole busy-hold window because its
// vehicle never finished the ride it was on, and was failed WITHOUT a push.
// It is an opaque outcome code, not a credential, and joins the dispatch-local
// code set documented in rest-api.md §7.8.
const codeReservationExpiredVehicleBusy = "reservation_expired_vehicle_busy"

// sweepResult counts one pass, for logging and tests.
type sweepResult struct {
	due        int // reservations selected as due
	dispatched int // claims won and handed to the nav push
	held       int // skipped this tick: vehicle busy, still inside the hold
	expired    int // failed honestly: busy past the hold window
	lost       int // claim lost to a peer sweeper, or already dispatched
}

// sweepOnce runs one pass: list what is due, then decide each reservation.
// Errors are logged and the pass ends (or the row is skipped) — the next tick
// retries, so no transient database failure can strand a reservation while it
// is still inside its hold window.
func (s *ReservationSweeper) sweepOnce(ctx context.Context) sweepResult {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.SweepTimeout)
	defer cancel()

	now := s.now()
	dueList, err := s.store.ListDueReservations(ctx, now, s.cfg.MaxPerSweep)
	if err != nil {
		s.logger.Error("reservation sweep: list due failed", slog.String("error", err.Error()))
		return sweepResult{}
	}

	res := sweepResult{due: len(dueList)}
	// Indexed rather than ranged by value: DueReservation is wide enough that
	// gocritic flags the per-iteration copy.
	for i := range dueList {
		s.handleDue(ctx, &dueList[i], now, &res)
	}
	if res.due > 0 {
		s.logger.Info("reservation sweep",
			slog.Int("due", res.due),
			slog.Int("dispatched", res.dispatched),
			slog.Int("held", res.held),
			slog.Int("expired", res.expired),
			slog.Int("lost", res.lost),
		)
	}
	return res
}

// handleDue decides one due reservation. The ORDER of the three steps is the
// whole safety argument:
//
//  1. Check busy BEFORE claiming. The claim is irreversible (the latch admits
//     one winner for the row's lifetime), so claiming a reservation we then
//     decline to push would burn it permanently. Holding costs nothing: the
//     row stays selectable and the next tick re-decides.
//  2. Claim, atomically. Whoever wins owns this dispatch; every peer sweeper
//     and every later tick loses and does nothing.
//  3. Publish `ride.due`, then push — both only for the winner, which is what
//     makes the seam exactly-once per reservation.
//
// The nav push runs on the dispatcher's bounded worker pool, so a slow or
// asleep car never delays the rest of the sweep.
func (s *ReservationSweeper) handleDue(ctx context.Context, r *DueReservation, now time.Time, res *sweepResult) {
	busy, err := s.store.VehicleHasActiveInstantRide(ctx, r.VehicleID)
	if err != nil {
		// Unknown busy state: do NOT claim. We cannot tell whether pushing
		// would hijack a live ride, and a held reservation is recoverable
		// where a wrong push is not.
		s.logger.Error("reservation sweep: vehicle busy check failed",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
			slog.String("error", err.Error()),
		)
		res.held++
		return
	}

	if busy {
		if !s.holdExpired(r, now) {
			s.logger.Info("reservation sweep: vehicle busy, holding",
				slog.String("ride_id", r.RideRequestID),
				slog.String("vehicle_id", r.VehicleID),
				slog.Time("scheduled_for", r.ScheduledFor),
			)
			res.held++
			return
		}
		s.expireBusy(ctx, r, res)
		return
	}

	claimed, err := s.dispatcher.store.ClaimDispatch(ctx, r.RideRequestID)
	if err != nil {
		s.logger.Error("reservation sweep: claim failed",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
			slog.String("error", err.Error()),
		)
		res.held++
		return
	}
	if !claimed {
		// A peer sweeper won this reservation (or it was dispatched between
		// the list and the claim). Exactly-once holds; nothing to do.
		res.lost++
		return
	}

	s.publishDue(ctx, r, now)
	res.dispatched++

	leg := s.dispatcher.pickupLeg(r.RideRequestID, r.VehicleID, r.OwnerID, r.Pickup)
	s.dispatcher.dispatchAsync(func(pushCtx context.Context) {
		s.dispatcher.runClaimedLeg(pushCtx, leg)
	})
}

// holdExpired reports whether a due reservation has waited out the busy-hold
// window. The deadline is anchored on scheduledFor (not on first observation),
// so it is a property of the reservation itself: a restart mid-hold resumes
// the same deadline rather than resetting it.
func (s *ReservationSweeper) holdExpired(r *DueReservation, now time.Time) bool {
	return now.After(r.ScheduledFor.Add(s.cfg.BusyHold))
}

// expireBusy resolves a reservation whose vehicle stayed busy past the hold
// window. It claims the latch and records an honest `failed` /
// reservation_expired_vehicle_busy WITHOUT pushing nav.
//
// Claiming here is deliberate and is the point: it converts an invisible
// "forever pending" row into a resolved, alertable outcome that surfaces on
// the existing dispatchStatus surface, and it stops the sweeper from
// re-evaluating the row every 30s forever. No `ride.due` is published — the
// reservation never actually dispatched, and the seam's future consumer is a
// "your car is on the way" notification that would be false here.
func (s *ReservationSweeper) expireBusy(ctx context.Context, r *DueReservation, res *sweepResult) {
	claimed, err := s.dispatcher.store.ClaimDispatch(ctx, r.RideRequestID)
	if err != nil {
		s.logger.Error("reservation sweep: claim for expiry failed",
			slog.String("ride_id", r.RideRequestID),
			slog.String("error", err.Error()),
		)
		res.held++
		return
	}
	if !claimed {
		res.lost++
		return
	}

	code := codeReservationExpiredVehicleBusy
	leg := s.dispatcher.pickupLeg(r.RideRequestID, r.VehicleID, r.OwnerID, r.Pickup)
	s.dispatcher.record(ctx, leg, "", OutcomeFailed, &code, "")
	s.logger.Warn("reservation sweep: vehicle busy past hold window, failing reservation",
		slog.String("ride_id", r.RideRequestID),
		slog.String("vehicle_id", r.VehicleID),
		slog.Time("scheduled_for", r.ScheduledFor),
		slog.Duration("busy_hold", s.cfg.BusyHold),
	)
	res.expired++
}

// publishDue emits the `ride.due` seam for a reservation whose claim we just
// won. Fire-and-forget and drop-safe: a nil bus or a publish failure is logged
// and the dispatch proceeds regardless — the nav push is the contract, the
// event is only a notification hook.
func (s *ReservationSweeper) publishDue(ctx context.Context, r *DueReservation, now time.Time) {
	if s.bus == nil {
		return
	}
	evt := events.NewEvent(events.RideDueEvent{
		RideRequestID: r.RideRequestID,
		VehicleID:     r.VehicleID,
		RiderID:       r.RiderID,
		OwnerID:       r.OwnerID,
		ScheduledFor:  r.ScheduledFor,
		DueAt:         now,
	})
	if err := s.bus.Publish(ctx, evt); err != nil {
		s.logger.Warn("reservation sweep: publish ride.due failed",
			slog.String("ride_id", r.RideRequestID),
			slog.String("error", err.Error()),
		)
	}
}
