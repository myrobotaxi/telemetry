package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupVehicleServiceWindowEndpoint mounts
// PUT /api/tesla/vehicles/{vehicleId}/service-window — the owner-entered
// "expected back" fallback behind `serviceEstimatedEndAt` (MYR-316,
// rest-api.md §7.16).
//
// The route is ALWAYS mounted and needs no tesla-http-proxy, no Tesla token,
// and no Tesla call: this is purely a local owner-scoped write to the Go-owned
// go_vehicle_control_state side table (migration 0017). It exists because
// Tesla's service_data is frequently ALL NULL — a visit booked by phone or a
// walk-in has no appointment record — so without an owner-entered fallback the
// service window, and the scheduler bound built on it, would not exist for
// those cars at all.
//
// `*store.VehicleRepo` satisfies telemetry.OwnerServiceWindowWriter directly
// (the signatures match), so no adapter shim is needed; the snapshot adapter is
// reused for the ownership lookup exactly as §7.14 does.
func setupVehicleServiceWindowEndpoint(deps httpRouteDeps) {
	logger := deps.logger.With(slog.String("component", "vehicle-service-window"))

	handler := telemetry.NewVehicleServiceWindowHandler(
		deps.authenticator,
		&vehicleSnapshotAdapter{repo: deps.vehicleRepo},
		deps.vehicleRepo,
		logger,
	)

	deps.srv.HandleFunc("PUT /api/tesla/vehicles/{vehicleId}/service-window", handler.ServeHTTP)
	logger.Info("vehicle service-window endpoint enabled (PUT /api/tesla/vehicles/{vehicleId}/service-window)")
}
