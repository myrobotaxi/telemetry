package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupAccountDeletionEndpoint mounts DELETE /api/users/me — user-initiated
// account deletion (MYR-355, rest-api.md §7.6, data-lifecycle.md §3).
//
// The route is ALWAYS mounted: every step is a local database operation, so
// there is no proxy, no Tesla call and no optional dependency that could make
// the endpoint unsafe to expose. An App Store review requirement must not be
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

	handler := telemetry.NewAccountDeletionHandler(deps.authenticator, deletionDeps, logger)

	deps.srv.HandleFunc("DELETE /api/users/me", handler.ServeHTTP)
	logger.Info("account deletion endpoint enabled (DELETE /api/users/me)",
		slog.Bool("session_invalidation", deps.sessionInvalidator != nil),
		slog.Bool("tesla_grant_revocation", deletionDeps.TeslaLink != nil),
	)
}
