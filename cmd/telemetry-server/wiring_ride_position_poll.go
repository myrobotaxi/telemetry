package main

// MYR-394 ride-position poll wiring.
//
// While a vehicle is carrying out a ride and is NOT streaming (in service,
// asleep, offline) nothing asks Tesla where it is, so the rider-tracking map
// renders the last fix from before the car went quiet — observed 1.5 miles from
// where the car actually sat. This wires the loop that asks, bounded to the
// ride's lifetime.
//
// It adds no Tesla client of its own. The backfill publisher AND the
// stream-recency reader are BOTH the ServiceStatusMonitor already built by
// setupServiceStatusMonitor — the same reuse wiring_vehicle_refresh.go makes
// for MYR-266 — which is what keeps this a new trigger rather than a second
// REST read path, and what makes the never-wake guarantee structural: the
// monitor holds no waker.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// ridePollReconcileTimeout bounds the ONE synchronous startup reconcile. Short
// on purpose: adopting the open rides is worth waiting for (a restart mid-ride
// must not leave the rider staring at a frozen marker), but not worth delaying
// the whole boot over — a pass that times out is retried by the reconcile loop
// within minutes.
const ridePollReconcileTimeout = 20 * time.Second

// setupRidePositionPoller builds the poller, subscribes it to the ride seams,
// runs the startup reconcile, and starts the safety-net reconcile loop.
//
// Order is the MYR-176 sequence from setupNavDispatcher, and each step depends
// on the previous one:
//
//  1. Start (subscribe) FIRST, so a ride accepted while step 2 is running is
//     not missed. Starting a poller twice is a no-op, so the overlap is safe in
//     the direction that matters.
//  2. Reconcile SYNCHRONOUSLY, so every ride that was live before this process
//     restarted is being polled again before the HTTP server starts answering
//     the requests that ask where those cars are — and so any poller left over
//     from a ride that ended while we were down is reaped rather than adopted.
//  3. The reconcile LOOP last, as the slow safety net for dropped bus events.
//
// A reconcile failure is logged and does NOT block startup: the loop will try
// again, and the event seams alone still cover every ride that starts from now
// on. Returns the poller so main can Stop it.
func setupRidePositionPoller(
	ctx context.Context,
	cfg *config.Config,
	bus events.Bus,
	monitor *telemetry.ServiceStatusMonitor,
	vinCache *store.VINCache,
	vehicleRepo *store.VehicleRepo,
	accountRepo *store.AccountRepo,
	rideRepo *store.RideRequestRepo,
	logger *slog.Logger,
) (*telemetry.RidePositionPoller, error) {
	log := logger.With(slog.String("component", "ride-position-poll"))

	poller := telemetry.NewRidePositionPoller(
		bus,
		telemetry.NewRidePositionPollerDeps(
			// Backfill AND stream recency are the same monitor: the MYR-260
			// read path and the MYR-300 freshness clock have to agree, and the
			// only way to guarantee that is for them to be one object.
			monitor,
			monitor,
			&ridePollVehicleResolverAdapter{repo: vehicleRepo},
			&vehicleOwnerAdapter{cache: vinCache},
			newTeslaTokenResolver(cfg, accountRepo, log),
			&ridePollTargetAdapter{repo: rideRepo},
		),
		telemetry.RidePositionPollConfig{
			Enabled:  cfg.RidePollEnabled(),
			Interval: cfg.RidePollInterval(),
		},
		log,
	)

	if err := poller.Start(ctx); err != nil {
		return nil, fmt.Errorf("starting ride position poller: %w", err)
	}

	reconcileCtx, cancel := context.WithTimeout(ctx, ridePollReconcileTimeout)
	adopted, reaped, err := poller.Reconcile(reconcileCtx)
	cancel()
	if err != nil {
		log.Warn("ride position poll: startup reconcile failed (non-fatal)",
			slog.String("error", err.Error()))
	} else {
		log.Info("ride position poll: startup reconcile complete",
			slog.Int("adopted", adopted), slog.Int("reaped", reaped))
	}

	go poller.RunReconcileLoop(ctx)

	return poller, nil
}

// ridePollVehicleResolverAdapter resolves a ride's vehicle cuid to its VIN.
//
// A separate type from dispatchVehicleResolverAdapter despite the identical
// body: that one translates store.ErrVehicleNotFound into a DISPATCH sentinel
// so the dispatcher can tell "never going to work" from "try again", and the
// poller has no such distinction to make — every failure is one skipped cycle.
// Sharing it would mean importing dispatch's error vocabulary into a component
// that has no use for it.
type ridePollVehicleResolverAdapter struct {
	repo *store.VehicleRepo
}

func (a *ridePollVehicleResolverAdapter) ResolveVIN(ctx context.Context, vehicleID string) (string, error) {
	v, err := a.repo.GetByID(ctx, vehicleID)
	if err != nil {
		return "", fmt.Errorf("resolve vin for ride position poll: %w", err)
	}
	return v.VIN, nil
}

// ridePollTargetAdapter adapts store.RideRequestRepo to
// telemetry.ActiveRideLister, re-typing the store's row struct into the
// telemetry one so internal/telemetry stays free of an internal/store import.
type ridePollTargetAdapter struct {
	repo *store.RideRequestRepo
}

func (a *ridePollTargetAdapter) ListActiveRideTargets(
	ctx context.Context, limit int,
) ([]telemetry.ActiveRideTarget, error) {
	rows, err := a.repo.ListActiveRidePollTargets(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("list active ride poll targets: %w", err)
	}
	targets := make([]telemetry.ActiveRideTarget, 0, len(rows))
	for _, r := range rows {
		targets = append(targets, telemetry.ActiveRideTarget{
			RideRequestID: r.RideRequestID,
			VehicleID:     r.VehicleID,
			VIN:           r.VIN,
		})
	}
	return targets, nil
}
