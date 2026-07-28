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
// the sweeper depends only on a small consumer-site read interface, adapted
// here onto the ride-request repo.

const (
	// reservationSweepInterval is the sweep cadence. It bounds how late a
	// punctual reservation dispatch can be: a ride due just after a tick waits
	// at most one interval. 30s is well inside the tolerance of a booking made
	// hours or days ahead while costing one indexed query per tick.
	reservationSweepInterval = 30 * time.Second
	// reservationBusyHold is how long past `scheduledFor` a due reservation
	// keeps waiting for a vehicle that is mid-ride before it is failed
	// honestly. See dispatch.ReservationConfig.BusyHold.
	reservationBusyHold = 30 * time.Minute
	// reservationSweepTimeout bounds one sweep pass (list + per-row busy check
	// and claim) so a database stall cannot wedge the ticker. The nav pushes
	// it starts are NOT bounded by it — they run on the dispatcher's own
	// OverallTimeout, off the sweep goroutine.
	reservationSweepTimeout = 30 * time.Second
)

// startReservationSweeper builds the sweeper over the already-constructed
// dispatcher (whose leg-1 claim/record seams and nav-push machinery it reuses)
// and runs it in its own goroutine until ctx cancels.
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
	logger *slog.Logger,
) {
	sweeper := dispatch.NewReservationSweeper(
		dispatcher,
		&reservationStoreAdapter{repo: rideRepo},
		bus,
		dispatch.ReservationConfig{
			Enabled:      cfg.ReservationDispatchEnabled(),
			Interval:     reservationSweepInterval,
			BusyHold:     reservationBusyHold,
			SweepTimeout: reservationSweepTimeout,
		},
		logger.With(slog.String("component", "reservation-sweeper")),
	)
	go sweeper.Run(ctx)
}

// reservationStoreAdapter adapts the ride-request repo to
// dispatch.ReservationStore. It also performs the record → DueReservation
// projection, which is where the P1 pickup coordinates cross into the
// dispatch package (already decrypted by the repo's scan, exactly as the
// accept path's RideAcceptedEvent carries them).
type reservationStoreAdapter struct {
	repo *store.RideRequestRepo
}

func (a *reservationStoreAdapter) ListDueReservations(ctx context.Context, now time.Time, limit int) ([]dispatch.DueReservation, error) {
	recs, err := a.repo.ListDueReservations(ctx, now, limit)
	if err != nil {
		return nil, err
	}
	out := make([]dispatch.DueReservation, 0, len(recs))
	for _, rec := range recs {
		if rec.ScheduledFor == nil {
			// Unreachable: the due query requires scheduled_for IS NOT NULL.
			// Skipped rather than dereferenced so a future query change can
			// never turn a projection bug into a nil-pointer panic in the
			// dispatch loop.
			continue
		}
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
			ScheduledFor:  *rec.ScheduledFor,
		})
	}
	return out, nil
}

func (a *reservationStoreAdapter) VehicleHasActiveInstantRide(ctx context.Context, vehicleID string) (bool, error) {
	return a.repo.VehicleHasActiveInstantRide(ctx, vehicleID)
}
