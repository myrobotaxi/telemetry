package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupServiceStatusMonitor wires MYR-259 Leg 2: the connectivity-edge
// in_service reader. On every connect/disconnect edge it fires ONE debounced,
// authoritative Tesla REST `in_service` read (GET /api/1/vehicles/{vin}) and
// persists status=in_service when the flag is true.
//
// The Fleet API reader targets the DIRECT Fleet API base URL (an UNSIGNED
// authenticated READ, NOT the signing tesla-http-proxy) — the same auth style
// as ListVehicles / GetTelemetryConfig. A read never touches the car, so it is
// wired unconditionally; the per-owner token resolver simply skips a vehicle
// whose owner has no Tesla token. Returns the monitor so main can Stop it.
func setupServiceStatusMonitor(
	cfg *config.Config,
	bus events.Bus,
	vinCache *store.VINCache,
	vehicleRepo *store.VehicleRepo,
	accountRepo *store.AccountRepo,
	logger *slog.Logger,
) (*telemetry.ServiceStatusMonitor, error) {
	log := logger.With(slog.String("component", "service-status"))

	reader := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL: cfg.Proxy().FleetAPIBaseURL, // empty => default NA Fleet API
	}, log.With(slog.String("subcomponent", "fleet-read")))

	monitor := telemetry.NewServiceStatusMonitor(
		bus,
		reader,
		newTeslaTokenResolver(cfg, accountRepo, log),
		&vehicleOwnerAdapter{cache: vinCache},
		&vehicleStatusUpdaterAdapter{repo: vehicleRepo},
		log,
	)
	if err := monitor.Start(); err != nil {
		return nil, err
	}
	return monitor, nil
}
