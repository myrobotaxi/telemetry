package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupAccountDeletionEndpoint mounts DELETE /api/users/me — user-initiated
// account deletion (MYR-355, rest-api.md §7.6, data-lifecycle.md §3).
//
// The route is ALWAYS mounted. Every step that ERASES anything is a local
// database operation, and the two Tesla calls in the sequence — the grant
// revoke (MYR-366) and the per-car stream-config delete (MYR-593) — are
// best-effort and independently nil-able, so no proxy and no Tesla credentials
// are required to expose it. An App Store review requirement must not be
// contingent on deployment shape.
//
// The session invalidator is the one nil-able seam: only the real
// JWTAuthenticator owns the caches (dev mode runs a NoopAuthenticator with
// nothing to invalidate), and a nil there simply means the deleted user's
// unexpired access token stops working when the cache TTL elapses rather than
// immediately.
func setupAccountDeletionEndpoint(deps httpRouteDeps) {
	logger := deps.logger.With(slog.String("component", "account-deletion"))

	rides := &rideRequestStoreAdapter{repo: deps.rideRepo}

	// MYR-366: revoke the Tesla grant before any step deletes the tokens.
	// A typed nil would satisfy the interface and defeat the handler's nil
	// check, so the assignment is guarded rather than passed straight through.
	deletionDeps := telemetry.AccountDeletionDeps{
		Vehicles: &ownedVehicleListerAdapter{repo: deps.vehicleRepo},
		Teardown: &ownerTeardownAdapter{teardown: store.NewOwnerTeardown(deps.pool, logger)},
		Rides:    &accountRideCancellerAdapter{repo: deps.rideRepo, rides: rides},
		Events:   deps.bus,
		Data:     &accountDataDeleterAdapter{deleter: store.NewAccountDeleter(deps.pool, logger)},
		Sessions: deps.sessionInvalidator,
	}
	if revoker := newTeslaLinkRevoker(deps, logger); revoker != nil {
		deletionDeps.TeslaLink = revoker
	}
	// MYR-593: stop each owned car streaming at Tesla before the revoke above
	// kills the grant those calls authenticate with. Same deleter and same
	// resolver the per-vehicle teardown endpoint uses — a deleted account's cars
	// must be severed exactly as an owner-removed car is, and the two paths
	// having drifted apart is the whole of the bug. Guarded rather than always
	// assigned so the handler can distinguish "no proxy configured" (a warning
	// it emits) from a configured deleter that failed.
	if fleet := newFleetConfigDeleter(deps.cfg, logger); fleet != nil {
		deletionDeps.StreamConfigs = telemetry.NewStreamConfigTeardown(
			fleet,
			newTeslaTokenResolver(deps.cfg, deps.accountRepo, logger),
			logger,
		)
	}

	handler := telemetry.NewAccountDeletionHandler(deps.authenticator, deletionDeps, logger)

	deps.srv.HandleFunc("DELETE /api/users/me", handler.ServeHTTP)
	logger.Info("account deletion endpoint enabled (DELETE /api/users/me)",
		slog.Bool("session_invalidation", deps.sessionInvalidator != nil),
		slog.Bool("tesla_grant_revocation", deletionDeps.TeslaLink != nil),
		slog.Bool("tesla_stream_config_delete", deletionDeps.StreamConfigs != nil),
	)
}
