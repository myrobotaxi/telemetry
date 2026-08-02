package push

import (
	"context"
	"log/slog"
	"time"
)

// The terminal half of the Live Activity lifecycle (MYR-172; the completion
// pair is MYR-418).
//
// Split out of activity_notifier_send.go, which owns the ordinary fan-out. This
// file owns the one moment that is not a state refresh: the push after which
// there is no successor, no ticker pass, and no row left to push to.

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
func (a *ActivityNotifier) endRide(ctx context.Context, rideRequestID, status string, linger time.Duration) {
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
	if alertPhaseFor(rc, endAt) != AlertPhaseNone {
		a.fanOut(ctx, rideRequestID, rc, activityFanOut{
			event:    ActivityEventUpdate,
			at:       endAt,
			alerting: true,
		})
		endAt = endAt.Add(endAfterAlertGap)
	}

	// Computed off the END's own instant rather than the pair's first, so the
	// linger the rider gets is measured from the push that actually ends the
	// Activity.
	dismissAt := endAt.Add(linger)
	a.fanOut(ctx, rideRequestID, rc, activityFanOut{
		event:     ActivityEventEnd,
		at:        endAt,
		dismissAt: &dismissAt,
	})
	a.tombstone(ctx, rideRequestID)
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
