// The MYR-489 escalation: synced-but-not-streaming on a car we can prove is
// awake earns ONE forced config re-push per pairing epoch.
//
// MYR-448 taught the reconciler to notice this state and say so
// (`fleet_config_synced_not_streaming`), then back off — on the theory that a
// car reporting a synced config is merely asleep. Tester Nabil's car disproved
// the theory in prod: signed commands applying (so awake, so paired), Tesla
// reporting the config synced, and zero receiver connections for two days. The
// config record existed at Tesla and the car had simply never acted on it.
//
// Contrast James Guan's car in the same state, which started streaming minutes
// after pairing with no intervention. Both must be handled by the same loop
// with no operator, which is what makes this an ESCALATION rather than a policy
// change: the ordinary backoff still runs, and the forced push fires only when
// the cheap explanations are exhausted and the expensive one is evidenced.

package telemetry

import (
	"context"
	"log/slog"
	"time"
)

// Outcome labels for the escalation path. Same vocabulary rules as the MYR-448
// labels: internal, never wire-exposed, never a raw Tesla body.
const (
	outcomeForcedRepush     = "forced_repush"
	outcomeForcedRepushFail = "forced_repush_failed"
)

// fleetVehicleStateReader reads Tesla's own connectivity state for a VIN. Like
// GetTelemetryConfig this is an UNSIGNED authenticated read and MUST target the
// direct Fleet API, never the signing proxy.
type fleetVehicleStateReader interface {
	GetVehicle(ctx context.Context, token, vin string) (*FleetVehicleState, error)
}

// fleetTelemetryConfigDeleter removes a VIN's config. It carries no body to
// sign, so it rides the proxy's plain-forward path exactly like the per-VIN
// GET (see DeleteTelemetryConfig's contract note) and is safe on the writer
// client.
type fleetTelemetryConfigDeleter interface {
	DeleteTelemetryConfig(ctx context.Context, token, vin string) error
}

// handleSyncedQuiet decides what to do about a candidate whose config Tesla
// reports as synced while the car stays silent.
//
// Returns after either escalating or rescheduling; the caller is done with the
// vehicle either way.
func (r *FleetConfigReconciler) handleSyncedQuiet(
	callCtx, schedCtx context.Context,
	c FleetConfigCandidate,
	accessToken, vin string,
	out *ReconcileOutcome,
) {
	out.AlreadySynced++

	// The MYR-448 line, unchanged: it is what tells an operator to stop looking
	// at fleet-config, and it is now also the marker of the FIRST observation
	// that arms the escalation on the next pass.
	r.logger.Info("fleet-config reconcile: config already synced but vehicle is quiet",
		slog.String("event", "fleet_config_synced_not_streaming"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", vin),
		slog.String("last_outcome", c.LastOutcome),
		slog.Time("last_updated", c.LastUpdated))

	reason, ok := r.escalationBlocker(c)
	if !ok {
		r.logger.Info("fleet-config reconcile: not escalating a synced-but-quiet vehicle",
			slog.String("event", "fleet_config_escalation_declined"),
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", vin),
			slog.String("reason", reason))
		r.reschedule(schedCtx, c, outcomeSyncedQuiet)
		return
	}

	if !r.provenAwake(callCtx, c, accessToken, vin) {
		r.reschedule(schedCtx, c, outcomeSyncedQuiet)
		return
	}

	r.forceRepush(callCtx, schedCtx, c, accessToken, vin, out)
}

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
//  3. EPOCH BUDGET. One force per pairing epoch. Unspent while forced_repush_at
//     is zero; re-armed only when a NEW epoch opens (signed_command_at moves
//     past the last force). This is the bound that makes a permanently
//     unreachable car cost one escalation and then nothing — it cannot loop,
//     because the forced path records a normal incrementing attempt instead of
//     clearing the schedule.
//
// MULTI-INSTANCE BOUND. The budget is read from the candidate row and stamped
// after the fact, so two server instances passing over the same car in the same
// window could each spend the epoch once — the same read-then-write property
// MYR-448's ordinary attempt path already has, and the same reason its counter
// increments server-side. The bound therefore degrades to "at most one forced
// re-push per epoch PER INSTANCE", not to an unbounded loop.
func (r *FleetConfigReconciler) escalationBlocker(c FleetConfigCandidate) (string, bool) {
	if c.LastOutcome != outcomeSyncedQuiet {
		return "first synced-but-quiet observation; giving the car a cycle to connect", false
	}
	if !c.LastAttemptAt.IsZero() && r.now().Sub(c.LastAttemptAt) < r.cfg.SyncedQuietGrace {
		return "synced-quiet grace period has not elapsed", false
	}
	if !c.ForcedRepushAt.IsZero() && !c.ForcedRepushAt.Before(c.SignedCommandAt) {
		return "forced re-push already spent for this key-pairing epoch", false
	}
	return "", true
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

// forceRepush performs the escalation: DELETE the existing config, then create
// it again.
//
// WHY DELETE-THEN-CREATE AND NOT A PLAIN OVERWRITE. The overwrite is what the
// ordinary path already does, with the identical body, and Tesla's answer to it
// is the `synced: true` that got us here — the config record at Tesla is
// already correct, so re-POSTing it changes no state the car would notice. What
// is stuck is the car's own telemetry session, and the only lever the Fleet API
// gives us over that is the existence of the config record itself. Deleting it
// and creating it again produces a genuinely new record for the firmware to
// re-read.
//
// THE WINDOW. Between the DELETE and the POST the car has no config. That is
// acceptable precisely because this path only runs against a car that is not
// streaming — there is no working session to interrupt — and the window is one
// HTTP round-trip. If the POST nevertheless fails, the car is left worse than
// we found it, so that case gets its own loud event and a retry at the BASE
// interval rather than a doubled backoff.
//
// CONNECTING MID-FLIGHT — an accepted, bounded risk. Candidates are listed at
// the start of a pass, so a car could in principle open its stream between the
// listing and its turn, and be re-configured while it is coming up. The window
// is one pass at most, the car must ALSO have been silent across two separate
// observations to reach this branch, and the worst outcome is that the same
// config it just read is re-issued and it re-syncs. Closing the window would
// mean a fresh per-vehicle staleness read against the Prisma "Vehicle" table
// immediately before every force, which buys a few seconds of precision on an
// event that is already several orders of magnitude rarer than the two-day
// silence this exists to end.
func (r *FleetConfigReconciler) forceRepush(
	callCtx, schedCtx context.Context,
	c FleetConfigCandidate,
	accessToken, vin string,
	out *ReconcileOutcome,
) {
	r.logger.Info("fleet-config reconcile: forcing a config re-push on an awake but silent vehicle",
		slog.String("event", "fleet_config_forced_repush"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", vin),
		slog.Time("signed_command_at", c.SignedCommandAt))

	// A delete failure is NOT fatal to the escalation — a 404 for a config that
	// is already gone reads the same as a transient error here, and the POST
	// below is the part that matters. It is logged and the push proceeds.
	deleted := true
	if err := r.deps.Writer.DeleteTelemetryConfig(callCtx, accessToken, c.VIN); err != nil {
		deleted = false
		r.logger.Warn("fleet-config reconcile: forced re-push could not delete the existing config",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", vin),
			slog.String("error", redactedErrorText(err)))
	}

	result, err := r.deps.Writer.PushTelemetryConfig(callCtx, accessToken, r.request(c.VIN))
	if err != nil || SkipErrorFor(result, c.VIN) != nil {
		r.recordFailedForce(schedCtx, c, vin, deleted, err)
		out.PushFailures++
		return
	}

	out.ForcedRepushes++
	r.logger.Info("fleet-config reconcile: forced re-push applied; vehicle should re-establish its stream",
		slog.String("event", "fleet_config_forced_repush_applied"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", vin),
		slog.Int("updated_vehicles", result.Response.UpdatedVehicles))

	// Deliberately NOT ClearFleetConfigAttempts. An ordinary repair clears the
	// schedule because it fixed a config that was genuinely missing; this one
	// pushed a config that already existed, on a hypothesis about the car's
	// firmware. Recording it as a normal incrementing attempt — while stamping
	// the epoch budget as spent — is what guarantees that a car which STILL
	// does not stream falls back into plain backoff instead of re-forcing every
	// pass.
	r.recordForce(schedCtx, c, r.now().Add(r.cfg.backoffFor(c.AttemptCount)), outcomeForcedRepush)
}

// recordFailedForce handles a forced re-push whose create step did not apply.
func (r *FleetConfigReconciler) recordFailedForce(
	ctx context.Context, c FleetConfigCandidate, vin string, deleted bool, err error,
) {
	msg := "fleet-config reconcile: forced re-push failed"
	level := slog.LevelWarn
	if deleted {
		// The delete succeeded and the create did not: the car now has NO
		// config at all. Recoverable — the retry below is at the base interval,
		// and the ordinary unconfigured path will push a fresh one — but it is
		// the one state this feature can leave behind that is worse than doing
		// nothing, so it is an error-level, uniquely greppable event.
		msg = "fleet-config reconcile: forced re-push deleted the config but could not recreate it — vehicle is UNCONFIGURED"
		level = slog.LevelError
	}
	r.logger.Log(ctx, level, msg,
		slog.String("event", "fleet_config_forced_repush_failed"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", vin),
		slog.Bool("config_deleted", deleted),
		slog.String("error", redactedErrorText(err)))

	// Base interval, not the doubled backoff: whatever else is true, this car
	// may now be unconfigured, and the ordinary push path can fix that. The
	// epoch budget is still stamped, so this cannot become a forced-push loop.
	r.recordForce(ctx, c, r.now().Add(r.cfg.Interval), outcomeForcedRepushFail)
}

// recordForce writes the attempt and stamps the pairing epoch's escalation
// budget as spent. A bookkeeping failure is logged, never fatal — but it is
// worse here than elsewhere, because an unstamped budget is a budget that can
// be spent again, so it says so.
func (r *FleetConfigReconciler) recordForce(
	ctx context.Context, c FleetConfigCandidate, next time.Time, outcome string,
) {
	if err := r.deps.Attempts.RecordForcedFleetConfigRepush(ctx, c.VehicleID, r.now(), next, outcome); err != nil {
		r.logger.Warn("fleet-config reconcile: could not record forced re-push (epoch budget not stamped)",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("outcome", outcome),
			slog.String("error", err.Error()))
	}
}
