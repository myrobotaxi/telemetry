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

	if ending, terminal := terminalStatuses[rc.Status]; terminal {
		a.endRide(ctx, rideRequestID, rc.Status, ending)
		return
	}

	a.fanOut(ctx, rideRequestID, rc, activityFanOut{
		event:    ActivityEventUpdate,
		at:       a.now(),
		alerting: true,
	})
}

// activityFanOut is one pass over a ride's Activities: which push shape goes
// out, what instant it claims its state was true at, and whether it is allowed
// to open the island.
//
// A struct rather than five more positional parameters, for exactly the reason
// `send` below states about ActivityNotification: the completion pair (MYR-418)
// differs from an ordinary pass in three of these fields AT ONCE, and a
// positional bool is how an `end` that alerts — or a pair of pushes that share
// one `aps.timestamp` — gets shipped by accident.
type activityFanOut struct {
	// event is `update` or `end`.
	event ActivityEvent

	// at is `aps.timestamp`: when this pass claims its state was true, and the
	// base every other instant on the push is computed from (the stale-date,
	// the expiration, the dismissal-date).
	//
	// PASSED IN RATHER THAN READ FROM THE CLOCK HERE, and that is the whole
	// reason this field exists: `aps.timestamp` is rendered in whole SECONDS,
	// so two passes that each called a.now() would stamp the identical integer
	// and the completion pair's ordering would rest on undefined behaviour. See
	// endAfterAlertGap.
	at time.Time

	// dismissAt is `aps.dismissal-date`, set only on an `end`.
	dismissAt *time.Time

	// lowPriority sends at apns-priority 5. Never set on a lifecycle pass; the
	// ticker builds its own notifications rather than coming through here.
	lowPriority bool

	// alerting lets this pass attach an `aps.alert` to the rows whose
	// high-water mark sits below the ride's phase.
	//
	// FALSE ON EVERY `end`, and that is the MYR-418 fix rather than a
	// conservative default. Apple's ActivityKit push documentation introduces
	// the alert dictionary under `start` and `update` and says of `end` only
	// that it should "include the final content state"; a `completed` end that
	// carried an alert anyway was accepted by APNs and expanded nothing on a
	// real device. The sixth expansion now rides an alerting UPDATE sent
	// immediately before the end — see endRide.
	alerting bool
}

// fanOut resolves a ride into its Activities and pushes one update to each.
//
// IT REPORTS HOW MANY ROWS ARE STILL OWED THIS PUSH, which only the held
// completion `end` acts on (MYR-421) — but the count is computed here because
// here is the only place that knows. Three outcomes are NOT owed anything
// further and must not read as failures:
//
//   - delivered — Apple took it;
//   - suppressed by preference — a muted rider is a normal, permanent state,
//     and counting it as a failure would retry that ride every 75 seconds
//     forever;
//   - permanently rejected — `send` has already deleted the row, so there is
//     nothing left to retry against.
//
// A registry lookup that failed is reported as one owed row rather than zero:
// the rows exist and were never even reached, and returning "nothing owed"
// would let a caller tombstone them on the strength of a failed read.
func (a *ActivityNotifier) fanOut(
	ctx context.Context,
	rideRequestID string,
	rc RideContext,
	spec activityFanOut,
) (owed int) {
	activities, err := a.store.ActivitiesForRide(ctx, rideRequestID)
	if err != nil {
		a.logger.Error("live activity: registry lookup failed",
			slog.String("ride_id", rideRequestID),
			slog.String("error", err.Error()),
		)
		return 1
	}
	if len(activities) == 0 {
		return 0
	}

	now := spec.at
	// One phase per ride: it is a function of the ride's state and of the age of
	// the reading that state was projected from, not of who is watching. Which
	// Activities actually ALERT on it still differs per row, because each
	// carries its own high-water mark.
	//
	// Computed even on a non-alerting pass, because it is on the log line below
	// and "the end went out at phase 6" is the sentence that would have found
	// MYR-418 in an afternoon rather than over a ride.
	phase := alertPhaseFor(rc, now)

	var delivered, alerted int
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
		var alert *ActivityAlert
		if spec.alerting {
			alert = alertFor(phase, act.AlertedPhase)
		}
		switch a.send(ctx, act, ActivityNotification{
			ActivityToken: act.Token,
			Sandbox:       act.Sandbox,
			Event:         spec.event,
			ContentState:  state,
			Timestamp:     now,
			DismissalDate: spec.dismissAt,
			LowPriority:   spec.lowPriority,
			Alert:         alert,
		}) {
		case sendDelivered:
			delivered++
			a.saveProgress(ctx, act, anchor)
			if alert != nil {
				alerted++
				a.saveAlertedPhase(ctx, act, phase)
			}
		case sendFailed:
			owed++
		case sendDropped:
			// The row is gone. Nothing to deliver and nothing to retry.
		}
	}

	a.logger.Info("live activity updated",
		slog.String("ride_id", rideRequestID),
		slog.String("event", string(spec.event)),
		slog.String("status", rc.Status),
		slog.Bool("has_eta", hasETA),
		slog.Bool("has_progress", hasProgress),
		slog.Int("activities", len(activities)),
		slog.Int("delivered", delivered),
		// The operational question this surface gets asked about the island is
		// "why did it open again?", so the count of alerting pushes is on the
		// line and the phase ordinal beside it. Both are P0 — a ladder position
		// names no place and no person.
		slog.Int("alerted", alerted),
		slog.Int("alert_phase", int(phase)),
		slog.Int("owed", owed),
	)
	return owed
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

// sendOutcome is what became of one push, and the distinction that matters is
// between the two ways of not being delivered.
//
// A bool collapsed them, which was harmless while every caller tombstoned
// unconditionally and became a stranded card the moment one caller wanted to
// retry (MYR-421): "Apple permanently rejected this token" and "Apple was
// briefly unreachable" both read as `false`, so a retry loop would either give
// up on the transient case or spin forever on the permanent one.
type sendOutcome int

const (
	// sendDelivered — Apple accepted it.
	sendDelivered sendOutcome = iota
	// sendFailed — a transient refusal (5xx, timeout, a cancelled context).
	// The row is untouched and another attempt is worth making.
	sendFailed
	// sendDropped — APNs permanently rejected the token and the row has
	// already been deleted. There is nothing left to retry against.
	sendDropped
)

// send delivers one already-assembled update and handles APNs's permanent
// rejections.
//
// The notification is built by the caller rather than from a parameter list
// here: the two send paths now differ in six fields, and a seventh positional
// bool is how a low-priority alert (a contradiction — see
// ActivityNotification.priority) gets sent by accident. `act` is still taken
// separately because the rejection paths log the ride, which the notification
// does not carry.
func (a *ActivityNotifier) send(ctx context.Context, act Activity, n ActivityNotification) sendOutcome {
	err := a.sender.SendActivity(ctx, n)
	if err == nil {
		return sendDelivered
	}

	if errors.Is(err, ErrUnregistered) {
		// The ACTIVITY is gone — dismissed by the rider, or expired — not the
		// phone. Drop this row only; the device registry is untouched.
		a.dropActivity(ctx, act.Token)
		return sendDropped
	}

	a.logger.Warn("live activity: send failed",
		slog.String("ride_id", act.RideRequestID),
		slog.String("activity_token_prefix", tokenPrefix(act.Token)),
		slog.String("error", err.Error()),
	)
	return sendFailed
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
