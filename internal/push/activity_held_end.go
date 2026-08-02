package push

import (
	"context"
	"log/slog"
	"time"
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
// THE RETRY TERMINATES, which is the other half of that rule and does not
// follow from it. Only 410 and 400 take a row out of the due list; a 403
// TopicDisallowed, a 404 or a timeout all come back retryable, so a retry with
// no ceiling would re-attempt one poisoned row every 75 seconds until the
// 24-hour registration sweep deleted it — and even the ordering would work
// against us, since a non-draining row sits at the head of a LIMITed list and
// blocks newly completed rides behind it. Three bounds together make the loop
// provably terminate and provably stay fair:
//
//   - heldEndGiveUpAfter — a horizon past which the card is abandoned in bulk,
//     so the due set drains whatever APNs is doing;
//   - newest-completion-first ordering — so a stuck row cannot hold the head of
//     the queue against a ride that has just finished;
//   - heldEndPassFraction and heldEndSendTimeout — so a saturated batch can
//     never starve the ETA refresh of more than one tick.
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

// heldEndGiveUpAfter is how long after COMPLETION the retry stops and the card
// is abandoned — same origin as the hold, so the retry budget is the difference
// between the two: five minutes to thirty, about twenty passes.
//
// A HORIZON IS REQUIRED, not a refinement. Only 410 and 400 take a row out of
// the due list; every other failure — a 403 TopicDisallowed from a misconfigured
// APNS_TOPIC, a 404, a timeout — comes back as retryable. Without a ceiling the
// only terminator is the 24-hour registration sweep, which is ~1,150 doomed
// pushes per row and then a DELETE with no `end` ever sent. That is worse than
// giving up, because it is giving up eventually while pretending not to.
//
// THIRTY MINUTES, and the number is chosen from both directions. Long enough
// that nothing recoverable is abandoned: it is six times the hold, and
// comfortably outlasts a rolling deploy, a database failover, or any APNs
// incident this service has seen — and note that reachability of the PHONE is
// not what is being waited on, since an `end` Apple accepts is stored for 24
// hours by its own apns-expiration. What this window is for is our ability to
// hand the push to Apple at all. Short enough that a poisoned head of the list
// drains within the half hour rather than the day, and short enough that the
// value being given up on has genuinely decayed: the client's own five-minute
// reaper (MYR-405) has removed the card on any phone whose app is alive, so
// what remains after thirty minutes is a dead-app phone showing a ride that
// finished half an hour ago.
const heldEndGiveUpAfter = 30 * time.Minute

// heldEndSendTimeout bounds ONE ride's end push.
//
// The APNs client allows 10 seconds per attempt and retries once on a network
// error or 5xx, so an unbounded ride can hold this loop for ~20 seconds by
// itself. Fifteen gives the first attempt a clean run and deliberately cuts the
// INLINE retry short, because the inline retry is the expensive one: the pass is
// already a retry, and a pass retry costs the loop nothing while an inline one
// holds every ride behind it. The cost of cutting it is 75 seconds of latency on
// a ride that had 25 minutes of budget.
//
// It cannot truncate a write. Both mark writes and the tombstone run on
// contexts detached from this one (see saveProgress and tombstone), which is
// what makes a deadline here safe rather than merely bounded.
const heldEndSendTimeout = 15 * time.Second

// heldEndPassFraction is the share of one tick interval the held-end pass may
// spend before it yields.
//
// THE PASS MUST NOT STARVE THE ETA REFRESH. RunPass runs the refresh and then
// the held ends serially on one goroutine, so a saturated batch does not delay
// the refresh it already ran — it delays every subsequent one, by delaying the
// loop's return to its timer. At MaxPerPass=200 with each send bounded at
// heldEndSendTimeout, an unbounded batch is fifty minutes during which no live
// ride's ETA is refreshed at all.
//
// A half of the interval, checked BEFORE each ride, gives the bound worth
// stating: a pass can overshoot by at most one heldEndSendTimeout, so the
// held-end work costs at most Interval/2 + 15s ≈ 52s at the default cadence —
// under the 60-second floor of the jittered interval, and therefore never more
// than one tick's delay to the refresh. A healthy full batch fits comfortably
// inside it: 200 sends at a normal APNs round-trip is well under 37 seconds.
const heldEndPassFraction = 0.5

// abandonExpiredHeldEnds retires the cards whose horizon has passed.
//
// FIRST IN THE PASS, so the due list this pass reads is already free of rows
// nothing will be done with — otherwise they would occupy slots in a LIMITed
// query that the newest-first ordering puts them last in, and never leave.
//
// Logged at ERROR because an abandoned card is a real failure with a rider on
// the other end of it: a completed ride whose Activity this service never
// managed to end. It is bounded and honest rather than silent, which is the
// most that can be said for it.
func (t *ActivityTicker) abandonExpiredHeldEnds(ctx context.Context) {
	abandonCtx, cancel := context.WithTimeout(ctx, t.cfg.ListTimeout)
	defer cancel()

	retired, err := t.legs.AbandonHeldEnds(abandonCtx, heldEndGiveUpAfter)
	if err != nil {
		t.logger.Error("live activity ticker: abandoning expired held ends failed",
			slog.String("error", err.Error()))
		return
	}
	if retired > 0 {
		t.logger.Error("live activity: gave up on held ends; cards were never ended",
			slog.Int64("activities", retired),
			slog.Duration("after", heldEndGiveUpAfter))
	}
}

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
	t.abandonExpiredHeldEnds(ctx)

	listCtx, cancel := context.WithTimeout(ctx, t.cfg.ListTimeout)
	rides, err := t.legs.RidesAwaitingEnd(listCtx, DismissAfter, heldEndGiveUpAfter, t.cfg.MaxPerPass)
	cancel()
	if err != nil {
		t.logger.Error("live activity ticker: held-end list failed",
			slog.String("error", err.Error()))
		return
	}
	if len(rides) == 0 {
		return
	}

	// Read from the notifier's injectable clock, like every other instant on
	// this surface, so a test can drive the budget deterministically rather
	// than by sleeping.
	yieldAt := t.notifier.now().Add(time.Duration(float64(t.cfg.Interval) * heldEndPassFraction))

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
		// YIELD BEFORE STARTING ANOTHER RIDE, never in the middle of one. The
		// check is here rather than after the send so the overshoot is bounded
		// by exactly one heldEndSendTimeout — see heldEndPassFraction for the
		// arithmetic that keeps the whole pass inside one tick.
		if !t.notifier.now().Before(yieldAt) {
			t.logger.Warn("live activity ticker: held-end pass yielded on its time budget",
				slog.Int("sent", sent),
				slog.Int("deferred", len(rides)-sent))
			break
		}
		// Serially, and on the pass context. Each ride is one fan-out over the
		// (usually one) Activity registered against it, and completions arrive
		// at human rates — a queue deep enough to need concurrency here is one
		// the per-pass cap has already truncated.
		//
		// Under its own deadline, so one unreachable ride cannot own a quarter
		// of the tick. The mark writes and the tombstone behind this call run on
		// detached contexts, so the deadline bounds the PUSH and nothing else.
		rideCtx, cancelRide := context.WithTimeout(ctx, heldEndSendTimeout)
		t.notifier.endHeldCompletion(rideCtx, rideID)
		cancelRide()
		sent++
	}

	t.logger.Info("live activity ticker: held ends sent",
		slog.Int("rides", sent),
		slog.Duration("held", DismissAfter),
		slog.Bool("capped", len(rides) == t.cfg.MaxPerPass))
}
