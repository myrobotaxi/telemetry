package main

// MYR-505 zero-touch setup completion wiring:
// POST /api/tesla/vehicles/{vehicleId}/complete-setup (rest-api.md §7.23).
//
// TWO CLIENTS, THE SAME SPLIT THE RECONCILER USES. `wake_up` and
// `fleet_status` are UNSIGNED authenticated reads and go to the DIRECT Fleet
// API; the config push is the only signed call and it goes through the
// tesla-http-proxy — inside MYR-489's own code, which is why nothing here
// builds a writer client at all. Sending either kind to the other's base URL
// fails, so the split is wired once, here, and never restated downstream.
//
// THE ROUTE IS ALWAYS MOUNTED, deliberately, even when the reconciler is off.
// A 404 would tell an SDK the server is too old to complete setup; what is
// actually true on a proxy-less deployment is that pairing can still be
// CHECKED (both probes are unsigned) and only the push is unavailable. So the
// endpoint answers `awaiting_virtual_key` / `streaming` / `token_failed`
// normally and returns a typed `503 service_unavailable` on the one path that
// needs the signer. This is the same reasoning setupVehicleRefreshEndpoint
// gives for always mounting §7.15.

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

func setupVehicleCompleteSetupEndpoint(deps httpRouteDeps) {
	logger := deps.logger.With(slog.String("component", "complete-setup"))

	probe := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL: deps.cfg.Proxy().FleetAPIBaseURL, // empty => default NA Fleet API
	}, logger.With(slog.String("subcomponent", "fleet-read")))

	completerDeps := telemetry.SetupCompleterDeps{
		Vehicles: &vehicleSnapshotAdapter{repo: deps.vehicleRepo},
		Tokens:   newTeslaTokenResolver(deps.cfg, deps.accountRepo, logger),
		Probe:    probe,
		Pairing:  &fleetConfigCandidateAdapter{repo: deps.vehicleRepo},
	}

	// Guarded assignment, NOT a direct one. A nil *FleetConfigReconciler placed
	// into the interface field would be a NON-nil interface holding a nil
	// pointer — the classic Go trap — and the completer's `Repusher == nil`
	// check would pass straight through it into a nil-receiver call on the
	// first paired car.
	if deps.fleetConfigReconciler != nil {
		completerDeps.Repusher = deps.fleetConfigReconciler
	} else {
		logger.Warn("complete-setup: no fleet-config reconciler (proxy/telemetry endpoint not configured) — " +
			"pairing can be checked but no config can be pushed")
	}

	completer := telemetry.NewSetupCompleter(completerDeps, telemetry.SetupCompletionConfig{}, logger)

	handler := telemetry.NewSetupCompletionHandler(
		deps.authenticator,
		&vehicleSnapshotAdapter{repo: deps.vehicleRepo},
		completer,
		logger,
	)

	deps.srv.HandleFunc("POST /api/tesla/vehicles/{vehicleId}/complete-setup", handler.ServeHTTP)
	logger.Info("vehicle complete-setup endpoint enabled (POST /api/tesla/vehicles/{vehicleId}/complete-setup)",
		slog.Bool("config_push_available", deps.fleetConfigReconciler != nil))
}
