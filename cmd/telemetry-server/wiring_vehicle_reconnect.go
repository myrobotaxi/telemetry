package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupVehicleReconnectEndpoint mounts
// POST /api/tesla/vehicles/{vehicleId}/reconnect — the owner's way back from an
// owner-inactivity telemetry suspension (MYR-592, rest-api.md §7.28).
//
// ALWAYS MOUNTED, even without a tesla-http-proxy, and that is the opposite of
// the instinct the sibling Tesla-touching wirings follow. The route is the
// target of a link in the owner's disconnect notice; a 404 there would read as
// a broken app, whereas the `502` an unwired pusher produces reads as "not right
// now" and is the truth. There is also no risk in mounting it: with no pusher
// the handler refuses before it reaches a token, so no outbound Tesla call can
// fire from a proxy-less deployment or from CI.
//
// IT REUSES THE FIRST-PAIRING PUSHER, deliberately. buildReconnectPusher below
// constructs the same *realFleetPusher the link-time hook and the MYR-448
// reconciler use, over the same FleetAPIClient and the same EndpointConfig, so
// the config this endpoint re-installs is byte-identical to the one the car was
// originally given. A second construction of that request is where a field-map
// change would silently reach three of four doors.
func setupVehicleReconnectEndpoint(deps httpRouteDeps) {
	logger := deps.logger.With(slog.String("component", "vehicle-reconnect"))

	pusher := buildReconnectPusher(deps.cfg, logger)

	handler := telemetry.NewVehicleReconnectHandler(
		deps.authenticator,
		&vehicleSnapshotAdapter{repo: deps.vehicleRepo},
		newTeslaTokenResolver(deps.cfg, deps.accountRepo, logger),
		pusher,
		store.NewTelemetrySuspensionRepo(deps.pool),
		logger,
	)

	deps.srv.HandleFunc("POST /api/tesla/vehicles/{vehicleId}/reconnect", handler.ServeHTTP)
	logger.Info("vehicle reconnect endpoint enabled (POST /api/tesla/vehicles/{vehicleId}/reconnect)",
		slog.Bool("fleet_config_push", pusher != nil),
	)
}

// buildReconnectPusher returns the shared fleet-config pusher, or nil when the
// proxy / telemetry endpoint is unconfigured.
//
// The nil is returned as a TYPED nil check rather than assigned straight into
// the interface, for the reason buildOwnerStreamHook documents: a nil
// *realFleetPusher inside an interface is a NON-nil interface value and would
// sail past the handler's `h.fleet == nil` guard into a nil-receiver call.
func buildReconnectPusher(cfg *config.Config, logger *slog.Logger) telemetry.FleetConfigPusher {
	if cfg.Proxy().URL == "" || cfg.Proxy().FleetTelemetryHostname == "" {
		return nil
	}
	return &realFleetPusher{
		client: telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
			BaseURL:    cfg.Proxy().URL,
			HTTPClient: proxyHTTPClient(cfg.Proxy().URL, logger),
		}, logger.With(slog.String("subcomponent", "fleet-push"))),
		endpoint: telemetry.EndpointConfig{
			Hostname: cfg.Proxy().FleetTelemetryHostname,
			Port:     cfg.Proxy().FleetTelemetryPort,
			CA:       cfg.Proxy().FleetTelemetryCA,
		},
	}
}
