// Step 2 of the MYR-505 sequence: giving a sleeping car a chance to act on the
// config it is about to be handed.
//
// Split from setup_completion.go for the CLAUDE.md 300-line file cap; the
// sequence and this file are one component, exactly as
// setup_completion_configure.go is.

package telemetry

import (
	"context"
	"log/slog"
)

// wake issues the unsigned wake and reports whether Tesla ACCEPTED it.
//
// The returned vehicle state is logged and otherwise ignored: for a sleeping
// car Tesla answers with the pre-wake state, so gating on `online` would refuse
// every car this endpoint exists to serve (see fleet_api_vehicle_wake.go).
func (s *SetupCompleter) wake(ctx context.Context, token string, row VehicleSnapshotRow, vin string) bool {
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()

	state, err := s.deps.Probe.WakeVehicle(callCtx, token, row.VIN)
	if err != nil {
		s.logger.Warn("complete-setup: wake was refused",
			slog.String("vehicle_id", row.ID), slog.String("vin", vin),
			slog.String("error", redactedErrorText(err)))
		return false
	}
	reported := ""
	if state != nil {
		reported = state.State
	}
	s.logger.Info("complete-setup: wake accepted",
		slog.String("event", "setup_complete_wake_accepted"),
		slog.String("vehicle_id", row.ID), slog.String("vin", vin),
		slog.String("reported_state", reported))
	return true
}
