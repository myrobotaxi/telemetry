package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// teslaUnlinkedDeepLink is the app deep link Tesla returns to after the owner
// confirms the consent-revoke page (car-offboarding.md §6). It is the sibling
// of the existing "myrobotaxi://tesla-linked" onboarding deep link (MYR-246).
const teslaUnlinkedDeepLink = "myrobotaxi://tesla-unlinked"

// setupVehicleTeardownEndpoint mounts DELETE /api/tesla/vehicles/{vehicleId} —
// the owner "Remove this car" full-teardown endpoint (MYR-258). The route is
// ALWAYS mounted: the authoritative teardown is the local DB transaction, which
// works with or without the tesla-http-proxy. The Tesla-side stream-config
// delete is wired ONLY when the proxy is configured (otherwise the deleter is
// nil and that best-effort step is skipped — so no real Tesla call can fire in
// tests/CI or on a proxy-less deployment).
func setupVehicleTeardownEndpoint(deps httpRouteDeps) {
	logger := deps.logger.With(slog.String("component", "vehicle-teardown"))

	// Tesla-side config deleter + token resolver: only meaningful when the
	// tesla-http-proxy is configured. When absent, fleet stays nil (the
	// handler skips the best-effort Tesla call).
	var fleet telemetry.TelemetryConfigDeleter
	if deps.cfg.Proxy().URL != "" {
		fleet = telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
			BaseURL:    deps.cfg.Proxy().URL,
			HTTPClient: proxyHTTPClient(deps.cfg.Proxy().URL, logger),
		}, logger.With(slog.String("subcomponent", "fleet")))
	}

	resolver := newTeslaTokenResolver(deps.cfg, deps.accountRepo, logger)

	handler := telemetry.NewVehicleTeardownHandler(
		deps.authenticator,
		&vehicleSnapshotAdapter{repo: deps.vehicleRepo},
		resolver,
		fleet,
		&ownerTeardownAdapter{teardown: store.NewOwnerTeardown(deps.pool, logger)},
		telemetry.VehicleTeardownConfig{
			RevokeClientID: deps.cfg.TeslaOAuth().ClientID,
			RevokeBackURL:  teslaUnlinkedDeepLink,
		},
		logger,
	)

	deps.srv.HandleFunc("DELETE /api/tesla/vehicles/{vehicleId}", handler.ServeHTTP)
	logger.Info("vehicle teardown endpoint enabled (DELETE /api/tesla/vehicles/{vehicleId})",
		slog.Bool("tesla_stream_config_delete", fleet != nil),
	)
}

// newTeslaTokenResolver builds an off-request Tesla token resolver (with
// auto-refresh when OAuth creds are configured), mirroring the dispatch
// resolver wiring in setupNavDispatcher.
func newTeslaTokenResolver(cfg *config.Config, accountRepo *store.AccountRepo, logger *slog.Logger) *telemetry.TeslaTokenResolver {
	var opts []telemetry.TeslaTokenResolverOption
	if cfg.TeslaOAuth().ClientID != "" {
		refresher := telemetry.NewTokenRefresher(telemetry.TeslaOAuthConfig{
			ClientID:     cfg.TeslaOAuth().ClientID,
			ClientSecret: cfg.TeslaOAuth().ClientSecret,
		}, logger.With(slog.String("subcomponent", "token-refresh")))
		opts = append(opts, telemetry.WithResolverRefresher(refresher, &teslaTokenUpdaterAdapter{repo: accountRepo}))
	}
	return telemetry.NewTeslaTokenResolver(
		&teslaTokenAdapter{repo: accountRepo},
		logger.With(slog.String("subcomponent", "token")),
		opts...,
	)
}
