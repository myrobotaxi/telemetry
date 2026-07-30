package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupVehicleRideShareEndpoint mounts
// PUT /api/tesla/vehicles/{vehicleId}/ride-share — the owner's switch for
// whether their car accepts ride requests at all (MYR-342, rest-api.md §7.18).
//
// The route is ALWAYS mounted and needs no tesla-http-proxy, no Tesla token, and
// no Tesla call: this is purely a local owner-scoped write to the Go-owned
// go_vehicle_control_state side table (migration 0021), exactly like the §7.16
// service-window sibling. There is no Tesla concept of "this owner is lending
// their car out" for it to talk to — the fact is entirely ours.
//
// Unconditional mounting is also the FAIL-SAFE direction here, and worth stating
// because it runs opposite to the usual instinct. If this route were gated on
// some capability and the gate were off, an owner would be unable to pause a car
// while the three enforcement gates kept honouring whatever value was already
// stored — the toggle would be visible in the catalog and unmovable. A route
// that is always present cannot strand an owner that way.
//
// `*store.VehicleRepo` satisfies telemetry.RideShareWriter directly (the
// signatures match), so no adapter shim is needed; the snapshot adapter is
// reused for the ownership lookup exactly as §7.14 / §7.16 do.
func setupVehicleRideShareEndpoint(deps httpRouteDeps) {
	logger := deps.logger.With(slog.String("component", "vehicle-ride-share"))

	handler := telemetry.NewVehicleRideShareHandler(
		deps.authenticator,
		&vehicleSnapshotAdapter{repo: deps.vehicleRepo},
		deps.vehicleRepo,
		logger,
	)

	deps.srv.HandleFunc("PUT /api/tesla/vehicles/{vehicleId}/ride-share", handler.ServeHTTP)
	logger.Info("vehicle ride-share endpoint enabled (PUT /api/tesla/vehicles/{vehicleId}/ride-share)")
}
