// The CLAIMED DISPATCH PATH and the two seams it fires. Split out of
// reservation_worker.go (MYR-556) because it now has a second caller: the
// owner's `POST /api/ride-requests/{id}/dispatch-now`.
//
// EXTRACTED RATHER THAN COPIED, and that is the whole point of the file. The
// sequence — claim, run the claimed leg, and publish the two seams only if the
// car actually took the pickup — is where every exactly-once property of
// scheduled dispatch lives. A second copy of it would be a second place for the
// `ride.due` latch rule to be got wrong, and the failure mode of getting it
// wrong is a "your car is on its way" notification for a car that is not.

package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// dispatchOutcome is what one run of the claimed dispatch path did with the
// leg-1 latch. It says nothing about whether the nav push SUCCEEDED — that is
// recorded on the row by runClaimedLeg and read back from it.
type dispatchOutcome int

const (
	// outcomeRan: this call won the claim and the push pipeline ran.
	outcomeRan dispatchOutcome = iota
	// outcomeLost: the claim went to a peer sweeper, or the ride was
	// cancelled/advanced between the read and the claim (the claim re-validates
	// status in SQL). Nothing was pushed and nothing is owed.
	outcomeLost
)

// claimAndDispatch is the irreversible half of reservation dispatch: win the
// leg-1 latch, then push the pickup and announce it.
//
// THE ORDER IS THE EXACTLY-ONCE ARGUMENT. The latch admits ONE winner for the
// row's lifetime, so every "may this car be dispatched right now?" question
// must already have been asked — a reservation claimed and then not pushed is
// burnt permanently. Callers own that half: the sweeper holds through its four
// probes, and the dispatch-now endpoint refuses in front of a human.
//
// BOTH SEAMS FIRE ONLY ON `sent`, and for the same reason. `ride.due` means
// "your car is on the way", and the MYR-555 frame tells an open app the same
// thing in the client's own vocabulary; a kill-switched (`skipped`) or failed
// push must emit neither, because the latch admits one winner and a false
// event could never be corrected by a later one.
//
// dueAt is the caller's clock reading at the moment it decided to dispatch —
// the sweeper's worker clock, or the instant the owner's tap arrived.
func (s *ReservationSweeper) claimAndDispatch(
	ctx context.Context, r *DueReservation, dueAt time.Time,
) (dispatchOutcome, error) {
	claimed, err := s.store.ClaimReservationDispatch(ctx, r.RideRequestID)
	if err != nil {
		return outcomeLost, fmt.Errorf("dispatch: claim reservation %s: %w", r.RideRequestID, err)
	}
	if !claimed {
		return outcomeLost, nil
	}

	leg := s.dispatcher.pickupLeg(r.RideRequestID, r.VehicleID, r.OwnerID, r.Pickup)
	if s.dispatcher.runClaimedLeg(ctx, leg) == OutcomeSent {
		s.publishDue(ctx, r, dueAt)
		s.publishDispatched(ctx, r, dueAt)
	}
	return outcomeRan, nil
}

// publishDue emits the `ride.due` seam for a reservation whose pickup push
// reached the car. Fire-and-forget and drop-safe: a nil bus or a publish
// failure is logged and the dispatch stands — the nav push is the contract,
// the event is only a notification hook.
func (s *ReservationSweeper) publishDue(ctx context.Context, r *DueReservation, dueAt time.Time) {
	s.publish(ctx, "ride.due", r.RideRequestID, events.RideDueEvent{
		RideRequestID: r.RideRequestID,
		VehicleID:     r.VehicleID,
		RiderID:       r.RiderID,
		OwnerID:       r.OwnerID,
		ScheduledFor:  r.ScheduledFor,
		DueAt:         dueAt,
	})
}

// publishDispatched emits the summary `ride.status.changed` seam so an OPEN APP
// finds out (MYR-555).
//
// THE HOLE IT FILLS. Reservation dispatch mutated `dispatch_status` and
// published `ride.due` — a topic the push notifier subscribes to and the WS
// broadcaster does not. A phone that was locked got a banner; a phone that was
// looking at the ride got NOTHING, and kept rendering a reservation that had
// already left. Prod c92b2c86 dispatched correctly at 21:40:24Z for a 21:45Z
// pickup and never surfaced. The broadcaster fans this frame to both parties,
// the client refetches, the record comes back with `dispatchedAt` set, and the
// iOS dormancy rule flips on its own — no new frame type, no new field, no
// client release required.
//
// THE STATUS IS UNCHANGED AND SAYS SO. `accepted` here is not an assumption: the
// claim's own SQL predicate is `status = 'accepted'`, so a winning claim is
// proof of the value at the instant it was stamped. PreviousStatus is the same
// value, which is what makes the event a no-op transition, and DispatchEdit is
// what makes the push notifier read it as one — without that marker this frame
// would deliver "Your ride is confirmed" a second time, hours after the accept
// and beside the `ride.due` push that IS this moment's news.
//
// THE TRIP VERSION IS THE RIDE'S CURRENT ONE (MYR-548's carry rule). A dispatch
// does not change the trip's shape, so the version is not bumped — but it must
// be CARRIED, because the frame omits it at zero and a client holding version N
// would read a zero as a regression and refuse the very refetch this frame
// exists to trigger.
//
// WHAT THE FRAME DELIBERATELY OMITS is `requesterName` and `rescheduleStatus`.
// The sweeper's projection is lean by design — it decrypts a pickup and reads
// no identity at all — and both keys are `omitempty` pointers, so they are
// ABSENT rather than null. A client reads absence as "unchanged" and keeps the
// label it has; the refetch this frame triggers is what carries the truth.
// Widening the sweep projection to populate them would put P1 identity on a
// 30-second background loop for a value the client is about to re-read anyway.
func (s *ReservationSweeper) publishDispatched(ctx context.Context, r *DueReservation, at time.Time) {
	s.publish(ctx, "ride.status.changed", r.RideRequestID, events.RideStatusChangedEvent{
		RideRequestID: r.RideRequestID,
		VehicleID:     r.VehicleID,
		RiderID:       r.RiderID,
		OwnerID:       r.OwnerID,
		Status:        rideStatusAccepted,
		// A no-op transition, stated as one: same status in and out.
		PreviousStatus: rideStatusAccepted,
		ScheduledFor:   &r.ScheduledFor,
		TripVersion:    r.TripVersion,
		DispatchEdit:   true,
		UpdatedAt:      at,
	})
}

// rideStatusAccepted is the one ride status this package names. It is the
// claim's own SQL predicate, so the sweeper never guesses it — see
// publishDispatched.
const rideStatusAccepted = "accepted"

// publish is the shared fire-and-forget publish both seams use. A nil bus is
// the ordinary unwired state, not a fault, and a publish failure is logged and
// dropped: the nav push is the contract, the events are notification hooks.
func (s *ReservationSweeper) publish(ctx context.Context, topic, rideID string, payload events.EventPayload) {
	if s.bus == nil {
		return
	}
	if err := s.bus.Publish(ctx, events.NewEvent(payload)); err != nil {
		s.logger.Warn("reservation sweep: publish failed",
			slog.String("topic", topic),
			slog.String("ride_id", rideID),
			slog.String("error", err.Error()),
		)
	}
}
