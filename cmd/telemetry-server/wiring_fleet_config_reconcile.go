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
	"fmt"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupFleetConfigReconciler builds the reconciler and starts its background
// loop. It does NOT run a pass here.
//
// WHY NO SYNCHRONOUS BOOT PASS. A pass is up to MaxPerPass sequential
// third-party HTTP round-trips. This function is called before server.New /
// srv.Start, so anything it blocks on delays the bind of /healthz (which
// fly.toml health-checks every 10s with a 10s grace) and — the part that
// actually hurts — the Tesla mTLS listener on :8443, meaning real customer
// cars get connection-refused for the whole window on every single deploy.
// The loop takes its own first pass shortly after start instead, off the boot
// path (see telemetry.startupDelay).
//
// SAFETY INVARIANT (self-serve-onboarding.md §5): this component pushes config
// to a REAL car, so it is constructed only when both the signing proxy and the
// telemetry endpoint are configured. Absent either, it logs and stays off —
// the same runtime guard buildOwnerStreamHook uses to keep live pushes out of
// dev and test processes.
// It returns the reconciler, or nil when the safety guard keeps it off. The
// caller passes that value to the vehicle-command endpoint, where it is
// registered as the MYR-489 applied-signed-command observer — a nil reconciler
// simply leaves the executor unhooked.
func setupFleetConfigReconciler(
	ctx context.Context,
	cfg *config.Config,
	vehicleRepo *store.VehicleRepo,
	accountRepo *store.AccountRepo,
	logger *slog.Logger,
) *telemetry.FleetConfigReconciler {
	log := logger.With(slog.String("component", "fleet-config-reconcile"))

	if cfg.Proxy().URL == "" || cfg.Proxy().FleetTelemetryHostname == "" {
		log.Warn("fleet-config reconciler disabled: proxy/telemetry endpoint not configured — " +
			"a car whose config push was skipped pre-pairing will NOT self-heal")
		return nil
	}

	reader := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL: cfg.Proxy().FleetAPIBaseURL, // empty => default NA Fleet API
	}, log.With(slog.String("subcomponent", "fleet-read")))

	writer := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL:    cfg.Proxy().URL,
		HTTPClient: proxyHTTPClient(cfg.Proxy().URL, log),
	}, log.With(slog.String("subcomponent", "fleet-push")))

	adapter := &fleetConfigCandidateAdapter{repo: vehicleRepo}
	reconciler := telemetry.NewFleetConfigReconciler(
		telemetry.FleetConfigReconcilerDeps{
			Candidates: adapter,
			Attempts:   vehicleRepo,
			Reader:     reader,
			Writer:     writer,
			Tokens:     newTeslaTokenResolver(cfg, accountRepo, log),
			Pairing:    adapter,
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

	go reconciler.RunReconcileLoop(ctx)
	return reconciler
}

// fleetConfigCandidateAdapter adapts store.VehicleRepo to
// telemetry.FleetConfigCandidateLister, re-typing the store row into the
// telemetry one so internal/telemetry stays free of an internal/store import
// (the ridePollTargetAdapter precedent).
type fleetConfigCandidateAdapter struct {
	repo *store.VehicleRepo
}

func (a *fleetConfigCandidateAdapter) ListFleetConfigCandidates(
	ctx context.Context, cutoff, now, hotSince time.Time, limit int,
) ([]telemetry.FleetConfigCandidate, error) {
	rows, err := a.repo.ListFleetConfigCandidates(ctx, cutoff, now, hotSince, limit)
	if err != nil {
		return nil, fmt.Errorf("list fleet-config candidates: %w", err)
	}
	out := make([]telemetry.FleetConfigCandidate, 0, len(rows))
	for i := range rows {
		out = append(out, toTelemetryFleetConfigCandidate(&rows[i]))
	}
	return out, nil
}

// ResetFleetConfigScheduleOnPairing adapts the MYR-489 pairing-epoch write the
// same way — the reconciler learns that a signed command applied to a VIN and
// needs the resulting candidate back, in its own type.
func (a *fleetConfigCandidateAdapter) ResetFleetConfigScheduleOnPairing(
	ctx context.Context, vin string, now, debounceBefore time.Time,
) (telemetry.FleetConfigCandidate, bool, error) {
	row, found, err := a.repo.ResetFleetConfigScheduleOnPairing(ctx, vin, now, debounceBefore)
	if err != nil {
		return telemetry.FleetConfigCandidate{}, false, fmt.Errorf("reset fleet-config schedule on pairing: %w", err)
	}
	if !found {
		return telemetry.FleetConfigCandidate{}, false, nil
	}
	return toTelemetryFleetConfigCandidate(&row), true, nil
}

func toTelemetryFleetConfigCandidate(r *store.FleetConfigCandidate) telemetry.FleetConfigCandidate {
	return telemetry.FleetConfigCandidate{
		VehicleID:       r.VehicleID,
		VIN:             r.VIN,
		UserID:          r.UserID,
		LastUpdated:     r.LastUpdated,
		Status:          r.Status,
		AttemptCount:    r.AttemptCount,
		LastOutcome:     r.LastOutcome,
		LastAttemptAt:   r.LastAttemptAt,
		SignedCommandAt: r.SignedCommandAt,
		ForcedRepushAt:  r.ForcedRepushAt,
		ScheduleCreated: r.ScheduleCreated,
	}
}
