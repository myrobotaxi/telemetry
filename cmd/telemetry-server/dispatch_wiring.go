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
//
// It RETURNS the reservation sweeper (MYR-556). The sweeper is not only a
// background loop any more: the owner's `dispatch-now` endpoint runs its
// claimed dispatch path synchronously, so the composition root has to carry the
// instance across to the HTTP routes. Nil when reservation dispatch could not be
// composed at all, which leaves that endpoint answering 500 — the same
// fail-closed reading every other unwired ride-request option gets.
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
) (*dispatch.ReservationSweeper, error) {
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
	// MYR-527 nav-apply close-loop: the CAR's own reported destination is the
	// confirmation `sent` never was. Wired on the server's root ctx so an
	// in-flight verification dies with the process instead of stalling
	// shutdown; bus carries the owner's "check the dash" seam.
	d = d.WithNavVerify(ctx, &navVerifyStoreAdapter{vehicles: vehicleRepo, rides: rideRepo}, bus)
	// MYR-539 multi-stop leg advance: a waypoint event for an intermediate stop
	// marks it completed, makes the next one current, and re-shares the car's
	// nav to it — through the same sequencer and the same verifier above.
	d = d.WithStopAdvance(&dispatchStopStoreAdapter{repo: rideRepo})
	if _, err := d.Subscribe(bus); err != nil {
		return nil, fmt.Errorf("subscribe nav dispatcher: %w", err)
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
	sweeper := startReservationSweeper(ctx, cfg, bus, activities, d, rideRepo, vehicleRepo, shareRepo, logger)

	logger.Info("nav-dispatch subscriber enabled",
		slog.Bool("dispatch_enabled", cfg.DispatchEnabled()),
		slog.Bool("reservation_dispatch_enabled", cfg.ReservationDispatchEnabled()),
		slog.Bool("signing_transport", transport.Enabled()),
		slog.Int("retry_max", dispatchRetryMax),
	)
	return sweeper, nil
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

// dispatchStopStoreAdapter adapts the MYR-539 leg-advance write onto
// dispatch.StopStore, translating the store's "already advanced" refusal into
// the dispatch sentinel so the dispatcher can no-op on it without coupling to
// internal/store — the same translation shape every other adapter here uses.
type dispatchStopStoreAdapter struct {
	repo *store.RideRequestRepo
}

func (a *dispatchStopStoreAdapter) AdvanceStopArrival(
	ctx context.Context, rideID, stopID string,
) (dispatch.StopAdvance, error) {
	adv, err := a.repo.AdvanceStopArrival(ctx, rideID, stopID)
	if err != nil {
		if errors.Is(err, store.ErrRideStopNotAdvanced) {
			return dispatch.StopAdvance{}, fmt.Errorf("advance ride stop: %w: %w", dispatch.ErrStopNotAdvanced, err)
		}
		return dispatch.StopAdvance{}, fmt.Errorf("advance ride stop: %w", err)
	}
	return dispatch.StopAdvance{
		NextStopID: adv.NextStopID,
		NextTarget: toEventRidePlace(adv.NextTarget),
	}, nil
}

// toEventRidePlace projects a store place onto the events shape, address
// flattened to "" when absent — the same projection the accept path and the
// reservation sweeper perform, so every caller hands the leg pipeline an
// identically shaped place.
func toEventRidePlace(p store.RidePlace) events.RidePlace {
	out := events.RidePlace{Latitude: p.Latitude, Longitude: p.Longitude, Label: p.Label}
	if p.Address != nil {
		out.Address = *p.Address
	}
	return out
}

// navVerifyStoreAdapter adapts the two lean reads the MYR-527 nav-apply
// verifier needs onto dispatch.NavVerifyStore. Same cross-repo seam shape as
// reservationStoreAdapter, for the same dependency-rule reason.
type navVerifyStoreAdapter struct {
	vehicles *store.VehicleRepo
	rides    *store.RideRequestRepo
}

func (a *navVerifyStoreAdapter) VehicleNavDestination(
	ctx context.Context,
	vehicleID string,
) (lat, lng *float64, err error) {
	return a.vehicles.VehicleNavDestination(ctx, vehicleID)
}

func (a *navVerifyStoreAdapter) RideStatus(ctx context.Context, rideID string) (string, error) {
	return a.rides.RideStatus(ctx, rideID)
}
