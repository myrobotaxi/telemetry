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
	var fleet telemetry.FleetConfigDeleter
	if deps.cfg.Proxy().URL != "" {
		fleet = telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
			BaseURL:    deps.cfg.Proxy().URL,
			HTTPClient: proxyHTTPClient(deps.cfg.Proxy().URL, logger),
		}, logger.With(slog.String("subcomponent", "fleet")))
	}

	resolver := newTeslaTokenResolver(deps.cfg, deps.accountRepo, logger)

	// MYR-366: active revocation of the Tesla grant on a last-vehicle removal.
	// Wired ONLY when an OAuth client id is configured — with no client id the
	// revoke call has nothing to identify itself with, and leaving it nil keeps
	// tests/CI and creds-less deployments incapable of any outbound Tesla call.
	var grantOpts []telemetry.VehicleTeardownOption
	if revoker := newTeslaLinkRevoker(deps, logger); revoker != nil {
		grantOpts = append(grantOpts, telemetry.WithTeslaGrantRevocation(revoker))
	}

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
		grantOpts...,
	)

	deps.srv.HandleFunc("DELETE /api/tesla/vehicles/{vehicleId}", handler.ServeHTTP)
	logger.Info("vehicle teardown endpoint enabled (DELETE /api/tesla/vehicles/{vehicleId})",
		slog.Bool("tesla_stream_config_delete", fleet != nil),
		slog.Bool("tesla_grant_revocation", len(grantOpts) > 0),
	)
}

// newTeslaLinkRevoker builds the MYR-366 Tesla grant revoker shared by the
// per-vehicle teardown (last car only) and the account-deletion sequence
// (whole account). Returns nil when no Tesla OAuth client id is configured, so
// a deployment without Tesla credentials — and every unit test — makes no
// outbound revoke call at all.
//
// It reads the STORED token through teslaTokenAdapter rather than the
// refreshing TeslaTokenResolver: revocation needs the refresh token, and
// resolving would mint a fresh grant from Tesla purely to destroy it.
func newTeslaLinkRevoker(deps httpRouteDeps, logger *slog.Logger) *telemetry.TeslaLinkRevoker {
	if deps.cfg.TeslaOAuth().ClientID == "" {
		return nil
	}
	sub := logger.With(slog.String("subcomponent", "tesla-revoke"))
	return telemetry.NewTeslaLinkRevoker(
		&teslaTokenAdapter{repo: deps.accountRepo},
		&ownedVehicleListerAdapter{repo: deps.vehicleRepo},
		telemetry.NewTokenRevoker(telemetry.TeslaOAuthConfig{
			ClientID:     deps.cfg.TeslaOAuth().ClientID,
			ClientSecret: deps.cfg.TeslaOAuth().ClientSecret,
		}, sub),
		sub,
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
