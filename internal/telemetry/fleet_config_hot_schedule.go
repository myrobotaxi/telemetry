// The HOT SCHEDULE for a fresh key-pairing epoch (MYR-529).
//
// THE CLIENT DIRECTIVE, verbatim: "You should run the reconciler once a car
// virtual key is paired why do people have to wait."
//
// WHAT WAS ACTUALLY WRONG, from Joey Verrone's row rather than from theory.
// His car linked at 01:22:08Z (MYR-517's seed wrote the schedule row), the
// virtual key was proven paired at 01:23:10.59Z (`signed_command_at`), and
// MYR-489's in-band signal did exactly what it was built to do 337 ms later: it
// reset the backoff and reconciled the car ON THE SPOT. Tesla answered `synced`.
// And then the three gates that ration the forced re-push declined it — for the
// best of reasons, since that was the FIRST synced-but-quiet observation and a
// car one second past pairing deserves a cycle to connect on its own — and
// parked the row at `next_attempt_at` +15 MINUTES with `forced_repush_at` NULL.
// Joey drove with no telemetry. Nothing was broken; the cadence was simply
// written for a fleet in its steady state and applied to a car in the one
// minute of its life when a human was watching.
//
// So the fix is not a new trigger — MYR-489/505/517 already put a trigger on
// every door. It is that the SCHEDULE a trigger writes must be measured in
// minutes while the pairing evidence is fresh:
//
//	epoch  →  +1 min  →  +3 min  →  +8 min  →  ordinary 15m/30m/1h backoff
//
// and the forced delete-and-recreate — the maneuver that actually brings a
// stuck car up — must be spent INSIDE that window rather than after it.
//
// THREE PROPERTIES KEEP THIS FROM BECOMING A TESLA-TRAFFIC MULTIPLIER.
//
//  1. The ladder is bounded and per-epoch. Three extra examinations, then the
//     MYR-448 curve resumes exactly as before. An epoch is opened only by proof
//     of pairing and only once per debounce window.
//  2. The cheapest check runs first. A car that came up on its own is detected
//     from the LISTING ROW — `status` plus `lastUpdated`, both already selected
//     — and costs ZERO Tesla calls. That is what makes it safe to look at a
//     freshly paired car every minute: the healthy majority are free.
//  3. The escalation budget is untouched. Still one forced re-push per pairing
//     epoch, still `epochForceSpent`, still shared verbatim with §7.23. The hot
//     window changes WHEN it is spent, never HOW MANY times.

package telemetry

import (
	"context"
	"log/slog"
	"time"
)

// hotPairingLadder is the gap AFTER each attempt made inside a fresh pairing
// epoch, indexed by the attempt count the row carried BEFORE that attempt.
//
// Read cumulatively from the epoch it produces examinations at roughly +1, +3
// and +8 minutes. The shape is deliberate rather than geometric: the first gap
// is short because a car that is going to connect by itself has usually done so
// within a minute (James Guan's did), the second is where the escalation lands,
// and the third is a last look before the ordinary cadence takes over.
var hotPairingLadder = [...]time.Duration{
	1 * time.Minute,
	2 * time.Minute,
	5 * time.Minute,
}

// hotForceMinAttempts is how many attempts must already have been recorded in
// this epoch before the forced re-push may be spent.
//
// Two, which places the force on the SECOND hot pass — about three minutes
// after pairing. One would put it at the epoch instant itself, tearing down a
// config the car may be in the middle of acting on; the ordinary path's answer
// to that is `escalationBlocker`'s repeat-observation gate, and this is the same
// caution expressed in the hot window's own units.
const hotForceMinAttempts = 2

// defaultFleetConfigHotEpochWindow is how long a pairing epoch stays HOT.
//
// It bounds two things at once: the ladder above, and the candidate query's
// exemption from the 30-minute staleness rule. Fifteen minutes comfortably
// covers the ~8-minute ladder plus clock skew between this process and
// Postgres, and expires long before a car could accumulate a meaningful backoff
// under it.
const defaultFleetConfigHotEpochWindow = 15 * time.Minute

// defaultFleetConfigTickInterval is how often the loop LOOKS for due rows.
//
// It is deliberately NOT the same number as Interval, and separating them is
// the third part of the MYR-529 fix. Interval is the BACKOFF BASE — the gap a
// single unlucky car waits between attempts — and 15 minutes is right for that.
// It was also, accidentally, the loop's tick, which meant a row whose
// `next_attempt_at` had just passed could sit unexamined for another full
// interval: Joey's 01:38 row was still untouched at 01:40+, and the hot ladder
// below would have been meaningless against it. A due row is now picked up
// within a minute. The pass itself stays cheap because the DUE predicate lives
// in SQL — a tick with nothing due is one indexed query and no Tesla traffic.
const defaultFleetConfigTickInterval = 1 * time.Minute

// cancelIfStreaming is THE CHEAPEST CHECK, AND IT RUNS FIRST — before the token
// resolve, before any Fleet API call. It reports whether the candidate was
// resolved here and needs no further work.
//
// A car that came up on its own is already visible in the row the listing
// returned: a `status` that only a streamed frame can have written (MYR-454)
// plus a `lastUpdated` inside the quiet horizon. Answering from that costs
// nothing upstream, which is the entire reason the hot ladder above is
// affordable — most freshly paired cars connect by themselves within a minute
// or two, and every one of them exits here for free with its remaining hot
// passes cancelled.
//
// Clearing the schedule rather than rescheduling it, for the same reason a
// successful push clears: the car is fine, it needs no schedule, and if it ever
// goes quiet again it deserves a fresh attempt count instead of a backoff it
// earned while it was broken.
func (r *FleetConfigReconciler) cancelIfStreaming(
	ctx context.Context, c FleetConfigCandidate, vin string, out *ReconcileOutcome,
) bool {
	if !isStreamingNow(c.Status, c.LastUpdated, r.now()) {
		return false
	}
	out.AlreadyStreaming++
	r.logger.Info("fleet-config reconcile: vehicle is streaming; cancelling its remaining passes",
		slog.String("event", "fleet_config_streaming_cancelled"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", vin),
		slog.Time("last_updated", c.LastUpdated))
	if err := r.deps.Attempts.ClearFleetConfigAttempts(ctx, c.VehicleID); err != nil {
		r.logger.Warn("fleet-config reconcile: could not clear the schedule for a streaming vehicle",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("error", err.Error()))
	}
	return true
}

// inHotEpoch reports whether this candidate's pairing epoch is recent enough
// for the hot ladder and the staleness exemption to apply.
func (r *FleetConfigReconciler) inHotEpoch(c FleetConfigCandidate) bool {
	if c.SignedCommandAt.IsZero() {
		return false
	}
	return r.now().Sub(c.SignedCommandAt) <= r.cfg.HotEpochWindow
}

// nextAttemptGap is the single answer to "when should this vehicle be looked at
// again", used by every scheduling write in the reconciler so the hot ladder
// and the MYR-448 backoff can never disagree about one car.
//
// Inside a hot epoch the ladder wins for its first few attempts and then DECAYS
// into the ordinary curve rather than jumping to it: the attempt count handed to
// backoffForOutcome has the ladder's length subtracted, so the pass after the
// ladder waits one plain Interval, the next two, and so on — the same sequence a
// car that had never been hot would have seen.
func (r *FleetConfigReconciler) nextAttemptGap(c FleetConfigCandidate, outcome string) time.Duration {
	if !r.inHotEpoch(c) {
		return r.cfg.backoffForOutcome(c.AttemptCount, outcome)
	}
	if c.AttemptCount >= 0 && c.AttemptCount < len(hotPairingLadder) {
		return hotPairingLadder[c.AttemptCount]
	}
	decayed := c.AttemptCount - len(hotPairingLadder)
	if decayed < 0 {
		decayed = 0
	}
	return r.cfg.backoffForOutcome(decayed, outcome)
}
