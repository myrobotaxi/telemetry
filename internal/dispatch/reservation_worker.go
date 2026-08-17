// The per-reservation decision (MYR-179): the lateness ceiling, the leave-time
// gate, the hold probes, and the hand-off to the claimed dispatch path. Runs
// inside a worker holding one of the sweeper's slots, under an
// OverallTimeout-bounded context.
//
// The ceiling's own policy lives in reservation_lapse.go and the claim and its
// two seams in reservation_seam.go; this file is the ORDER those are asked in,
// which is the whole safety argument.

package dispatch

import (
	"context"
	"log/slog"
)

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

	// The claim and everything after it is claimAndDispatch (MYR-556), shared
	// verbatim with the owner's dispatch-now endpoint — see reservation_seam.go
	// for why the sequence has exactly one implementation.
	outcome, err := s.claimAndDispatch(ctx, r, now)
	switch {
	case err != nil:
		s.logger.Error("reservation sweep: claim failed",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
			slog.String("error", err.Error()),
		)
		return decisionHeld
	case outcome == outcomeLost:
		// A peer sweeper won it, or the ride was cancelled/advanced between
		// the SELECT and the claim (the claim re-checks status). Either way it
		// is not ours to dispatch: exactly-once holds, nothing to do, and
		// nothing to log — this is an ordinary outcome, not a fault.
		return decisionLost
	default:
		return decisionDispatched
	}
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
	// THE NAMELESS-OWNER ARM (MYR-581) — the third and last enforcement layer,
	// and the only one that runs outside a request. See the file header's note
	// on monotonicity for why this layer is a BACKSTOP rather than a live gate:
	// namelessness only ever resolves FORWARD, so the population it catches is
	// reservations accepted BEFORE the create gate existed.
	//
	// HOLDS RATHER THAN EXPIRES, beside the pause arm and BEFORE the claim, and
	// the mirror is exact: an owner who sets their name inside the lateness
	// window gets the dispatch they meant to allow, exactly as an owner who
	// un-pauses does; one who never does lets the reservation resolve honestly
	// as `reservation_expired` at scheduledFor + MaxLateness, with no new
	// resolution path and no new wire status.
	//
	// The NAME IS NEVER LOGGED and never read here — the store hands this layer
	// a boolean, so there is no P1 value in the sweep at all.
	if !state.OwnerNamed {
		s.logger.Info("reservation sweep: vehicle owner has no display name, holding",
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
