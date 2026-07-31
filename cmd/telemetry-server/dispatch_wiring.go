package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/dispatch"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// Nav-dispatch wiring (MYR-176). Composes the ride.accepted subscriber that
// pushes the pickup into the vehicle's Tesla navigation. Lives at cmd/ (not
// inside internal/dispatch) so the dispatcher depends only on small
// consumer-site interfaces, wired here to store + telemetry + commands — the
// same dependency-rule boundary the vehicle_deleted dispatcher follows.

// dispatchRetryMax / dispatchRetryBackoff are the bounded retry policy for a
// transient (transport / asleep-after-wake) nav push.
const (
	dispatchRetryMax     = 2
	dispatchRetryBackoff = 2 * time.Second
	// dispatchReconcileAge is the minimum claim age for the startup reconciler
	// to treat a claimed-but-unresolved dispatch as interrupted. It sits
	// comfortably above the dispatcher's default OverallTimeout (2m) so a
	// genuinely in-flight dispatch is never mistaken for an orphan.
	dispatchReconcileAge = 5 * time.Minute
	// dispatchReconcileTimeout bounds the one-shot startup reconciliation pass.
	dispatchReconcileTimeout = 30 * time.Second
)

// setupNavDispatcher builds the command executor over the tesla-http-proxy
// (the same sidecar the vehicle-command endpoint uses), the owner-token
// resolver, and the store adapter, then subscribes the dispatcher on the
// ride.accepted seam. The subscription lives until bus.Close on shutdown.
func setupNavDispatcher(
	ctx context.Context,
	cfg *config.Config,
	bus events.Bus,
	activities dispatch.RideActivityEnder,
	vehicleRepo *store.VehicleRepo,
	accountRepo *store.AccountRepo,
	rideRepo *store.RideRequestRepo,
	shareRepo *store.VehicleShareRepo,
	logger *slog.Logger,
) error {
	transport := newCommandTransport(cfg.Proxy().URL, cfg.Proxy().FleetAPIBaseURL,
		logger.With(slog.String("component", "dispatch-transport")))
	executor := commands.NewExecutor(transport, logger.With(slog.String("component", "dispatch-executor")))

	var tokenOpts []telemetry.TeslaTokenResolverOption
	if cfg.TeslaOAuth().ClientID != "" {
		refresher := telemetry.NewTokenRefresher(telemetry.TeslaOAuthConfig{
			ClientID:     cfg.TeslaOAuth().ClientID,
			ClientSecret: cfg.TeslaOAuth().ClientSecret,
		}, logger.With(slog.String("component", "dispatch-token-refresh")))
		tokenOpts = append(tokenOpts, telemetry.WithResolverRefresher(refresher, &teslaTokenUpdaterAdapter{repo: accountRepo}))
	}
	resolver := telemetry.NewTeslaTokenResolver(
		&teslaTokenAdapter{repo: accountRepo},
		logger.With(slog.String("component", "dispatch-token")),
		tokenOpts...,
	)

	d := dispatch.New(
		&dispatchVehicleResolverAdapter{repo: vehicleRepo},
		&dispatchTokenSourceAdapter{resolver: resolver},
		executor,
		&dispatchOutcomeStoreAdapter{repo: rideRepo},
		dispatch.Config{
			Enabled:    cfg.DispatchEnabled(),
			MaxRetries: dispatchRetryMax,
			Backoff:    dispatchRetryBackoff,
		},
		logger.With(slog.String("component", "nav-dispatch")),
	)
	if _, err := d.Subscribe(bus); err != nil {
		return fmt.Errorf("subscribe nav dispatcher: %w", err)
	}

	// Startup reconciliation: resolve any dispatch orphaned by a crash/SIGTERM
	// in the claim→record window (dispatched_at set, dispatch_status NULL). We
	// log-and-continue on error rather than blocking startup — a reconcile
	// failure must not stop the server from serving.
	reconcileCtx, cancel := context.WithTimeout(ctx, dispatchReconcileTimeout)
	defer cancel()
	reconcileAdapter := &dispatchOutcomeStoreAdapter{repo: rideRepo}
	if n, err := d.Reconcile(reconcileCtx, reconcileAdapter, dispatchReconcileAge); err != nil {
		logger.Error("nav-dispatch startup reconciliation failed", slog.String("leg", "pickup"), slog.String("error", err.Error()))
	} else if n > 0 {
		logger.Info("nav-dispatch startup reconciliation resolved interrupted dispatches", slog.String("leg", "pickup"), slog.Int("count", n))
	}
	// Leg-2 (dropoff) startup reconciliation (MYR-266): symmetric with leg 1 —
	// resolve any dropoff push orphaned by a crash/SIGTERM in the
	// claim→record window (dropoff_dispatched_at set, dropoff_dispatch_status
	// NULL). Log-and-continue on error so a reconcile miss never blocks startup.
	if n, err := d.ReconcileDropoff(reconcileCtx, reconcileAdapter, dispatchReconcileAge); err != nil {
		logger.Error("nav-dispatch startup reconciliation failed", slog.String("leg", "dropoff"), slog.String("error", err.Error()))
	} else if n > 0 {
		logger.Info("nav-dispatch startup reconciliation resolved interrupted dispatches", slog.String("leg", "dropoff"), slog.Int("count", n))
	}

	// Reservation-time dispatch (MYR-179): a SCHEDULED ride's accept does NOT
	// push nav (see dispatch.Dispatcher.process); this sweeper fires its
	// pickup at `scheduledFor` instead, claiming the SAME leg-1 latch. Started
	// AFTER the reconciliation above so orphaned claims from the previous
	// process are resolved before new ones are made.
	startReservationSweeper(ctx, cfg, bus, activities, d, rideRepo, vehicleRepo, shareRepo, logger)

	logger.Info("nav-dispatch subscriber enabled",
		slog.Bool("dispatch_enabled", cfg.DispatchEnabled()),
		slog.Bool("reservation_dispatch_enabled", cfg.ReservationDispatchEnabled()),
		slog.Bool("signing_transport", transport.Enabled()),
		slog.Int("retry_max", dispatchRetryMax),
	)
	return nil
}

// dispatchVehicleResolverAdapter resolves a vehicle cuid to its VIN over the
// vehicle repo (dispatch.VehicleResolver).
type dispatchVehicleResolverAdapter struct {
	repo *store.VehicleRepo
}

func (a *dispatchVehicleResolverAdapter) ResolveVIN(ctx context.Context, vehicleID string) (string, error) {
	v, err := a.repo.GetByID(ctx, vehicleID)
	if err != nil {
		// Translate the store's permanent not-found into the dispatch
		// sentinel so the dispatcher short-circuits (no retry); anything else
		// (DB hiccup) stays transient and is retried under the bounded policy.
		if errors.Is(err, store.ErrVehicleNotFound) {
			return "", fmt.Errorf("resolve vin for dispatch: %w: %w", dispatch.ErrVehicleNotFound, err)
		}
		return "", fmt.Errorf("resolve vin for dispatch: %w", err)
	}
	return v.VIN, nil
}

// dispatchTokenSourceAdapter resolves the owner's Tesla access token over the
// shared TeslaTokenResolver (dispatch.TokenSource).
type dispatchTokenSourceAdapter struct {
	resolver *telemetry.TeslaTokenResolver
}

func (a *dispatchTokenSourceAdapter) ResolveToken(ctx context.Context, userID string) (string, error) {
	tok, err := a.resolver.Resolve(ctx, userID)
	if err != nil {
		// Translate the resolver's permanent conditions into the dispatch
		// sentinels so the dispatcher (a) does not retry them and (b) records
		// DISTINCT outcome codes: token_expired (must re-link) vs
		// token_unavailable (never linked). Preserve the concrete cause via
		// multi-%w for logs. Any other error stays transient (retried).
		switch {
		case errors.Is(err, telemetry.ErrTeslaTokenExpired):
			return "", fmt.Errorf("resolve tesla token for dispatch: %w: %w", dispatch.ErrTokenExpired, err)
		case errors.Is(err, telemetry.ErrTeslaTokenUnavailable):
			return "", fmt.Errorf("resolve tesla token for dispatch: %w: %w", dispatch.ErrTokenUnavailable, err)
		default:
			return "", fmt.Errorf("resolve tesla token for dispatch: %w", err)
		}
	}
	return tok.AccessToken, nil
}

// dispatchOutcomeStoreAdapter adapts the ride-request repo to
// dispatch.OutcomeStore, translating the dispatch.Outcome string into the
// store's DispatchStatus enum.
type dispatchOutcomeStoreAdapter struct {
	repo *store.RideRequestRepo
}

func (a *dispatchOutcomeStoreAdapter) ClaimDispatch(ctx context.Context, rideID string) (bool, error) {
	return a.repo.ClaimDispatch(ctx, rideID)
}

func (a *dispatchOutcomeStoreAdapter) RecordDispatchOutcome(ctx context.Context, rideID string, status dispatch.Outcome, errCode *string) error {
	return a.repo.RecordDispatchOutcome(ctx, rideID, store.DispatchStatus(status), errCode)
}

// ClaimDropoffDispatch / RecordDropoffDispatchOutcome are the leg-2 (dropoff)
// seams (MYR-265) — the same shape as the leg-1 pair, over the independent
// dropoff_* columns so neither leg clobbers the other's dispatch history.
func (a *dispatchOutcomeStoreAdapter) ClaimDropoffDispatch(ctx context.Context, rideID string) (bool, error) {
	return a.repo.ClaimDropoffDispatch(ctx, rideID)
}

func (a *dispatchOutcomeStoreAdapter) RecordDropoffDispatchOutcome(ctx context.Context, rideID string, status dispatch.Outcome, errCode *string) error {
	return a.repo.RecordDropoffDispatchOutcome(ctx, rideID, store.DispatchStatus(status), errCode)
}

// ListInterruptedDispatches satisfies dispatch.InterruptedDispatchLister for
// the startup reconciler.
func (a *dispatchOutcomeStoreAdapter) ListInterruptedDispatches(ctx context.Context, olderThan time.Duration) ([]string, error) {
	return a.repo.ListInterruptedDispatches(ctx, olderThan)
}

// ListInterruptedDropoffDispatches satisfies
// dispatch.InterruptedDropoffDispatchLister for the leg-2 startup reconciler
// (MYR-266).
func (a *dispatchOutcomeStoreAdapter) ListInterruptedDropoffDispatches(ctx context.Context, olderThan time.Duration) ([]string, error) {
	return a.repo.ListInterruptedDropoffDispatches(ctx, olderThan)
}
