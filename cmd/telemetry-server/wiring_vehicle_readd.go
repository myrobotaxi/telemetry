package main

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupVehicleReaddEndpoint mounts POST /api/tesla/vehicles/{teslaVehicleId}/re-add
// — the owner "Add this car back" deliberate re-add endpoint (MYR-262). The route
// is ALWAYS mounted: clearing the removed-vehicle tombstone (the durable un-trap)
// is a local DB transaction that needs no proxy. The inline re-provision (list +
// upsert + stream-config push) is best-effort and only pushes config when the
// tesla-http-proxy is configured (same guard as the onboarding stream hook), so
// no real Tesla call can fire in tests/CI or on a proxy-less deployment.
//
// This is the deliberate half of the MYR-261 tombstone design: it clears the
// tombstone via store.RemovedVehicleRegistry.ClearTombstone BEFORE provisioning,
// whereas the passive AfterLink sync never clears one.
func setupVehicleReaddEndpoint(deps httpRouteDeps) {
	logger := deps.logger.With(slog.String("component", "vehicle-readd"))

	// Owner-scoped tombstone clearer (the un-trap). *store.RemovedVehicleRegistry
	// satisfies telemetry.TombstoneClearer directly.
	registry := store.NewRemovedVehicleRegistry(deps.pool, logger)

	// Reuse the onboarding stream hook's shared per-vehicle provisioning path so a
	// re-added car returns through exactly the code the passive sync uses. The
	// provisioner (OwnerProvisioner) is the same owner-scoped, tombstone-gated
	// upsert; the hook's pusher stays nil unless the proxy is configured.
	//
	// The inline provisioner is wired only when an encryptor is present (it always
	// is in production — main injects it; OwnerProvisioner mandates a non-nil
	// encryptor). When absent (route-surface tests), the provisioner stays nil and
	// the handler still clears the tombstone — the durable un-trap — leaving the
	// re-provision to the next Tesla link's passive sync.
	var provisioner telemetry.VehicleReaddProvisioner
	if deps.encryptor != nil {
		prov := store.NewOwnerProvisioner(deps.pool, deps.encryptor, logger)
		hook := buildOwnerStreamHook(deps.cfg, prov, deps.fleetConfigReconciler, logger)
		resolver := newTeslaTokenResolver(deps.cfg, deps.accountRepo, logger)
		provisioner = &readdProvisionerAdapter{tokens: resolver, hook: hook, logger: logger}
	}

	handler := telemetry.NewVehicleReaddHandler(
		deps.authenticator,
		registry,
		provisioner,
		logger,
	)

	deps.srv.HandleFunc("POST /api/tesla/vehicles/{teslaVehicleId}/re-add", handler.ServeHTTP)
	logger.Info("vehicle re-add endpoint enabled (POST /api/tesla/vehicles/{teslaVehicleId}/re-add)")
}

// readdProvisionerAdapter satisfies telemetry.VehicleReaddProvisioner. It
// resolves the owner's Tesla token off the request path (best-effort, with
// auto-refresh when OAuth creds are configured) and delegates the targeted,
// owner-filtered re-provision to the shared stream hook. A missing token is NOT
// an error: the tombstone is already cleared (the durable un-trap), so the car
// will be provisioned by the next Tesla link's passive sync.
type readdProvisionerAdapter struct {
	tokens *telemetry.TeslaTokenResolver
	hook   *ownerStreamHook
	logger *slog.Logger
}

// ProvisionReaddedVehicle resolves the token and re-provisions the single owned
// car matching teslaVehicleID. Returns whether it was provisioned inline.
func (a *readdProvisionerAdapter) ProvisionReaddedVehicle(ctx context.Context, userID, teslaVehicleID string) (bool, error) {
	tok, err := a.tokens.Resolve(ctx, userID)
	if err != nil {
		a.logger.Warn("re-add: no Tesla token — skipping inline provision (tombstone cleared; next link provisions)",
			slog.String("user_id", userID))
		//nolint:nilerr // best-effort: an unresolvable Tesla token is NOT a re-add
		// failure — the tombstone is already cleared (the durable un-trap), so the
		// next Tesla link's passive sync provisions the car. Surfacing the error
		// would wrongly fail the un-trap.
		return false, nil
	}
	return a.hook.ReaddVehicle(ctx, userID, tok.AccessToken, teslaVehicleID), nil
}
