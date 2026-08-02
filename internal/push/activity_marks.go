package push

import (
	"context"
	"log/slog"
)

// The two per-Activity marks the send path persists (MYR-398).
//
// Split out of activity_notifier_send.go, which owns the fan-out itself. Both
// functions here answer the same question in two currencies — what has this
// phone ACTUALLY been shown? — and both are written after the send rather than
// before it, for reasons that point in opposite directions and are argued in
// full above each one.

// saveProgress persists the anchor an Activity was just shown, AFTER the send
// and only on a delivery Apple accepted.
//
// The order is the monotonicity promise. `Value` is the floor the next push
// clamps to, so it must record what the phone HAS SEEN, not what we hoped to
// show it: writing before the send would advance the floor past a push that
// never arrived, and the client's next value could then be lower than its
// previous one — the one thing the clamp exists to prevent.
//
// Best-effort and detached from the caller's deadline, which the send may have
// consumed.
//
// A LOST WRITE CAN REGRESS THE DELIVERED FLOOR BY ONE STEP, and the honest
// statement of the bound is worth more than the comfortable one. If APNs
// accepts a push and this UPDATE never lands — the process dies on a deploy or
// an OOM, or the statement errors and is logged and dropped — the next pass
// reads back the PREVIOUS floor. Where the clamp was load-bearing, the phone
// can then be shown a lower value than it already has: a leg that delivered
// 0.99 from the all-but-arrived branch and lost the write can follow it with
// the 0.93 the next reading recomputes. Bounded to one step and self-healing,
// because the next successful write records whatever was actually delivered.
//
// The only alternative is to write BEFORE the send, which advances the floor
// past pushes that never arrived and is strictly worse — it turns a rare
// regression after a crash into a routine one after every dropped push. The
// process-death window is not closable from this side; the database-error half
// of it is bounded by the guarded UPDATE in store.SaveActivityProgress, which
// also refuses a write that would LOWER the floor across two replicas.
func (a *ActivityNotifier) saveProgress(ctx context.Context, act Activity, anchor ProgressAnchor) {
	if sameAnchor(anchor, act.Progress) {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()

	key := ActivityKey{RideRequestID: act.RideRequestID, UserID: act.UserID}
	if err := a.store.SaveProgress(ctx, key, anchor); err != nil {
		a.logger.Warn("live activity: save progress anchor failed",
			slog.String("ride_id", act.RideRequestID),
			slog.String("error", err.Error()),
		)
	}
}

// saveAlertedPhase raises this Activity's island-expand high-water mark, AFTER
// the send and only on a delivery Apple accepted.
//
// Same ordering as saveProgress and for the same reason, with the sign of the
// failure reversed and worth stating: the mark records what the phone HAS BEEN
// SHOWN, so writing before the send would burn a phase on a push that never
// arrived and the rider would never get that expansion at all. Written after,
// the failure mode is one EXTRA expansion on a phase the next push re-evaluates
// — a lost write costs an island opening twice, not a phase change the rider
// misses. Between "a state change went unseen" and "the island opened once
// more than it had to", the second is the one to choose.
//
// IT IS REACHED ON EVERY PHASE INCLUDING THE SIXTH, which it was not before
// MYR-418. This used to bail out on an `end` event, because `completed` was the
// one phase that rode an end push and the write would have raced the tombstone
// that follows it. Now that the sixth expansion rides an alerting UPDATE sent
// BEFORE the end (see endRide), the write lands on a row that is still live and
// records honestly what the phone was shown — the guarded UPDATE in
// store.SaveActivityAlertedPhase is scoped to `ended_at IS NULL`, so ordering
// this pass ahead of the tombstone is what makes the mark reachable at all.
//
// Nothing downstream reads a mark of 6 — the rows are tombstoned a moment later
// and there is no phase seven — so its value is diagnostic: a completed ride
// whose row tombstones at 5 is now the signature of an expansion that did not
// happen, which is precisely the evidence MYR-418 was opened on.
//
// Best-effort and detached from the caller's deadline, which the send may have
// consumed.
func (a *ActivityNotifier) saveAlertedPhase(ctx context.Context, act Activity, phase AlertPhase) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()

	key := ActivityKey{RideRequestID: act.RideRequestID, UserID: act.UserID}
	if err := a.store.SaveAlertedPhase(ctx, key, phase); err != nil {
		a.logger.Warn("live activity: save alerted phase failed",
			slog.String("ride_id", act.RideRequestID),
			slog.Int("alert_phase", int(phase)),
			slog.String("error", err.Error()),
		)
	}
}
