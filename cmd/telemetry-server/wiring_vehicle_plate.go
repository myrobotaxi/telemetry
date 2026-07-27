package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupVehiclePlateEndpoint mounts PUT /api/tesla/vehicles/{vehicleId}/plate —
// the owner-only license-plate write endpoint (MYR-286, rest-api.md §7.14).
//
// The route is ALWAYS mounted and needs no tesla-http-proxy: the plate is not a
// Tesla value at all (the Fleet API exposes none), so this is purely a local
// owner-scoped DB write against the Prisma-owned `Vehicle.licensePlate` column
// — the narrow carve-out documented in data-lifecycle.md §1.4. No migration:
// the column already exists.
//
// `*store.VehicleRepo` satisfies telemetry.VehiclePlateWriter directly (the
// signatures match), so unlike the read paths no adapter shim is needed; the
// snapshot adapter is reused for the ownership lookup exactly as the §7.12
// teardown does.
func setupVehiclePlateEndpoint(deps httpRouteDeps) {
	logger := deps.logger.With(slog.String("component", "vehicle-plate"))

	handler := telemetry.NewVehiclePlateHandler(
		deps.authenticator,
		&vehicleSnapshotAdapter{repo: deps.vehicleRepo},
		deps.vehicleRepo,
		logger,
	)

	deps.srv.HandleFunc("PUT /api/tesla/vehicles/{vehicleId}/plate", handler.ServeHTTP)
	logger.Info("vehicle plate endpoint enabled (PUT /api/tesla/vehicles/{vehicleId}/plate)")
}
