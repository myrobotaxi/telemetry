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
		// MYR-316: on the same connectivity-edge path (and sharing its per-VIN
		// debounce), read Tesla's service_data for an in-service car and clear
		// both service-window columns when the car leaves service. The reader
		// is the SAME direct-Fleet-API client as the in_service read — an
		// unsigned authenticated read, never the signing proxy.
		telemetry.WithServiceWindow(
			reader,
			vehicleRepo,
			&vehicleIDAdapter{cache: vinCache},
		),
		// MYR-320: ONE extra non-waking GET on the same trigger, for the FSD
		// designation — the release-notes title is the only place the Fleet API
		// exposes it. Same direct-Fleet-API client, same unsigned read style.
		telemetry.WithReleaseNotes(reader),
		// MYR-320: populate the Prisma-owned "Vehicle".color column from
		// vehicle_config.exterior_color. The narrow owner-scoped UPDATE carve-out
		// (data-lifecycle.md §1.4); the wire field `color` already exists and is
		// already emitted from that column, so this needs no contract change.
		telemetry.WithVehicleColor(vehicleRepo),
		// MYR-507: fill the Prisma-owned "Vehicle".model / .year columns from
		// the VIN. Same carve-out shape as the colour write above, and needed
		// for the same reason — both are already wire fields emitted from these
		// columns, and nothing has ever populated them for a car the GO server
		// provisioned. Unlike the colour this costs no Tesla read at all.
		telemetry.WithVehicleIdentity(vehicleRepo),
		// MYR-320: the periodic in-service re-poll. STRUCTURAL — the edge-only
		// triggers above never fire for a car that sits offline at a service
		// centre for days, so nothing re-read it and a mid-visit service_etc was
		// missed. The pass runs the SAME read bundle through the SAME per-VIN
		// debounce; only the trigger is new.
		telemetry.WithPeriodicInServicePoll(vehicleRepo, telemetry.PeriodicPollConfig{
			Enabled:  cfg.ServiceRepollEnabled(),
			Interval: cfg.ServiceRepollInterval(),
		}),
	)
	if err := monitor.Start(); err != nil {
		return nil, err
	}
	return monitor, nil
}
