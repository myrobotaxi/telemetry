// The gates that decide whether a synced-but-silent car earns its one forced
// config re-push (MYR-489), and the awake proof behind the last of them.
//
// Split from fleet_config_force_repush.go for the CLAUDE.md 300-line file cap;
// the escalation and this file are one component.

package telemetry

import (
	"context"
	"log/slog"
)

// escalationBlocker answers whether this candidate is eligible for its one
// forced re-push, and names the blocker when it is not.
//
// Three gates, each closing a distinct failure mode:
//
//  1. REPEAT OBSERVATION. The previous attempt must ALSO have ended in
//     synced-not-streaming. A car that has just been configured is legitimately
//     a few minutes from its first connection (James Guan's did exactly that),
//     and forcing a re-push at it would tear down a config it is in the middle
//     of acting on.
//  2. QUIET GRACE. At least SyncedQuietGrace must have passed since that
//     observation. Redundant with the backoff at default settings and
//     deliberately so — it is what keeps the gate honest if Interval is ever
//     tuned down.
//  3. EPOCH BUDGET. One force per pairing epoch — `epochForceSpent`, shared
//     verbatim with the MYR-505 on-demand path (fleet_config_force_now.go) so
//     the two entry points cannot ration the same car differently.
//
// INSIDE A HOT PAIRING EPOCH GATE 2 GAINS A SECOND WAY TO BE SATISFIED, and
// loses nothing (MYR-529). Gate 2 asks "has this car had long enough to connect
// on its own?", and it could only answer in units of the steady-state backoff —
// fifteen minutes — which is why Joey Verrone's car, paired and silent, was
// still waiting when its owner gave up on it. While the epoch is fresh the same
// question can also be answered in the hot ladder's units. Gates 1 and 3 are
// untouched, so the force still requires a REPEAT observation and still costs
// exactly one spend per epoch; the net effect is that it lands at about three
// minutes past pairing instead of never.
func (r *FleetConfigReconciler) escalationBlocker(c FleetConfigCandidate) (string, bool) {
	if c.LastOutcome != outcomeSyncedQuiet {
		return "first synced-but-quiet observation; giving the car a cycle to connect", false
	}
	if reason, ok := r.quietGraceElapsed(c); !ok {
		return reason, false
	}
	if epochForceSpent(c) {
		return "forced re-push already spent for this key-pairing epoch", false
	}
	return "", true
}

// quietGraceElapsed is gate 2: has this car had long enough to connect on its
// own?
//
// TWO WAYS TO SATISFY IT, AND THE HOT ONE ONLY EVER ADDS (MYR-529). The
// original answer is a wall-clock one — SyncedQuietGrace since the observation
// — and it stays exactly as it was, which is what keeps Nabil's car (synced,
// silent for two days, last examined eight hours ago) escalating the instant a
// signed command proves its key. What it cannot answer is a car examined
// SECONDS ago: opening a pairing epoch triggers an immediate examination, and
// that examination stamps `last_attempt_at`, so a car one minute past pairing
// always looks like it has just been looked at — which is precisely how Joey
// Verrone's row spent fifteen minutes doing nothing while he drove.
//
// So inside a hot epoch there is a SECOND way to satisfy the same question:
// hotForceMinAttempts examinations already recorded in this epoch. With the
// ladder that is the epoch-instant pass plus the one at +1 minute, so the gate
// opens on the pass at +3. It is an OR, never a replacement — a hot car whose
// wall-clock grace has already elapsed is not made to wait for the ladder.
func (r *FleetConfigReconciler) quietGraceElapsed(c FleetConfigCandidate) (string, bool) {
	if c.LastAttemptAt.IsZero() || r.now().Sub(c.LastAttemptAt) >= r.cfg.SyncedQuietGrace {
		return "", true
	}
	if r.inHotEpoch(c) && c.AttemptCount >= hotForceMinAttempts {
		return "", true
	}
	if r.inHotEpoch(c) {
		return "hot pairing epoch: the car has not yet had its early passes to connect on its own", false
	}
	return "synced-quiet grace period has not elapsed", false
}

// provenAwake establishes that this is a live car worth spending an escalation
// on, rather than one that is off the network.
//
// Cheap proof first: a signed command applied within AwakeWindow is already on
// the row and costs nothing. Only when that is absent or stale do we pay for a
// GetVehicle read — which means the extra Fleet API call happens at most once
// per car per epoch, since a declined escalation reschedules and a spent one is
// blocked by the budget.
//
// Tesla's `asleep` is NOT treated as awake. A sleeping car cannot act on a new
// config either, so forcing one at it burns the epoch budget on a no-op and
// leaves the car briefly unconfigured for nothing; it will be re-examined when
// the backoff elapses.
func (r *FleetConfigReconciler) provenAwake(
	ctx context.Context, c FleetConfigCandidate, accessToken, vin string,
) bool {
	if !c.SignedCommandAt.IsZero() && r.now().Sub(c.SignedCommandAt) <= r.cfg.AwakeWindow {
		return true
	}

	state, err := r.deps.Reader.GetVehicle(ctx, accessToken, c.VIN)
	if err != nil {
		// Not a reason to escalate blind. The read is the cheap call; if it is
		// failing, a delete-then-create is a bad bet.
		r.logger.Info("fleet-config reconcile: could not confirm the vehicle is awake; not escalating",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", vin),
			slog.String("error", redactedErrorText(err)))
		return false
	}
	if state == nil || state.State != "online" {
		r.logger.Info("fleet-config reconcile: not escalating an offline/asleep vehicle",
			slog.String("event", "fleet_config_escalation_declined"),
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", vin),
			slog.String("reason", "vehicle is not online"))
		return false
	}
	return true
}
