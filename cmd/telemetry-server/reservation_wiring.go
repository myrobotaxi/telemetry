package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/dispatch"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
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
	// Dispatch-lead policy (MYR-535, client decision 2026-08-12):
	// `scheduledFor` is the pickup ARRIVAL instant, so the car is dispatched
	// early by the estimated drive time from its current position plus a
	// buffer, clamped to [floor, cap]. The cap doubles as the sweep's SELECT
	// horizon.
	reservationLeadBuffer = 3 * time.Minute
	reservationLeadFloor  = 5 * time.Minute
	reservationLeadCap    = 30 * time.Minute
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
	activities dispatch.RideActivityEnder,
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
			LeadBuffer:    reservationLeadBuffer,
			LeadFloor:     reservationLeadFloor,
			LeadCap:       reservationLeadCap,
		},
		logger.With(slog.String("component", "reservation-sweeper")),
	).WithActivityEnder(activities)
	go sweeper.Run(ctx)
}

// reservationStoreAdapter adapts the ride-request repo to
// dispatch.ReservationStore. It also performs the row → DueReservation
// projection, which is where the P1 pickup coordinates cross into the
// dispatch package (already decrypted by the repo's scan, exactly as the
// accept path's RideAcceptedEvent carries them).
type reservationStoreAdapter struct {
	repo *store.RideRequestRepo
	// vehicles serves the combined MYR-342 pause / MYR-372 service probe.
	// Neither fact lives on a ride row — the pause switch is on the Go-owned
	// control-state side table and the status is on the Prisma "Vehicle" row —
	// so the sweeper's store seam spans repos. The two are read together in one
	// joined statement, so the sweeper asks this repo a single question.
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
			// MYR-555: threaded through so the dispatch frame states the ride's
			// CURRENT trip version rather than the wire's zero.
			TripVersion: rec.TripVersion,
		})
	}
	return out, nil
}

func (a *reservationStoreAdapter) VehicleHasActiveInstantRide(ctx context.Context, vehicleID string) (bool, error) {
	return a.repo.VehicleHasActiveInstantRide(ctx, vehicleID)
}

// VehiclePosition serves the MYR-535 dispatch-lead estimate: the car's
// freshest decrypted position, or a nil pair when the server holds none.
// Delegates to the vehicle repo's lean two-column read — never the wide
// snapshot — because the sweeper re-asks every tick for every candidate
// inside the lead window.
func (a *reservationStoreAdapter) VehiclePosition(
	ctx context.Context,
	vehicleID string,
) (lat, lng *float64, err error) {
	return a.vehicles.VehiclePosition(ctx, vehicleID)
}

// VehicleDispatchState answers the MYR-372 service re-check and the MYR-342
// pause check together, from the repo's single joined read.
//
// This adapter method is the WHOLE POINT of putting the status question here
// rather than in internal/dispatch: it hands the raw status to
// telemetry.VehicleStatusIsInService — one arm of the very switch the
// owner-accept gate's 409 is built from. cmd/ is the only layer that can see
// both internal/store and internal/telemetry, so it is the only place the two
// surfaces can share one enumeration instead of keeping two copies that drift.
//
// ONLY THE IN-SERVICE ARM CROSSES OVER. Accept also refuses `offline`; the
// sweeper must not, because `offline` is the "Vehicle" schema default that
// nothing in this server ever writes back, so holding on it would strand every
// not-yet-streaming car until its reservation expired — while the nav push's
// own wake ladder is exactly what would have cleared it. The full argument is
// at holdIfVehicleBlocked in internal/dispatch; do not "restore" the symmetry
// here without reading it.
//
// A vehicle that has vanished surfaces as ErrVehicleNotFound from the repo, and
// the sweeper holds on any error — the right answer for a car nobody can look
// up, and one the lateness ceiling resolves within the window.
func (a *reservationStoreAdapter) VehicleDispatchState(
	ctx context.Context,
	vehicleID string,
) (dispatch.VehicleDispatchState, error) {
	status, rideShareEnabled, err := a.vehicles.GetDispatchState(ctx, vehicleID)
	if err != nil {
		return dispatch.VehicleDispatchState{}, err
	}
	return dispatch.VehicleDispatchState{
		InService:        telemetry.VehicleStatusIsInService(string(status)),
		RideShareEnabled: rideShareEnabled,
	}, nil
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

// ListLapsedDispatches projects the SECOND ceiling pass (MYR-555): reservations
// that were dispatched and never became a ride.
//
// The projection is trivial because the row is — three columns, no place, no
// rider, no decrypt. That is the point of keeping it a separate seam from the
// due list: this pass pushes nothing anywhere, so it should not pay for the
// pickup coordinates the due pass exists to deliver.
func (a *reservationStoreAdapter) ListLapsedDispatches(
	ctx context.Context,
	lapsedBefore time.Time,
	limit int,
) ([]dispatch.LapsedReservation, error) {
	recs, err := a.repo.ListLapsedDispatches(ctx, lapsedBefore, limit)
	if err != nil {
		return nil, err
	}
	out := make([]dispatch.LapsedReservation, 0, len(recs))
	for i := range recs {
		out = append(out, dispatch.LapsedReservation{
			RideRequestID: recs[i].ID,
			VehicleID:     recs[i].VehicleID,
			ScheduledFor:  recs[i].ScheduledFor,
		})
	}
	return out, nil
}

func (a *reservationStoreAdapter) ExpireDispatchedReservation(ctx context.Context, rideID string) (bool, error) {
	return a.repo.ExpireDispatchedReservation(ctx, rideID)
}
