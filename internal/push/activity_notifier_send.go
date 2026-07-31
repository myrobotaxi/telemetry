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
	state := contentState(rc, now)

	var delivered int
	for _, act := range activities {
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
		}
	}

	a.logger.Info("live activity updated",
		slog.String("ride_id", rideRequestID),
		slog.String("event", string(event)),
		slog.String("status", rc.Status),
		slog.Bool("has_eta", state.ETA != nil),
		slog.Int("activities", len(activities)),
		slog.Int("delivered", delivered),
	)
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

// contentState projects a ride into what the Activity renders.
//
// The ETA conversion is the one piece of arithmetic on this path: the car
// reports a DURATION in whole minutes (Tesla's minutesToArrival, persisted
// verbatim) and the Activity needs an INSTANT, because an instant survives the
// gap between pushes and a duration does not. A negative or absent value means
// no route, and the key is omitted rather than guessed.
func contentState(rc RideContext, now time.Time) ActivityContentState {
	state := ActivityContentState{
		Version:     ActivityContentStateVersion,
		Status:      rc.Status,
		VehicleName: rc.VehicleName,
		Destination: rc.Destination,
	}
	if rc.ETAMinutes != nil && *rc.ETAMinutes >= 0 {
		eta := now.Add(time.Duration(*rc.ETAMinutes) * time.Minute).Unix()
		state.ETA = &eta
	}
	return state
}
