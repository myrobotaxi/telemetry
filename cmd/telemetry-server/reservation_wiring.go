package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/dispatch"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// Reservation-time dispatch wiring (MYR-179). Composes the sweeper that fires
// a SCHEDULED ride's pickup nav at its `scheduledFor` instant — the accept
// having deliberately deferred it. Lives at cmd/ (not inside
// internal/dispatch) for the same dependency-rule reason as the dispatcher:
// the sweeper depends only on a small consumer-site store interface, adapted
// here onto the ride-request repo.

const (
	// reservationSweepInterval is the sweep cadence. It bounds how late a
	// punctual reservation dispatch can be: a ride due just after a tick waits
	// at most one interval. 30s is well inside the tolerance of a booking made
	// hours or days ahead while costing one indexed query per tick.
	reservationSweepInterval = 30 * time.Second
	// reservationMaxLateness is the lateness ceiling: how far past
	// `scheduledFor` a due reservation may still be dispatched before it is
	// failed honestly instead. See dispatch.ReservationConfig.MaxLateness.
	reservationMaxLateness = 30 * time.Minute
	// reservationSweepTimeout bounds the due-list QUERY so a database stall
	// cannot wedge the ticker. The per-reservation work it starts is NOT
	// bounded by it — each worker runs under the dispatcher's own
	// OverallTimeout, exactly like an instant dispatch.
	reservationSweepTimeout = 30 * time.Second
	// reservationMaxPerSweep caps one pass. Modest because claims are
	// just-in-time: an unreached candidate is re-selected on the next tick
	// rather than skipped, so a small window costs latency, never dispatches.
	reservationMaxPerSweep = 25
	// reservationMaxConcurrent is the sweeper's OWN worker budget, separate
	// from the dispatcher's instant-dispatch pool so a backlog of
	// asleep-vehicle reservation pushes can never delay an instant accept's
	// pickup push.
	reservationMaxConcurrent = 2
)

// startReservationSweeper builds the sweeper over the already-constructed
// dispatcher (whose leg-1 record seam and nav-push machinery it reuses) and
// runs it in its own goroutine until ctx cancels.
//
// It is started AFTER the leg-1 startup reconciliation in setupNavDispatcher:
// the reconciler must be allowed to resolve dispatches orphaned by the
// previous process's crash before the sweeper begins making new claims. The
// sweeper's own first pass waits one full interval, so the ordering holds even
// though both are asynchronous.
func startReservationSweeper(
	ctx context.Context,
	cfg *config.Config,
	bus events.Bus,
	dispatcher *dispatch.Dispatcher,
	rideRepo *store.RideRequestRepo,
	vehicleRepo *store.VehicleRepo,
	shareRepo *store.VehicleShareRepo,
	logger *slog.Logger,
) {
	sweeper := dispatch.NewReservationSweeper(
		dispatcher,
		&reservationStoreAdapter{repo: rideRepo, vehicles: vehicleRepo, shares: shareRepo},
		bus,
		dispatch.ReservationConfig{
			Enabled:       cfg.ReservationDispatchEnabled(),
			Interval:      reservationSweepInterval,
			MaxLateness:   reservationMaxLateness,
			SweepTimeout:  reservationSweepTimeout,
			MaxPerSweep:   reservationMaxPerSweep,
			MaxConcurrent: reservationMaxConcurrent,
		},
		logger.With(slog.String("component", "reservation-sweeper")),
	)
	go sweeper.Run(ctx)
}

// reservationStoreAdapter adapts the ride-request repo to
// dispatch.ReservationStore. It also performs the row → DueReservation
// projection, which is where the P1 pickup coordinates cross into the
// dispatch package (already decrypted by the repo's scan, exactly as the
// accept path's RideAcceptedEvent carries them).
type reservationStoreAdapter struct {
	repo *store.RideRequestRepo
	// vehicles serves the MYR-342 pause probe. The switch lives on the Go-owned
	// control-state side table, not on any ride row, so the sweeper's store seam
	// spans two repos — one question to each, both before the claim.
	vehicles *store.VehicleRepo
	// shares serves the MYR-369 rider-grant probe. Neither repo above owns
	// go_vehicle_shares, so the seam spans three — one question to each,
	// all before the claim.
	shares *store.VehicleShareRepo
}

func (a *reservationStoreAdapter) ListDueReservations(
	ctx context.Context,
	now, expiredBefore time.Time,
	limit int,
) ([]dispatch.DueReservation, error) {
	recs, err := a.repo.ListDueReservations(ctx, now, expiredBefore, limit)
	if err != nil {
		return nil, err
	}
	out := make([]dispatch.DueReservation, 0, len(recs))
	// Indexed rather than ranged by value: DueReservation is wide enough that
	// gocritic flags the per-iteration copy.
	for i := range recs {
		rec := &recs[i]
		pickup := events.RidePlace{
			Latitude:  rec.Pickup.Latitude,
			Longitude: rec.Pickup.Longitude,
			Label:     rec.Pickup.Label,
		}
		// Address flattened to "" when absent — the same projection the accept
		// path's toEventPlace performs, so both callers hand runClaimedLeg an
		// identically shaped place.
		if rec.Pickup.Address != nil {
			pickup.Address = *rec.Pickup.Address
		}
		out = append(out, dispatch.DueReservation{
			RideRequestID: rec.ID,
			VehicleID:     rec.VehicleID,
			RiderID:       rec.RiderID,
			OwnerID:       rec.OwnerID,
			Pickup:        pickup,
			ScheduledFor:  rec.ScheduledFor,
		})
	}
	return out, nil
}

func (a *reservationStoreAdapter) VehicleHasActiveInstantRide(ctx context.Context, vehicleID string) (bool, error) {
	return a.repo.VehicleHasActiveInstantRide(ctx, vehicleID)
}

// VehicleRideShareEnabled reads the owner's ride-sharing switch (MYR-342).
//
// It crosses to the VEHICLE repo rather than the ride-request one, because the
// flag lives on the Go-owned go_vehicle_control_state side table next to the
// service window, not on a ride row. That is why this adapter now holds two
// repos: the sweeper asks one question of each, both immediately before the
// irreversible claim.
func (a *reservationStoreAdapter) VehicleRideShareEnabled(ctx context.Context, vehicleID string) (bool, error) {
	return a.vehicles.RideShareEnabled(ctx, vehicleID)
}

// RiderMayRequestRides reads the rider's ride capability over the vehicle
// (MYR-369).
//
// A THIRD repo on this adapter, and for the same reason as the second: the fact
// lives on go_vehicle_shares, which neither the ride-request repo nor the
// vehicle repo owns. The sweeper now asks one question of each, all three
// immediately before the irreversible claim.
func (a *reservationStoreAdapter) RiderMayRequestRides(ctx context.Context, riderID, vehicleID string) (bool, error) {
	return a.shares.RiderMayRequestRides(ctx, riderID, vehicleID)
}

func (a *reservationStoreAdapter) ClaimReservationDispatch(ctx context.Context, rideID string) (bool, error) {
	return a.repo.ClaimReservationDispatch(ctx, rideID)
}
