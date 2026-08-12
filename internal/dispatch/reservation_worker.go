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
//  2. The leave-time gate (MYR-535, holdIfBeforeLeaveTime). The SELECT
//     horizon is now + LeadCap, so a candidate may be up to LeadCap early;
//     it waits here — one lean position read, nothing claimed, no per-row
//     log — until now >= scheduledFor - lead.
//  3. Ask every "may this car be dispatched right now?" question BEFORE
//     claiming — busy, then the vehicle's service state and the owner's
//     ride-sharing pause together in one read (MYR-372 / MYR-342), then the
//     rider's ride capability (MYR-369). The claim is irreversible (the latch
//     admits one winner for the row's lifetime), so claiming a reservation we
//     then decline to push would burn it permanently. Holding costs nothing:
//     the row stays selectable and the next tick re-decides, which turns every
//     one of these into a bounded retry that MaxLateness resolves rather than
//     a refusal. Note what is NOT asked: whether the car is `offline`. See
//     holdIfVehicleBlocked — that one is the dispatcher's wake ladder to
//     answer, not ours to pre-empt.
//  4. Claim, atomically, and only then. The claim re-validates status in SQL,
//     so a rider cancel or an owner picked-up landing after the pass SELECTed
//     the row loses the claim and we do nothing.
//  5. Push, then publish `ride.due` only if the push actually reached the car.
//
// TOCTOU, honestly stated: an instant ride accepted in the gap between step 3
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

	// MYR-535: the SELECT horizon is now + LeadCap, so a candidate may be up
	// to LeadCap early. Gate on the computed leave-time BEFORE any other
	// probe — a waiting candidate costs one lean position read per tick and
	// burns nothing else.
	if s.holdIfBeforeLeaveTime(ctx, r, now) {
		return decisionWaiting
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

	if held := s.holdIfVehicleBlocked(ctx, r); held {
		return decisionHeld
	}

	if held := s.holdIfRiderNotGranted(ctx, r); held {
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

// holdIfVehicleBlocked answers, from ONE vehicle read, both "is this car in
// for service?" (MYR-372) and "does its owner still accept rides?" (MYR-342).
//
// WHAT THE SERVICE ARM CATCHES. The accept gate asks "can this car be
// dispatched right now?" and, for a reservation, correctly declines to answer —
// a car in service today says nothing about a car next Saturday. Its own
// comment names who owes the answer instead: "availability at the reservation
// instant belongs to the scheduled-dispatch machinery, which must re-check the
// vehicle THEN". Nothing did. A car that entered service in the days between
// accept and the due instant was nav-dialled anyway — a rider walking out to
// meet a car that is on a lift, which is exactly the class of edge a first
// external beta produces and has nobody on hand to explain.
//
// ⚠ IT HOLDS ON `in_service` ONLY — NOT ON `offline`. THIS ASYMMETRY IS THE
// POINT, AND REVERSING IT BREAKS EVERY NEW OWNER'S FIRST RESERVATION.
//
// The accept gate refuses both statuses, and copying that here looks like the
// consistent thing to do. It is not, because the two statuses have opposite
// dynamics in this server:
//
//   - `in_service` is set by the service-status monitor and REFRESHED
//     INDEPENDENTLY by the MYR-320 periodic re-poll, which exists precisely
//     because a car at a service centre raises no mTLS connection. Waiting on
//     it is therefore a bounded wait on a fact somebody else is maintaining,
//     and it will clear on its own when the visit ends. Holding is honest.
//
//   - `offline` is the SCHEMA DEFAULT on "Vehicle" (NOT NULL DEFAULT
//     'offline'), and the owned-vehicle provisioning INSERT never sets the
//     column. Nothing writes the value back for a car that has never
//     STREAMED either: every status writer needs a live stream event (a
//     connectivity edge, a completed drive, or — since MYR-454 — any
//     telemetry frame carrying gear/speed), the MYR-320 re-poll selects
//     `in_service` rows only, and the §7.15 refresh writes everything except
//     status. So a key-paired car that has not yet streamed — every new
//     owner, for their first days — sits at `offline` with NOTHING scheduled
//     to correct it.
//
//     MYR-454 narrowed this bullet; it did NOT overturn it. The telemetry
//     fold now heals `offline` the instant a frame arrives, which means the
//     permanently-`offline` population is now EXACTLY the never-streamed
//     cars described here — the ones this hold decision is about. The
//     argument is sharper than when it was written, not weaker.
//
// Holding on `offline` would therefore be self-perpetuating: the sweeper would
// hold every 30 seconds for 30 minutes and then expire the reservation, every
// time, forever. And it would suppress the one thing that WOULD have fixed it.
// Nav dispatch never depended on the telemetry stream — internal/commands
// classifies offline/asleep/unavailable alike as OutcomeAsleep and runs a wake
// ladder within a bounded budget, and this package's own config comments name
// asleep-vehicle pushes as ordinary steady state. Pre-MYR-372 the push fired
// and the ladder woke the car. So the hold would replace a working dispatch
// with a guaranteed `reservation_expired`.
//
// `offline` is consequently left to the dispatcher's existing arbitration,
// exactly as before MYR-372: push, wake, and record an honest outcome if the
// car really cannot be raised. (Accept's `409` on `offline` for INSTANT rides
// is untouched pre-existing MYR-277 behaviour — a different question, asked
// with a human waiting, and not this probe's to change.)
//
// THE VOCABULARY IS NOT RESTATED HERE. The adapter answers via
// telemetry.VehicleStatusIsInService, one arm of the same switch the accept
// gate's 409 is built from, so the `in_service` literal lives in exactly one
// place even though the two readers act on different arms.
//
// HOLDS RATHER THAN EXPIRES, beside the grant probe and BEFORE the claim, for
// the reason they share: the claim is irreversible, so a reservation claimed
// and then not pushed is burnt permanently, while holding is free and the next
// tick re-decides. That is what makes this a BOUNDED RETRY rather than a
// refusal — a car back from service inside the lateness window still gets its
// dispatch and the ride simply happens a few minutes late, while one that
// never returns is resolved honestly at scheduledFor + MaxLateness as
// `reservation_expired`, the same outcome a permanently-busy car produces.
//
// NO NEW WIRE STATUS, deliberately. Held and expired are both already
// expressible: the ride row stays `accepted` throughout, and expiry moves only
// dispatchStatus/dispatch_error. §7.8 documents both, so the client can render
// this honestly today with nothing new to learn.
//
// THE PAUSE ARM (MYR-342) rides the same read. It is the THIRD and last
// enforcement layer of the owner's ride-sharing switch and the only one that
// runs outside a request: the create gate and the accept backstop cannot reach
// backwards to a reservation that was ALREADY accepted when its owner reached
// for the switch, and without this layer a paused car would still be dialled
// days later. It has deliberately no expiry logic of its own — the lateness
// ceiling resolves anything the owner never un-pauses — and an owner who
// un-pauses inside the window still gets the dispatch they meant to allow.
//
// ONE READ SERVES BOTH ARMS, matching the accept gate's discipline of
// resolving everything it needs from a single row rather than from two reads
// that can disagree.
//
// AN UNREADABLE STATE HOLDS, matching every other probe in this file. We
// cannot tell whether pushing would dial a car on a lift or one its owner has
// withdrawn, and a held reservation is recoverable where a wrong push is not.
// A vehicle row that cannot be found at all is an error too, never a
// fabricated "dispatchable" — it holds, and the ceiling resolves it.
func (s *ReservationSweeper) holdIfVehicleBlocked(ctx context.Context, r *DueReservation) bool {
	state, err := s.store.VehicleDispatchState(ctx, r.VehicleID)
	if err != nil {
		s.logger.Error("reservation sweep: vehicle dispatch-state check failed",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
			slog.String("error", err.Error()),
		)
		return true
	}
	if state.InService {
		s.logger.Info("reservation sweep: vehicle is in service, holding",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
			slog.Time("scheduled_for", r.ScheduledFor),
		)
		return true
	}
	if !state.RideShareEnabled {
		s.logger.Info("reservation sweep: ride sharing paused for vehicle, holding",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
			slog.Time("scheduled_for", r.ScheduledFor),
		)
		return true
	}
	return false
}

// holdIfRiderNotGranted is the MYR-369 ride-capability probe — the THIRD and
// last enforcement layer of the per-grant ride flag, and, like the pause arm
// above it, the only one that runs outside a request.
//
// WHAT IT CATCHES that the other two cannot. The create gate refuses a rider who
// lacks the capability, and the accept gate refuses again in case it moved while
// the request sat in the owner's queue. Neither can reach forward to a
// reservation that was ALREADY accepted when its owner suspended the grant or
// patched `allowRides` off — and a reservation may sit accepted for DAYS, which
// is exactly the window an editable capability opens and a fixed tier did not.
// Without this probe, suspending somebody would still let their booked car turn
// up on Saturday.
//
// AN OWNER'S OWN RESERVATION always passes: the store answers true when the
// rider owns the vehicle, so the ordinary case never consults a grant.
//
// HOLDS RATHER THAN EXPIRES, and sits BESIDE the pause probe and BEFORE the
// claim, for the identical reason: the claim is irreversible, so a reservation
// claimed and then not pushed is burnt permanently, while holding is free and
// the next tick re-decides. An owner who un-suspends inside the lateness window
// still gets the dispatch they meant to allow, and one who does not lets the
// reservation expire honestly as `reservation_expired` at
// scheduledFor + MaxLateness — no new resolution path.
//
// AN UNREADABLE GRANT HOLDS, matching both the busy probe and the pause probe.
// Nobody is waiting on an answer here, so the recoverable choice is the right
// one: we cannot tell whether pushing would dial a car to somebody who has been
// cut off, and a held reservation can still be dispatched where a wrong push
// cannot be recalled.
func (s *ReservationSweeper) holdIfRiderNotGranted(ctx context.Context, r *DueReservation) bool {
	granted, err := s.store.RiderMayRequestRides(ctx, r.RiderID, r.VehicleID)
	if err != nil {
		s.logger.Error("reservation sweep: rider grant check failed",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
			slog.String("error", err.Error()),
		)
		return true
	}
	if !granted {
		s.logger.Info("reservation sweep: rider no longer holds the ride capability, holding",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
			slog.Time("scheduled_for", r.ScheduledFor),
		)
		return true
	}
	return false
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
	// MYR-172: the ride row stays `accepted`, so this failure is invisible on
	// the event bus. Tell the rider's Live Activity directly, or it counts down
	// to a pickup that will never happen.
	s.endActivities(ctx, r.RideRequestID)
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
