// START EARLY (MYR-556). The owner's `POST /api/ride-requests/{id}/dispatch-now`
// runs the reservation sweeper's own claimed dispatch path, on demand, ahead of
// the computed leave-time.
//
// IT IS THE SAME PATH, NOT A SECOND ONE. Everything that makes scheduled
// dispatch safe — the exactly-once latch, the status re-validation inside the
// claim, the nav sequencer, the retry ladder, the outcome contract, and the two
// seams that only fire on `sent` — lives in claimAndDispatch, and this file adds
// exactly two things around it: a synchronous busy answer and an outcome a
// human can be told about. A forked sequence here would be a second place for
// "your car is on the way" to become false.

package dispatch

import (
	"context"
	"log/slog"
)

// NowResult is what an on-demand dispatch attempt did, in terms the
// HTTP layer can answer with. It is deliberately COARSE: whether the nav push
// itself succeeded is recorded on the ride row (dispatchStatus), and the
// endpoint answers the refreshed record rather than re-deciding what a
// `failed` push means.
type NowResult int

const (
	// NowDispatched: the claim was won and the push pipeline ran to an
	// outcome, whatever that outcome was. The caller re-reads the ride.
	NowDispatched NowResult = iota
	// NowVehicleBusy: the car is mid-ride on an active INSTANT ride.
	// Nothing was claimed and nothing was pushed.
	NowVehicleBusy
	// NowAlreadyDispatched: the leg-1 latch was already held — the
	// sweeper reached it first, or this is a repeated tap. Nothing happened,
	// and nothing needed to.
	NowAlreadyDispatched
)

// DispatchNow sends a reservation's car NOW, at the owner's request.
//
// THE ONE GATE IT KEEPS IS THE BUSY HOLD, and it keeps it for the reason the
// sweeper does: the claim is irreversible, and re-pointing the navigation of a
// car that is carrying somebody else is the one mistake this whole package is
// arranged to avoid. The difference is what happens next — the sweeper HOLDS
// and re-decides in 30 seconds, which is a bounded retry; here there is a human
// waiting, so the answer is a refusal they can act on.
//
// THE THREE HOLD PROBES ARE DELIBERATELY NOT RE-ASKED, and the reason is one
// sentence: all three switches belong to the person making this request. The
// service flag, the ride-sharing pause (MYR-342) and the rider's ride
// capability (MYR-369) exist to defer or refuse an UNATTENDED dispatch that the
// owner is not present for; an owner standing in front of their own car and
// tapping "send it now" is that owner overriding their own switch, deliberately,
// with full knowledge. Their sweeper equivalents are HOLDS — bounded retries
// that resolve at the ceiling — which is not an answer a synchronous request can
// give at all. (The rider-grant probe is the same story from the other side: the
// owner granted that access and the owner is the one asking.)
//
// EVERYTHING ELSE THE ENDPOINT REFUSES ON — party, ownership, status,
// reservation-ness, already-dispatched — is decided by the HTTP layer against
// the row it loaded, because those are the refusals a human needs a sentence
// for. This function's job is the part that must not be duplicated.
//
// ON A NON-NIL ERROR THE RESULT IS MEANINGLESS — check the error first. The two
// errors are a busy probe that could not be answered and a claim that could not
// be taken; both mean "we do not know", and the endpoint renders them as an
// honest 500 rather than guessing a refusal.
//
// The dispatch instant comes from the sweeper's own injectable clock, so
// `ride.due`'s DueAt and the MYR-555 frame's timestamp are read exactly where
// the scheduled path reads them.
func (s *ReservationSweeper) DispatchNow(ctx context.Context, r *DueReservation) (NowResult, error) {
	busy, err := s.store.VehicleHasActiveInstantRide(ctx, r.VehicleID)
	if err != nil {
		// Unknown busy state fails CLOSED here, exactly as it holds in the
		// sweeper: we cannot tell whether pushing would hijack a live ride, and
		// a refusal the owner can retry is recoverable where a wrong push is
		// not. The caller renders it as a 500 — an honest "ask me again".
		return NowVehicleBusy, err //nolint:wrapcheck // the store seam already wraps; the caller only branches on nil-ness
	}
	if busy {
		s.logger.Info("dispatch-now: vehicle busy, refusing",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
		)
		return NowVehicleBusy, nil
	}

	outcome, err := s.claimAndDispatch(ctx, r, s.now())
	if err != nil {
		return NowAlreadyDispatched, err
	}
	if outcome == outcomeLost {
		// The sweeper won it in the seconds since the handler read the row, or
		// the ride moved on (the claim re-validates status in SQL). Either way
		// the car has its instructions and there is nothing owed.
		s.logger.Info("dispatch-now: claim already held, nothing to do",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
		)
		return NowAlreadyDispatched, nil
	}

	s.logger.Info("dispatch-now: owner dispatched a reservation early",
		slog.String("ride_id", r.RideRequestID),
		slog.String("vehicle_id", r.VehicleID),
		slog.Time("scheduled_for", r.ScheduledFor),
	)
	return NowDispatched, nil
}
