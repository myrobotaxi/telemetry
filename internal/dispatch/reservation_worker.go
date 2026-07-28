// The per-reservation decision (MYR-179): expiry, the vehicle-busy hold, the
// just-in-time claim, the push, and the `ride.due` seam. Runs inside a worker
// holding one of the sweeper's slots, under an OverallTimeout-bounded context.

package dispatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// codeReservationExpired is recorded in dispatch_error when a reservation came
// due but was still unclaimed past its lateness ceiling (scheduledFor +
// MaxLateness), so it was failed WITHOUT a push. The two ways to get here are
// a vehicle that stayed on another ride for the whole window and a gap in
// dispatch itself (downtime, or the reservation kill-switch); the outcome is
// the same either way, so the code is one generalised value rather than a
// per-cause family. The distinguishing detail rides the log line, not the
// column.
//
// It is an opaque outcome code, not a credential, and joins the dispatch-local
// code set documented in rest-api.md §7.8.
const codeReservationExpired = "reservation_expired"

// handleDue decides one due reservation. The ORDER of the steps is the whole
// safety argument:
//
//  1. Lateness ceiling FIRST. Past scheduledFor + MaxLateness the reservation
//     is expired no matter what the car is doing. A nav push that is hours
//     late — after downtime, or after the kill-switch was re-enabled — dials a
//     car whose rider gave up, which is strictly worse than an honest,
//     alertable failure (the MYR-176 reconciler stance, applied to the
//     sweeper's much larger lateness surface).
//  2. Check busy BEFORE claiming. The claim is irreversible (the latch admits
//     one winner for the row's lifetime), so claiming a reservation we then
//     decline to push would burn it permanently. Holding costs nothing: the
//     row stays selectable and the next tick re-decides.
//  3. Claim, atomically, and only then. The claim re-validates status in SQL,
//     so a rider cancel or an owner picked-up landing after the pass SELECTed
//     the row loses the claim and we do nothing.
//  4. Push, then publish `ride.due` only if the push actually reached the car.
//
// TOCTOU, honestly stated: an instant ride accepted in the gap between step 2
// and the push still races the reservation, and last writer wins the car's
// navigation. That gap is now SECONDS (one busy probe plus one claim, both
// adjacent to the push in the same worker) rather than the minutes the
// original pass-start check allowed, and closing it entirely needs the busy
// predicate to arbitrate the accept as well — a v1 boundary documented in
// rest-api.md §7.8, not something the sweeper can fix alone.
func (s *ReservationSweeper) handleDue(ctx context.Context, r *DueReservation) sweepDecision {
	now := s.now()

	if s.pastDeadline(r, now) {
		return s.expire(ctx, r)
	}

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
		return decisionHeld
	}
	if busy {
		s.logger.Info("reservation sweep: vehicle busy, holding",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
			slog.Time("scheduled_for", r.ScheduledFor),
		)
		return decisionHeld
	}

	claimed, err := s.store.ClaimReservationDispatch(ctx, r.RideRequestID)
	if err != nil {
		s.logger.Error("reservation sweep: claim failed",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
			slog.String("error", err.Error()),
		)
		return decisionHeld
	}
	if !claimed {
		// A peer sweeper won it, or the ride was cancelled/advanced between
		// the SELECT and the claim (the claim re-checks status). Either way it
		// is not ours to dispatch: exactly-once holds, nothing to do, and
		// nothing to log — this is an ordinary outcome, not a fault.
		return decisionLost
	}

	leg := s.dispatcher.pickupLeg(r.RideRequestID, r.VehicleID, r.OwnerID, r.Pickup)
	if s.dispatcher.runClaimedLeg(ctx, leg) == OutcomeSent {
		// `ride.due` is published only once the car actually took the pickup.
		// The topic's contract is "your car is on the way", so a kill-switched
		// (`skipped`) or failed push must not emit it — the latch admits one
		// winner, so a false event could never be corrected by a later one.
		s.publishDue(ctx, r, now)
	}
	return decisionDispatched
}

// pastDeadline reports whether a due reservation has passed its lateness
// ceiling. The deadline is anchored on scheduledFor (not on first
// observation), so it is a property of the reservation itself: a restart
// mid-hold resumes the same deadline rather than resetting it, and downtime
// cannot buy a stale reservation a fresh window.
//
// now is read INSIDE the worker rather than at pass start, so a candidate that
// waited behind the sweeper's worker budget is judged on when it is actually
// about to be pushed.
func (s *ReservationSweeper) pastDeadline(r *DueReservation, now time.Time) bool {
	return now.After(r.ScheduledFor.Add(s.cfg.MaxLateness))
}

// expire resolves a reservation that is past its lateness ceiling. It claims
// the latch and records an honest `failed` / reservation_expired WITHOUT
// pushing nav — whatever the vehicle happens to be doing.
//
// Claiming here is deliberate and is the point: it converts an invisible
// "forever pending" row into a resolved, alertable outcome that surfaces on
// the existing dispatchStatus surface, and it stops the sweeper from
// re-evaluating the row every 30s forever. No `ride.due` is published — the
// reservation never actually dispatched, and the seam's future consumer is a
// "your car is on the way" notification that would be false here.
//
// The claim re-validates status too, so a reservation the rider cancelled is
// left alone rather than stamped with a dispatch failure it never had.
func (s *ReservationSweeper) expire(ctx context.Context, r *DueReservation) sweepDecision {
	claimed, err := s.store.ClaimReservationDispatch(ctx, r.RideRequestID)
	if err != nil {
		s.logger.Error("reservation sweep: claim for expiry failed",
			slog.String("ride_id", r.RideRequestID),
			slog.String("error", err.Error()),
		)
		return decisionHeld
	}
	if !claimed {
		return decisionLost
	}

	code := codeReservationExpired
	leg := s.dispatcher.pickupLeg(r.RideRequestID, r.VehicleID, r.OwnerID, r.Pickup)
	s.dispatcher.record(ctx, leg, "", OutcomeFailed, &code, "")
	s.logger.Warn("reservation sweep: past the lateness ceiling, failing reservation",
		slog.String("ride_id", r.RideRequestID),
		slog.String("vehicle_id", r.VehicleID),
		slog.Time("scheduled_for", r.ScheduledFor),
		slog.Duration("max_lateness", s.cfg.MaxLateness),
	)
	return decisionExpired
}

// publishDue emits the `ride.due` seam for a reservation whose pickup push
// reached the car. Fire-and-forget and drop-safe: a nil bus or a publish
// failure is logged and the dispatch stands — the nav push is the contract,
// the event is only a notification hook.
func (s *ReservationSweeper) publishDue(ctx context.Context, r *DueReservation, dueAt time.Time) {
	if s.bus == nil {
		return
	}
	evt := events.NewEvent(events.RideDueEvent{
		RideRequestID: r.RideRequestID,
		VehicleID:     r.VehicleID,
		RiderID:       r.RiderID,
		OwnerID:       r.OwnerID,
		ScheduledFor:  r.ScheduledFor,
		DueAt:         dueAt,
	})
	if err := s.bus.Publish(ctx, evt); err != nil {
		s.logger.Warn("reservation sweep: publish ride.due failed",
			slog.String("ride_id", r.RideRequestID),
			slog.String("error", err.Error()),
		)
	}
}
