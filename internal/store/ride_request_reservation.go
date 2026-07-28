// RideRequestRepo reservation-dispatch accessors (MYR-179). The two reads the
// reservation sweeper (internal/dispatch) needs to fire a scheduled ride's
// pickup nav at its `scheduledFor` instant: which reservations are due, and
// whether the target car is mid-ride.
//
// Both are READS. The sweeper's WRITES are the EXISTING MYR-176 leg-1 pair
// (ClaimDispatch / RecordDispatchOutcome in ride_request_dispatch.go) — the
// same exactly-once latch the instant path uses, which is what buys scheduled
// dispatch the identical exactly-once + restart-safe semantics for free.

package store

import (
	"context"
	"fmt"
	"time"
)

// defaultDueReservationLimit caps one sweep pass when the caller passes
// limit <= 0. A backlog larger than this drains over subsequent ticks
// (oldest reservation first), so a pile-up after downtime can never make one
// tick unbounded.
const defaultDueReservationLimit = 200

// ListDueReservations returns the accepted, still-unclaimed reservations whose
// `scheduled_for` is at or before now, oldest first. It is the sweeper's due
// query: see queryRideRequestListDue for why each conjunct is load-bearing.
//
// now is the SWEEPER's clock rather than the database's NOW() on purpose — the
// same instant then decides the busy-hold expiry in Go, so the two decisions
// can never straddle a clock skew, and tests can sit exactly on the boundary.
//
// Returns an empty (non-nil) slice when nothing is due; that is the steady
// state, not an error.
func (r *RideRequestRepo) ListDueReservations(ctx context.Context, now time.Time, limit int) ([]RideRequestRecord, error) {
	if limit <= 0 {
		limit = defaultDueReservationLimit
	}
	recs, err := r.list(ctx, "ride_request.list_due_reservations", queryRideRequestListDue, now, limit)
	if err != nil {
		return nil, fmt.Errorf("RideRequestRepo.ListDueReservations: %w", err)
	}
	return recs, nil
}

// VehicleHasActiveInstantRide reports whether the vehicle is currently
// committed to an active INSTANT ride (`accepted`/`arrived`/`enroute` with no
// `scheduled_for`) — the MYR-266 per-vehicle busy predicate.
//
// The reservation sweeper calls it BEFORE claiming: a car mid-ride must not
// have its navigation re-pointed at a new pickup, so a due reservation for a
// busy car is HELD (no claim, no outcome) and retried on the next tick.
//
// SCHEDULED rides are outside the predicate, so the due reservation being
// checked never reports itself busy.
func (r *RideRequestRepo) VehicleHasActiveInstantRide(ctx context.Context, vehicleID string) (bool, error) {
	start := time.Now()
	var busy bool
	err := r.pool.QueryRow(ctx, queryRideRequestVehicleBusy, vehicleID).Scan(&busy)
	r.metrics.ObserveQueryDuration("ride_request.vehicle_busy", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("ride_request.vehicle_busy")
		return false, fmt.Errorf("RideRequestRepo.VehicleHasActiveInstantRide(%s): %w", vehicleID, err)
	}
	return busy, nil
}
