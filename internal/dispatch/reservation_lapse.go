// The LATENESS CEILING, both halves of it. Split out of reservation_worker.go
// (MYR-555) when the second half landed: until then "expire" meant only "a
// reservation that was never dispatched", and the file's whole subject was the
// per-reservation dispatch decision.

package dispatch

import (
	"context"
	"log/slog"
	"time"
)

// codeReservationExpired is recorded in dispatch_error when a reservation ran
// past its lateness ceiling (scheduledFor + MaxLateness) without turning into a
// ride. It covers BOTH ways that happens:
//
//   - never dispatched — the vehicle stayed on another ride for the whole
//     window, or dispatch itself was unavailable (downtime, kill-switch). The
//     reservation is failed WITHOUT a push (see expire).
//   - dispatched and never driven — the nav push landed, the car was told, and
//     the ride simply never happened: nobody was picked up, the status never
//     left `accepted` (see expireLapsedDispatch). Prod c92b2c86: scheduled
//     21:45Z, dispatched correctly at 21:40:24Z, still `accepted` hours later.
//
// ONE CODE FOR BOTH, deliberately, because every consumer of it asks the same
// question and needs the same answer: "is this reservation still a thing that
// might happen?" The Live Activity registration guard, the ETA ticker's
// exclusion predicate and the retention sweep all key on the pair
// (`dispatch_status = 'failed'`, `dispatch_error = 'reservation_expired'`), and
// a second code for the second cause would mean teaching all three about it or
// leaving the second cause producing exactly the lock-screen-forever bug the
// first one's tombstone exists to prevent. The distinguishing detail rides the
// log line, not the column — as it already did for expire's two causes.
//
// It is an opaque outcome code, not a credential, and joins the dispatch-local
// code set documented in rest-api.md §7.8.
const codeReservationExpired = "reservation_expired"

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

// expire resolves a reservation that is past its lateness ceiling and was NEVER
// DISPATCHED. It claims the latch and records an honest `failed` /
// reservation_expired WITHOUT pushing nav — whatever the vehicle happens to be
// doing.
//
// Claiming here is deliberate and is the point: it converts an invisible
// "forever pending" row into a resolved, alertable outcome that surfaces on
// the existing dispatchStatus surface, and it stops the sweeper from
// re-evaluating the row every 30s forever. No `ride.due` is published — the
// reservation never actually dispatched, and the seam's consumer is a
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

// LapsedReservation is one accepted reservation that WAS dispatched and then
// never became a ride. Leaner even than DueReservation: nothing here is pushed
// anywhere, so there is no pickup to decrypt and no owner token to resolve —
// the ids and the instant are the whole log line.
type LapsedReservation struct {
	RideRequestID string
	VehicleID     string
	ScheduledFor  time.Time
}

// sweepLapsed is the SECOND pass of a tick: resolve reservations that were
// dispatched and never driven.
//
// WHY THERE HAS TO BE A SECOND PASS AT ALL. The due query's third conjunct is
// `dispatched_at IS NULL` — it is what makes the sweep self-terminating, and it
// is exactly why a DISPATCHED reservation falls out of the candidate set the
// instant it is claimed. So the ceiling never ran for one. A reservation whose
// nav push succeeded and whose rider never got in the car sat `accepted`
// forever: no expiry, no `reservation_expired` tombstone, and therefore a rider
// Live Activity counting down to a pickup that already came and went, with
// nothing left in the system able to end it (prod c92b2c86, hours past
// scheduled + MaxLateness). Widening the due query instead was rejected — its
// LIMIT window is an ANTI-STARVATION device and lapsed rows would crowd out
// live dispatch candidates, and the OR would take the query off migration
// 0016's partial index, which exists precisely so this 30-second loop stays a
// few index tuples forever. A second statement over its own partial index costs
// one extra probe per tick and keeps both properties.
//
// It is SELF-DRAINING for the same reason the first pass is: the resolution
// stamps the very column the candidate predicate excludes on, so a row is
// resolved exactly once and never re-listed. A row that instead advances out of
// `accepted` — the parties proceeded manually, or somebody cancelled — leaves
// the set on its own, which is the right answer: the reservation became a ride
// after all and there is nothing to expire.
//
// Sequential rather than fanned out across the worker pool, unlike the due
// pass. Each candidate is one guarded UPDATE plus a best-effort Activity end —
// no nav push, no wake ladder, no Tesla round trip — so the concurrency the
// pool exists to bound is not present here, and spending its two slots would
// let a lapse backlog queue in front of a live dispatch.
func (s *ReservationSweeper) sweepLapsed(ctx context.Context, now time.Time) int {
	listCtx, cancel := context.WithTimeout(ctx, s.cfg.SweepTimeout)
	lapsed, err := s.store.ListLapsedDispatches(listCtx, now.Add(-s.cfg.MaxLateness), s.cfg.MaxPerSweep)
	cancel()
	if err != nil {
		s.logger.Error("reservation sweep: list lapsed dispatches failed", slog.String("error", err.Error()))
		return 0
	}

	var resolved int
	for i := range lapsed {
		if ctx.Err() != nil {
			// Shutdown. The rows stay exactly as they are and the next process
			// re-lists them — the deadline is anchored on scheduledFor, so
			// nothing is lost by stopping here.
			break
		}
		if s.expireLapsedDispatch(ctx, &lapsed[i]) {
			resolved++
		}
	}
	return resolved
}

// expireLapsedDispatch records the ceiling verdict on ONE already-dispatched
// reservation and ends its Live Activities. Reports whether this call was the
// one that resolved the row.
//
// THE LATCH IS NOT THE CLAIM, and it cannot be. `dispatched_at` is already
// stamped on these rows — that is the definition of the set — so
// ClaimReservationDispatch can never win here. The one-winner guarantee moves
// into the resolving statement itself: it refuses a row that already carries
// this verdict, so a peer sweeper, a redelivery or the very next tick all see
// no rows and do nothing. Without that, endActivities would fire every 30
// seconds for the life of the row.
//
// THE OUTCOME OVERWRITES `sent`, and that is the honest statement rather than a
// lossy one. `dispatch_status` answers "what became of this reservation's
// dispatch", and the answer is that it produced no ride; the fact that Tesla
// once accepted the share survives where it always lived for operability, in
// the `dispatch attempt` audit line and this warning beside it. Keeping `sent`
// and inventing a parallel marker would leave three consumers — the Activity
// registration guard, the ETA ticker's exclusion predicate, the retention sweep
// — reading a pair that says the reservation is still live, which is precisely
// the lock-screen-forever bug this whole path exists to end.
//
// THE VERDICT IS STILL PROVISIONAL (MYR-461). It is scoped to `accepted` in the
// statement, so the moment the parties proceed manually and the ride reaches
// `arrived`, the Activity registration is let back in and the rider's card
// works again. A reservation that lapsed is not a ride that cannot happen.
func (s *ReservationSweeper) expireLapsedDispatch(ctx context.Context, r *LapsedReservation) bool {
	expired, err := s.store.ExpireDispatchedReservation(ctx, r.RideRequestID)
	if err != nil {
		s.logger.Error("reservation sweep: expiring a lapsed dispatch failed",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
			slog.String("error", err.Error()),
		)
		return false
	}
	if !expired {
		// Somebody else resolved it, or the ride moved on between the list and
		// the write. Ordinary, and silent — the same reading a lost claim gets.
		return false
	}

	s.logger.Warn("reservation sweep: dispatched but never driven, failing reservation",
		slog.String("ride_id", r.RideRequestID),
		slog.String("vehicle_id", r.VehicleID),
		slog.Time("scheduled_for", r.ScheduledFor),
		slog.Duration("max_lateness", s.cfg.MaxLateness),
	)
	// The same courtesy the never-dispatched arm pays, and needed more here:
	// this rider's card has been counting down since the car was dialled.
	s.endActivities(ctx, r.RideRequestID)
	return true
}
