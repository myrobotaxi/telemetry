package push

import (
	"context"
	"log/slog"
)

// The held completion `end` (MYR-421).
//
// THE MEASUREMENT THIS FILE EXISTS FOR. An `.ended` Live Activity leaves the
// DYNAMIC ISLAND about 1.4 seconds after the end lands — the lock-screen card
// stays for its dismissal-date, but the island presence does not. MYR-418's
// completion pair sends the alerting update and then the end one second later,
// so the sixth expansion's check was on the island for roughly two seconds in
// total and the client could not find it at all.
//
// So the two halves are pulled apart. Completion sends ONLY the alerting update
// (activity_terminal.go), the rows stay live, and the `end` follows five minutes
// later from here. The island keeps the check for the whole hold; the card keeps
// its own five minutes past the end, measured from the end's own instant exactly
// as it always was.
//
// IT IS A SWEEP, NOT A TIMER, and that is the crash-safety argument in one
// word. An in-process `time.AfterFunc` would be lost by every deploy and every
// restart, and the ride it was holding is terminal — no transition will ever
// fire for it again, and the ETA ticker does not list completed rides — so a
// lost timer is a card stranded until ActivityKit's own multi-hour ceiling. A
// pass over "completed at least five minutes ago and not yet ended" has no such
// state: it is recomputed from `go_ride_requests.completed_at`, which the status
// write itself stamped, on every pass of every process. The worst a restart
// costs is one pass of latency, and the ticker's startup pass fires ~20s in.
//
// THE BOUND, stated rather than implied: a card is ended at the first pass at or
// after completion + DismissAfter, so its island presence lasts between the hold
// and the hold plus one tick interval — five minutes to about six and a half.
// Nothing can stretch it further, because the predicate is a durable column
// rather than anything this loop remembers.
//
// THE TOMBSTONE IS THE RECEIPT FOR A DELIVERED END, and that rule lives in
// endRide (activityEnding.retryable). A pass that failed to reach Apple leaves
// the rows live and the next pass re-lists them; the `end` is idempotent, since
// a second one at an Activity the phone has already ended is answered 410 and
// deletes the row. Without it the hold was single-shot and one 30-second APNs
// blip overlapping a pass would strand every card in that pass at once.
//
// TWO REPLICAS CAN BOTH SEND ONE RIDE'S END, and that is ACCEPTED rather than
// prevented. This service runs a single replica today; if it did not, both
// would list the same due ride, both would push, and the loser's tombstone
// would move zero rows. The cost is one duplicate `end` at a card that is
// already ending — invisible to the rider, and answered 410 at worst. The
// obvious guard is a claim-then-send latch (`UPDATE … WHERE ended_at IS NULL
// RETURNING`, the shape ClaimReservationDispatch already uses) and it is the
// WRONG trade here for exactly the reason above: claiming before the send makes
// the tombstone a receipt for having tried, which is the stranding bug this
// file was corrected for. Between one duplicate push and a card nobody ends,
// this surface chooses the first — the same choice §7.21.4 makes about the
// island opening once more than it had to.

// endHeldCompletions sends the `end` push for every completed ride whose hold
// has expired.
//
// THE HORIZON IS DismissAfter — one number, shared with the dismissal-date it
// is derived alongside. The hold is "how long the rider gets to look at the
// arrival state on the island" and the dismissal-date is "how long they get to
// look at it on the lock screen"; they are the same five minutes the client
// counts down locally (MYR-405), and a second constant is how one of them
// silently drifts from the other. It is deliberately NOT a TickerConfig field:
// a configurable hold is an operator's chance to disagree with the client about
// a number the client also holds.
//
// Bounded by MaxPerPass like the refresh above it, and for a stronger reason:
// after a long outage this list is every ride that completed while the process
// was down. Oldest completion first, so the cap sheds the least overdue.
func (t *ActivityTicker) endHeldCompletions(ctx context.Context) {
	listCtx, cancel := context.WithTimeout(ctx, t.cfg.ListTimeout)
	rides, err := t.legs.RidesAwaitingEnd(listCtx, DismissAfter, t.cfg.MaxPerPass)
	cancel()
	if err != nil {
		t.logger.Error("live activity ticker: held-end list failed",
			slog.String("error", err.Error()))
		return
	}
	if len(rides) == 0 {
		return
	}

	var sent int
	for _, rideID := range rides {
		// STOP AT THE FIRST SIGN OF SHUTDOWN, before the send rather than
		// inside it. A cancelled context makes every remaining push fail — and
		// the tombstone deliberately runs on a context.WithoutCancel, so a loop
		// that ploughed on through SIGTERM would tombstone the rest of the
		// batch on the strength of pushes that never left. With the retry rule
		// in endRide this would already leave the rows live, but the pass would
		// still spend a doomed APNs round-trip per ride while the process is
		// trying to exit. The survivors are picked up by the next process's
		// startup pass.
		if err := ctx.Err(); err != nil {
			t.logger.Info("live activity ticker: held-end pass cut short",
				slog.Int("sent", sent),
				slog.Int("remaining", len(rides)-sent),
				slog.String("reason", err.Error()))
			return
		}
		// Serially, and on the pass context. Each ride is one fan-out over the
		// (usually one) Activity registered against it, and completions arrive
		// at human rates — a queue deep enough to need concurrency here is one
		// the per-pass cap has already truncated.
		t.notifier.endHeldCompletion(ctx, rideID)
		sent++
	}

	t.logger.Info("live activity ticker: held ends sent",
		slog.Int("rides", sent),
		slog.Duration("held", DismissAfter),
		slog.Bool("capped", len(rides) == t.cfg.MaxPerPass))
}
