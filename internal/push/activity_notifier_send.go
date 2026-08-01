package push

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// The Live Activity fan-out half (MYR-172).

// pushRide sends the current state of a ride to every Activity registered
// against it, ending them when the status is terminal.
func (a *ActivityNotifier) pushRide(ctx context.Context, rideRequestID, topic string) {
	if !a.active() {
		a.logger.Debug("live activity skipped",
			slog.String("topic", topic),
			slog.String("ride_id", rideRequestID),
			slog.Bool("push_enabled", a.cfg.Enabled),
			slog.Bool("apns_configured", a.sender != nil),
		)
		return
	}

	rc, err := a.store.RideContextFor(ctx, rideRequestID)
	if err != nil {
		// A ride that has been hard-deleted (owner teardown, account deletion)
		// races every terminal send. Nothing to update and nothing to fix.
		a.logger.Warn("live activity: ride context lookup failed",
			slog.String("ride_id", rideRequestID),
			slog.String("error", err.Error()),
		)
		return
	}

	if linger, terminal := terminalStatuses[rc.Status]; terminal {
		a.endRide(ctx, rideRequestID, rc.Status, linger)
		return
	}

	a.fanOut(ctx, rideRequestID, rc, ActivityEventUpdate, nil, false)
}

// endRide delivers the final content-state with `event: "end"` and a
// dismissal-date, then tombstones the rows.
//
// Order matters: the tombstone comes AFTER the sends, because a row ended first
// would be excluded from its own final push and the Activity would be left on
// the lock screen showing the last state it happened to receive — which for a
// declined ride is "your car is on its way".
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

	dismissAt := a.now().Add(linger)
	a.fanOut(ctx, rideRequestID, rc, ActivityEventEnd, &dismissAt, false)
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

// fanOut resolves a ride into its Activities and pushes one update to each.
func (a *ActivityNotifier) fanOut(
	ctx context.Context,
	rideRequestID string,
	rc RideContext,
	event ActivityEvent,
	dismissAt *time.Time,
	lowPriority bool,
) {
	activities, err := a.store.ActivitiesForRide(ctx, rideRequestID)
	if err != nil {
		a.logger.Error("live activity: registry lookup failed",
			slog.String("ride_id", rideRequestID),
			slog.String("error", err.Error()),
		)
		return
	}
	if len(activities) == 0 {
		return
	}

	now := a.now()

	var delivered int
	var hasETA, hasProgress bool
	// Indexed rather than ranged by value: Activity carries the progress anchor
	// (MYR-398) and gocritic flags the per-iteration copy, exactly as it does on
	// the ticker's twin of this loop.
	for i := range activities {
		act := activities[i]
		// Built per Activity rather than once per ride, because the progress
		// anchor is per Activity: two phones watching one ride may have been
		// delivered different fractions, and the monotonicity promise is made
		// to each of them separately. Every other field is identical across
		// the loop; v1 registers exactly one Activity per ride anyway.
		state, anchor := contentState(rc, act.Progress, now)
		hasETA, hasProgress = state.ETA != nil, state.Progress != nil
		// MYR-349 / MYR-194 decision 7 — the recipient's own switch. A rider
		// who muted ride updates gets no Activity pushes; the Activity itself
		// still runs, started locally by the app, and falls back to its own
		// stale rendering. Checked per recipient because a ride's Activities
		// can belong to different people once the owner variant lands.
		if !a.allowed(ctx, act.UserID, rideRequestID) {
			continue
		}
		if a.send(ctx, act, state, event, dismissAt, lowPriority, now) {
			delivered++
			a.saveProgress(ctx, act, anchor)
		}
	}

	a.logger.Info("live activity updated",
		slog.String("ride_id", rideRequestID),
		slog.String("event", string(event)),
		slog.String("status", rc.Status),
		slog.Bool("has_eta", hasETA),
		slog.Bool("has_progress", hasProgress),
		slog.Int("activities", len(activities)),
		slog.Int("delivered", delivered),
	)
}

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

// allowed applies the ride_lifecycle preference gate, failing open exactly as
// the alert notifier's twin does.
func (a *ActivityNotifier) allowed(ctx context.Context, userID, rideRequestID string) bool {
	if a.prefs == nil || userID == "" {
		return true
	}

	prefs, err := a.prefs.PrefsForUser(ctx, userID)
	if err != nil {
		a.logger.Error("live activity: prefs lookup failed; sending anyway",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return true
	}
	if prefs.Allows(CategoryRideLifecycle) {
		return true
	}

	a.logger.Info("live activity suppressed by preference",
		slog.String("category", string(CategoryRideLifecycle)),
		slog.String("user_id", userID),
		slog.String("ride_id", rideRequestID),
	)
	return false
}

// send delivers one update and handles APNs's permanent rejections.
func (a *ActivityNotifier) send(
	ctx context.Context,
	act Activity,
	state ActivityContentState,
	event ActivityEvent,
	dismissAt *time.Time,
	lowPriority bool,
	now time.Time,
) bool {
	err := a.sender.SendActivity(ctx, ActivityNotification{
		ActivityToken: act.Token,
		Sandbox:       act.Sandbox,
		Event:         event,
		ContentState:  state,
		Timestamp:     now,
		DismissalDate: dismissAt,
		LowPriority:   lowPriority,
	})
	if err == nil {
		return true
	}

	if errors.Is(err, ErrUnregistered) {
		// The ACTIVITY is gone — dismissed by the rider, or expired — not the
		// phone. Drop this row only; the device registry is untouched.
		a.dropActivity(ctx, act.Token)
		return false
	}

	a.logger.Warn("live activity: send failed",
		slog.String("ride_id", act.RideRequestID),
		slog.String("activity_token_prefix", tokenPrefix(act.Token)),
		slog.String("error", err.Error()),
	)
	return false
}

// dropActivity removes a permanently rejected token.
func (a *ActivityNotifier) dropActivity(ctx context.Context, token string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()

	if err := a.store.DeleteActivityToken(ctx, token); err != nil {
		a.logger.Error("live activity: delete rejected token failed",
			slog.String("activity_token_prefix", tokenPrefix(token)),
			slog.String("error", err.Error()),
		)
		return
	}
	a.logger.Info("live activity: deleted unregistered activity",
		slog.String("activity_token_prefix", tokenPrefix(token)),
	)
}
