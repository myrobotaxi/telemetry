package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupVehicleRefreshEndpoint mounts POST /api/tesla/vehicles/{vehicleId}/refresh
// — the owner-only on-demand state refresh (MYR-315, rest-api.md §7.15).
//
// Three existing subsystems are reused verbatim rather than reimplemented:
//
//   - the ServiceStatusMonitor's per-VIN stream recency (MYR-300) decides the
//     free `fresh` short-circuit, so "fresh" means exactly what it means to the
//     backfill gate;
//   - the commands Executor's bounded wake+retry budget does the waking, so an
//     owner-triggered wake spends the same budget and yields the same typed
//     503 vehicle_asleep as a command-triggered one;
//   - the monitor's MYR-260 vehicle_data mapping does the read + publish, so
//     the values land on the same broadcast + persist path as a streamed frame
//     (and pick up `trim` / `seatCoolingCapable` from vehicle_config for free).
//
// The Fleet API reader targets the DIRECT Fleet API base URL — an UNSIGNED
// authenticated READ, matching setupServiceStatusMonitor. Only the wake goes
// through the command transport (proxy-preferred, Fleet REST fallback).
//
// The route is ALWAYS mounted so the SDK sees a typed error rather than a 404:
// with no wake transport configured a car that needs waking resolves to
// command_failed, and a car that is already awake still refreshes normally.
func setupVehicleRefreshEndpoint(deps httpRouteDeps) {
	logger := deps.logger.With(slog.String("component", "vehicle-refresh"))

	transport := newCommandTransport(deps.cfg.Proxy().URL, deps.cfg.Proxy().FleetAPIBaseURL,
		logger.With(slog.String("subcomponent", "wake-transport")))
	waker := commands.NewExecutor(transport, logger.With(slog.String("subcomponent", "wake-executor")))

	fleet := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL: deps.cfg.Proxy().FleetAPIBaseURL, // empty => default NA Fleet API
	}, logger.With(slog.String("subcomponent", "fleet-read")))

	refresher := telemetry.NewVehicleRefresher(
		deps.serviceStatus,
		newTeslaTokenResolver(deps.cfg, deps.accountRepo, logger),
		fleet,
		waker,
		deps.serviceStatus,
		logger,
	)

	handler := telemetry.NewVehicleRefreshHandler(
		deps.authenticator,
		&vehicleSnapshotAdapter{repo: deps.vehicleRepo},
		refresher,
		logger,
	)

	deps.srv.HandleFunc("POST /api/tesla/vehicles/{vehicleId}/refresh", handler.ServeHTTP)
	logger.Info("vehicle refresh endpoint enabled (POST /api/tesla/vehicles/{vehicleId}/refresh)")
}
