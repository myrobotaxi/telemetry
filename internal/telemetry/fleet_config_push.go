// The push half of the MYR-448 fleet-config reconciler: sending a config to
// one unconfigured car, classifying Tesla's answer, and the shared scheduling
// helpers both halves use.
//
// Split from fleet_config_reconciler.go purely for the CLAUDE.md 300-line file
// cap; the pass logic and this file are one component.

package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// push sends the config for one unconfigured VIN and classifies the answer.
//
// callCtx bounds the Tesla call; schedCtx is the pass context used for the
// bookkeeping write, so an attempt is still recorded when the per-vehicle
// budget is what expired.
func (r *FleetConfigReconciler) push(
	callCtx, schedCtx context.Context,
	c FleetConfigCandidate,
	accessToken, vin string,
	out *ReconcileOutcome,
) {
	result, err := r.deps.Writer.PushTelemetryConfig(callCtx, accessToken, r.request(c.VIN))
	if err != nil {
		out.PushFailures++
		r.logger.Warn("fleet-config reconcile: push failed (will retry after backoff)",
			slog.String("event", "fleet_config_push_failed"),
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", vin),
			slog.String("error", redactedErrorText(err)))
		r.reschedule(schedCtx, c, outcomePushFailed)
		return
	}

	// THE BUG THIS RECONCILER EXISTS FOR: a 200 does not mean it applied.
	if skipErr := SkipErrorFor(result, c.VIN); skipErr != nil {
		var skipped *SkippedVehicleError
		if errors.As(skipErr, &skipped) && skipped.AwaitingVirtualKey() {
			out.AwaitingKey++
			// Not a fault — the owner has genuinely not paired yet. Logged at
			// Info every pass so "which testers are still unpaired" is a log
			// query rather than a mystery.
			r.logger.Info("fleet-config reconcile: awaiting virtual-key pairing",
				slog.String("event", "fleet_config_awaiting_virtual_key"),
				slog.String("vehicle_id", c.VehicleID),
				slog.String("vin", vin),
				slog.String("reason", skipped.Reason))
			r.reschedule(schedCtx, c, outcomeAwaitingKey)
			return
		}
		out.SkippedOther++
		r.logger.Warn("fleet-config reconcile: Tesla skipped the vehicle for an unrecognised reason",
			slog.String("event", "fleet_config_skipped_unknown"),
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", vin),
			slog.String("error", skipErr.Error()))
		r.reschedule(schedCtx, c, outcomeSkippedOther)
		return
	}

	out.Repaired++
	// The headline event: a car that was silently unconfigured is now told to
	// stream. Error level would overstate it; this is a repair, and it should
	// be trivially greppable.
	r.logger.Info("fleet-config reconcile: config pushed, vehicle should now stream",
		slog.String("event", "fleet_config_repaired"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", vin),
		slog.Int("updated_vehicles", result.Response.UpdatedVehicles))

	// Success clears the schedule: the car should now stream, and if it ever
	// goes quiet again it deserves a fresh count rather than an old backoff.
	if err := r.deps.Attempts.ClearFleetConfigAttempts(schedCtx, c.VehicleID); err != nil {
		r.logger.Warn("fleet-config reconcile: could not clear attempt schedule",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("error", err.Error()))
	}
}

// reschedule records an unsuccessful attempt and pushes the vehicle's next
// eligibility out by the backoff for its attempt count.
//
// A bookkeeping failure is logged, never fatal — but it does matter: without
// the row the car stays permanently due, so the pass would retry it at full
// rate. That is the pre-backoff behaviour, i.e. degraded but not broken.
func (r *FleetConfigReconciler) reschedule(ctx context.Context, c FleetConfigCandidate, outcome string) {
	now := r.now()
	next := now.Add(r.cfg.backoffForOutcome(c.AttemptCount, outcome))
	if err := r.deps.Attempts.RecordFleetConfigAttempt(ctx, c.VehicleID, now, next, outcome); err != nil {
		r.logger.Warn("fleet-config reconcile: could not record attempt schedule (vehicle stays due)",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("outcome", outcome),
			slog.String("error", err.Error()))
	}
}

// request builds the same config body the link hook, the REST handler and
// `ops fleet-config push` send, so a reconciled car is configured identically
// to a hand-pushed one.
func (r *FleetConfigReconciler) request(vin string) FleetConfigRequest {
	var ca *string
	if r.endpoint.CA != "" {
		ca = &r.endpoint.CA
	}
	// Tesla requires exp between ~31 and ~360 days from now.
	exp := r.now().Add(350 * 24 * time.Hour).Unix()
	return FleetConfigRequest{
		VINs: []string{vin},
		Config: FleetConfig{
			Hostname:   r.endpoint.Hostname,
			Port:       r.endpoint.Port,
			CA:         ca,
			Fields:     DefaultFieldConfig(),
			AlertTypes: []string{"service"},
			Exp:        &exp,
		},
	}
}
