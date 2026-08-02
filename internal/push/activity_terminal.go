package push

import (
	"context"
	"log/slog"
	"time"
)

// The terminal half of the Live Activity lifecycle (MYR-172; the completion
// pair is MYR-418, and the delay between its halves is MYR-421).
//
// Split out of activity_notifier_send.go, which owns the ordinary fan-out. This
// file owns the one moment that is not a state refresh: the push after which
// there is no successor, no ticker pass, and no row left to push to.

// activityEnding is how one terminal transition leaves a ride's Activities:
// which halves of the completion pair this call sends, and how long the card
// lingers after the `end`.
//
// A struct rather than three positional parameters for the reason activityFanOut
// states next door: the difference between an ending that announces and holds,
// one that announces and ends, and one that only ends is three booleans' worth
// of meaning, and a positional bool is how the wrong one ships.
type activityEnding struct {
	// announce sends the alerting UPDATE carrying the final content-state
	// BEFORE the end — the first half of MYR-418's pair. It is a permission,
	// not an instruction: whether an update is actually alerting is still
	// decided by alertPhaseFor and by each row's own high-water mark, so the
	// unhappy endings set it and get nothing.
	//
	// False on the DELAYED end alone, where the announcement already went out
	// five minutes earlier and re-sending the same content-state as a second,
	// alert-free update would be a wasted push whose `aps.timestamp` collides
	// with the end's.
	announce bool

	// hold stops after the announcement: no `end`, no tombstone, the rows left
	// live for the ticker's held-end pass to finish (MYR-421).
	hold bool

	// linger is the dismissal-date's offset past the `end`'s own instant.
	linger time.Duration
}

// completedEnding is a completed ride's FIRST half: the alerting update, and
// then nothing until the hold expires.
//
// linger is carried even though this call never sends an `end`, because the two
// halves must agree on it and DismissAfter is the single number they agree on —
// it is both the hold horizon (how long the island keeps the check) and the
// dismissal offset (how long the lock-screen card outlives the end). See
// heldCompletionEnding.
var completedEnding = activityEnding{announce: true, hold: true, linger: DismissAfter}

// heldCompletionEnding is a completed ride's SECOND half, sent by the ticker
// once the hold has expired: the `end` alone.
var heldCompletionEnding = activityEnding{linger: DismissAfter}

// promptEnding is every unhappy ending — declined, cancelled, and a reservation
// that lapsed. UNCHANGED by MYR-421: one push, sent at once, dismissed in thirty
// seconds. Holding these would be the opposite of what they are for; a card that
// keeps saying "on its way" after a cancellation is the whole reason this
// surface pushes on every transition, and the news is the ending itself rather
// than a state worth lingering over.
var promptEnding = activityEnding{announce: true, linger: DismissPromptly}

// endAfterAlertGap is how far the `end` push's `aps.timestamp` is stamped ahead
// of the alerting update that precedes it on a completed ride.
//
// A SECOND, BECAUSE THE FIELD HAS NO FINER RESOLUTION. `aps.timestamp` is
// rendered in whole unix seconds, so the two pushes of the completion pair —
// built microseconds apart from one clock read — would otherwise carry the
// IDENTICAL integer. ActivityKit's documented rule is that it discards an
// update older than the one it is already showing; what it does with an equal
// stamp is not written down anywhere, and the push it would be discarding is
// the `end`. That is the one push in this system that must never be dropped:
// the rows are tombstoned as it goes out, nothing retries, and a discarded end
// leaves the rider's card frozen on the arrival state until ActivityKit's own
// multi-hour ceiling reaps it.
//
// So the END takes the later stamp, not the update. Backdating the update
// instead would put the ordering risk on the alert — and an alert whose
// timestamp collides with a tick the ETA ticker sent a second earlier is
// exactly the silently-swallowed expansion this issue is about. A second of
// forward-dating costs nothing measurable: the stale-date and the
// dismissal-date shift with it, and both are minutes wide.
const endAfterAlertGap = time.Second

// endRide delivers a ride's final content-state and tombstones the rows.
//
// THE COMPLETED RIDE GOES OUT AS TWO PUSHES, and the split is MYR-418's whole
// substance. The design's sixth island expansion belongs to `completed`, which
// is also the transition that ends the Activity — so the expansion used to be
// asked for by hanging an `aps.alert` on the `end` push itself. APNs accepted
// every one of them and no island ever opened: Apple's ActivityKit push
// documentation introduces the alert dictionary under `start` and `update`, and
// says of an `end` only that it should "include the final content state". An
// alert on an `end` is not documented, not rejected, and — on the client's
// real-device ride — not honoured.
//
// So the alert is moved to the shape Apple documents it on. A terminal status
// that sits on the ladder gets an alerting UPDATE carrying the final
// content-state first (status `completed`, a full track, the sixth banner),
// which is an ordinary alerting update in every respect: priority 10, the
// 24-hour expiration floor, and the high-water mark raised on delivery, which
// an end push could never do because the tombstone was already racing it. The
// `end` follows a second later carrying THE SAME final content-state — Apple
// asks for it, and it is what the card is left rendering — its dismissal-date,
// and no alert.
//
// THE UNHAPPY ENDINGS ARE UNCHANGED AND STILL A SINGLE PUSH. `declined`,
// `cancelled` and `reservation_expired` are outside the design's six phases, so
// alertPhaseFor puts them at AlertPhaseNone and the pre-end pass is skipped
// entirely. Their `end` still carries their own final content-state, because a
// lock screen that keeps saying "on its way" after a cancellation is the whole
// reason this surface pushes on every transition.
//
// THE TOMBSTONE COMES LAST, AFTER BOTH SENDS. A row ended first would be
// excluded from its own final push and the Activity would be left on the lock
// screen showing the last state it happened to receive — which for a declined
// ride is "your car is on its way".
//
// AND ON A COMPLETED RIDE THE PAIR IS NOW FIVE MINUTES APART (MYR-421). The
// second push does not merely end the Activity, it REMOVES IT FROM THE DYNAMIC
// ISLAND about 1.4 seconds after it lands — so a pair one second apart showed
// the sixth expansion's check for roughly two seconds and then took the island
// presence away with it. Completion therefore sends only the announcement here
// and returns with the rows still live; the ticker's held-end pass sends the
// `end` once the hold expires, and tombstones then. Everything that reads
// `ended_at IS NULL` sees a live row for those five minutes — audited in
// §7.21.4, and the short version is that the ticker's active-leg list excludes
// `completed` and the MYR-413 banner gate has no completed banner to suppress.
func (a *ActivityNotifier) endRide(ctx context.Context, rideRequestID, status string, ending activityEnding) {
	if !a.active() {
		return
	}

	rc, err := a.store.RideContextFor(ctx, rideRequestID)
	if err != nil {
		a.logger.Warn("live activity: ride context lookup failed on end",
			slog.String("ride_id", rideRequestID),
			slog.String("error", err.Error()),
		)
		// Still tombstone: the ride is over whatever the read said, and leaving
		// live rows behind would keep the ETA ticker pushing at a finished ride.
		a.tombstone(ctx, rideRequestID)
		return
	}
	// The caller's status wins. A reservation expiry ends an Activity while the
	// ride row still reads `accepted`, and the lock screen must show the ending,
	// not the stale status the row still carries.
	rc.Status = status

	endAt := a.now()
	// Read from the same ladder every other push reads, rather than testing for
	// `completed` by name: the question being asked is "is this ending one of
	// the design's six phases?", and alertPhaseFor is the one place that is
	// answered. A seventh phase, or a terminal status promoted onto the ladder,
	// is then covered by construction. Which Activities actually alert is still
	// decided per row inside the fan-out, against each one's own mark.
	if ending.announce && alertPhaseFor(rc, endAt) != AlertPhaseNone {
		a.fanOut(ctx, rideRequestID, rc, activityFanOut{
			event:    ActivityEventUpdate,
			at:       endAt,
			alerting: true,
		})
		if ending.hold {
			// THE ROWS STAY LIVE ON PURPOSE. Tombstoning here would exclude
			// them from their own `end` five minutes from now, which is the
			// same defect the send-then-tombstone ordering below exists to
			// prevent — relocated in time rather than avoided.
			a.logger.Info("live activity end held",
				slog.String("ride_id", rideRequestID),
				slog.String("status", status),
				slog.Duration("hold", ending.linger),
			)
			return
		}
		endAt = endAt.Add(endAfterAlertGap)
	}

	// Computed off the END's own instant rather than the pair's first, so the
	// linger the rider gets is measured from the push that actually ends the
	// Activity. On a held completion that instant is five minutes past the
	// announcement, which is the point: the card the rider is left with outlives
	// the end by the same five minutes it has always been given.
	dismissAt := endAt.Add(ending.linger)
	a.fanOut(ctx, rideRequestID, rc, activityFanOut{
		event:     ActivityEventEnd,
		at:        endAt,
		dismissAt: &dismissAt,
	})
	a.tombstone(ctx, rideRequestID)
}

// endHeldCompletion sends the `end` a completed ride's announcement held back,
// and tombstones the rows (MYR-421).
//
// The status is forced to `completed` rather than read from the row for the
// same reason endRide takes one at all: this is called from a list that already
// filtered on it, and a ride that was hard-deleted between the list and the read
// must still tombstone rather than push a zero-valued state. `completed` is
// terminal and latched, so there is no transition that could have moved it under
// us.
//
// No announcement: the alerting update went out at completion and raised the
// mark to 6, so a second one would attach no alert, expand nothing, and collide
// with this push's own `aps.timestamp` in the bargain.
func (a *ActivityNotifier) endHeldCompletion(ctx context.Context, rideRequestID string) {
	a.endRide(ctx, rideRequestID, statusCompleted, heldCompletionEnding)
}

// tombstone marks a ride's Activities ended so no later tick reaches them.
func (a *ActivityNotifier) tombstone(ctx context.Context, rideRequestID string) {
	// Detached from the caller's deadline: the sends above may have consumed
	// most of it, and a missed tombstone is the one failure here that keeps
	// costing after the request is over.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()

	ended, err := a.store.EndActivitiesForRide(ctx, rideRequestID)
	if err != nil {
		a.logger.Error("live activity: end tombstone failed",
			slog.String("ride_id", rideRequestID),
			slog.String("error", err.Error()),
		)
		return
	}
	if ended > 0 {
		a.logger.Info("live activities ended",
			slog.String("ride_id", rideRequestID),
			slog.Int64("count", ended),
		)
	}
}
