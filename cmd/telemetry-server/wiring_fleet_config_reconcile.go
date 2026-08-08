package main

// MYR-448 fleet-config reconciler wiring.
//
// The onboarding-to-streaming path has exactly one automatic fleet-config
// push, and it fires inside the Tesla OAuth callback — necessarily BEFORE the
// owner pairs the virtual key, since pairing is a manual step in the Tesla
// app. Tesla answers that push with 200 + `skipped_vehicles: {vin:
// "missing_key"}`, so it applies nothing, and until now nothing ever retried.
// Every self-serve owner was therefore linked-but-never-streaming, forever.
//
// This wires the retry that docs/architecture/self-serve-onboarding.md §5
// always specified ("it is retried when pairing completes") but that was never
// built. It is a reconciler and not an event hook because pairing happens
// inside Tesla's app: there is no event to subscribe to.
//
// TWO CLIENTS, ON PURPOSE. The config READ is an unsigned authenticated call
// that must go to the direct Fleet API; the config PUSH must go through the
// tesla-http-proxy, which signs it into a JWS. Sending either to the other's
// base URL fails. This mirrors buildOwnerStreamHook, which splits its lister
// and pusher the same way.

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// fleetConfigReconcileTimeout bounds the ONE synchronous startup pass. A
// deploy is the moment an operator is most likely to be watching, so getting a
// pass in early is worth a short wait — but never worth stalling the boot: a
// pass that times out is retried by the loop within the interval.
const fleetConfigReconcileTimeout = 60 * time.Second

// setupFleetConfigReconciler builds the reconciler, runs one synchronous pass,
// and starts the background loop.
//
// SAFETY INVARIANT (self-serve-onboarding.md §5): this component pushes config
// to a REAL car, so it is constructed only when both the signing proxy and the
// telemetry endpoint are configured. Absent either, it logs and stays off —
// the same runtime guard buildOwnerStreamHook uses to keep live pushes out of
// dev and test processes.
//
// A pass failure never blocks startup.
func setupFleetConfigReconciler(
	ctx context.Context,
	cfg *config.Config,
	vehicleRepo *store.VehicleRepo,
	accountRepo *store.AccountRepo,
	logger *slog.Logger,
) {
	log := logger.With(slog.String("component", "fleet-config-reconcile"))

	if cfg.Proxy().URL == "" || cfg.Proxy().FleetTelemetryHostname == "" {
		log.Warn("fleet-config reconciler disabled: proxy/telemetry endpoint not configured — " +
			"a car whose config push was skipped pre-pairing will NOT self-heal")
		return
	}

	reader := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL: cfg.Proxy().FleetAPIBaseURL, // empty => default NA Fleet API
	}, log.With(slog.String("subcomponent", "fleet-read")))

	writer := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL:    cfg.Proxy().URL,
		HTTPClient: proxyHTTPClient(cfg.Proxy().URL, log),
	}, log.With(slog.String("subcomponent", "fleet-push")))

	reconciler := telemetry.NewFleetConfigReconciler(
		telemetry.FleetConfigReconcilerDeps{
			Candidates: &fleetConfigCandidateAdapter{repo: vehicleRepo},
			Reader:     reader,
			Writer:     writer,
			Tokens:     newTeslaTokenResolver(cfg, accountRepo, log),
		},
		telemetry.FleetConfigReconcileConfig{},
		telemetry.EndpointConfig{
			Hostname: cfg.Proxy().FleetTelemetryHostname,
			Port:     cfg.Proxy().FleetTelemetryPort,
			CA:       cfg.Proxy().FleetTelemetryCA,
		},
		log,
	)

	log.Info("fleet-config reconciler enabled")

	passCtx, cancel := context.WithTimeout(ctx, fleetConfigReconcileTimeout)
	out, err := reconciler.Reconcile(passCtx)
	cancel()
	if err != nil {
		log.Warn("fleet-config reconcile: startup pass failed (non-fatal)",
			slog.String("error", err.Error()))
	} else {
		log.Info("fleet-config reconcile: startup pass complete",
			slog.Int("examined", out.Examined),
			slog.Int("repaired", out.Repaired),
			slog.Int("awaiting_virtual_key", out.AwaitingKey))
	}

	go reconciler.RunReconcileLoop(ctx)
}

// fleetConfigCandidateAdapter adapts store.VehicleRepo to
// telemetry.FleetConfigCandidateLister, re-typing the store row into the
// telemetry one so internal/telemetry stays free of an internal/store import
// (the ridePollTargetAdapter precedent).
type fleetConfigCandidateAdapter struct {
	repo *store.VehicleRepo
}

func (a *fleetConfigCandidateAdapter) ListFleetConfigCandidates(
	ctx context.Context, cutoff time.Time, limit int,
) ([]telemetry.FleetConfigCandidate, error) {
	rows, err := a.repo.ListFleetConfigCandidates(ctx, cutoff, limit)
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.FleetConfigCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, telemetry.FleetConfigCandidate{
			VehicleID:   r.VehicleID,
			VIN:         r.VIN,
			UserID:      r.UserID,
			LastUpdated: r.LastUpdated,
		})
	}
	return out, nil
}
