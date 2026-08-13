// RideRequestRepo reservation-dispatch accessors (MYR-179). What the
// reservation sweeper (internal/dispatch) needs to fire a scheduled ride's
// pickup nav at its `scheduledFor` instant: which reservations are due,
// whether the target car is mid-ride, and the reservation-side leg-1 claim.
//
// The claim is deliberately its OWN statement rather than the instant path's
// ClaimDispatch: it stamps the same dispatched_at latch (so exactly-once holds
// across both paths) but additionally re-validates status at claim time. The
// outcome WRITE is still the existing MYR-176 RecordDispatchOutcome, so a
// reservation resolves into the very same three columns an instant ride does.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// defaultDueReservationLimit caps one sweep pass when the caller passes
// limit <= 0. A backlog larger than this drains over subsequent ticks (oldest
// reservation first). Since MYR-179's review round the claim is just-in-time —
// a candidate this pass does not reach is simply re-selected next tick, never
// lost — so the cap bounds work per tick rather than rationing dispatches.
const defaultDueReservationLimit = 25

// DueReservation is the LEAN due-sweep projection (see rideRequestDueColumns):
// the ids, the decrypted PICKUP place, and the reservation instant. It is
// everything the sweeper needs to decide and push, and nothing else — no
// dropoff, no passenger contact, no requester identity.
//
// ScheduledFor is a value, not a pointer: the due query requires
// `scheduled_for IS NOT NULL`, so an absent instant is impossible here (unlike
// RideRequestRecord, which models every ride).
type DueReservation struct {
	ID           string
	RiderID      string
	OwnerID      string
	VehicleID    string
	Pickup       RidePlace
	ScheduledFor time.Time
	// TripVersion is the ride's CURRENT trip-shape version (MYR-555). One plain
	// integer on the row — no decrypt, no subselect — carried so the dispatch's
	// `ride_status_changed` frame can state the version instead of a zero
	// (MYR-548's carry rule).
	TripVersion int
}

// LapsedReservation is the LAPSED-dispatch projection (see
// rideRequestLapsedColumns): the ride, its car, and the instant the ceiling is
// measured from. Nothing on this path is pushed anywhere, so it carries no
// place, no rider and no cryptography at all — three columns is the whole row.
type LapsedReservation struct {
	ID           string
	VehicleID    string
	ScheduledFor time.Time
}

// ListDueReservations returns the accepted, still-unclaimed reservations whose
// `scheduled_for` is at or before dueBefore and which are ACTIONABLE this pass
// — either their vehicle is free, or they are already past expiredBefore and
// must be resolved regardless. Oldest first. See queryRideRequestListDue for
// why each conjunct is load-bearing.
//
// dueBefore is the sweeper's DISPATCH HORIZON, not its clock (MYR-535):
// `scheduledFor` is the instant the car should ARRIVE at the pickup, so the
// sweeper selects candidates up to its maximum dispatch lead EARLY and decides
// the exact leave-time per row in Go — the coordinates it needs for that are
// encrypted at rest, so the lead cannot be computed in SQL. Both instants
// derive from the SWEEPER's clock rather than the database's NOW() on purpose:
// the same clock then decides the per-row lead gate and the lateness deadline,
// so no decision can straddle a clock skew, and tests can sit exactly on the
// boundary. expiredBefore is `now - MaxLateness`.
//
// Returns an empty (non-nil) slice when nothing is due; that is the steady
// state, not an error.
func (r *RideRequestRepo) ListDueReservations(
	ctx context.Context,
	dueBefore, expiredBefore time.Time,
	limit int,
) ([]DueReservation, error) {
	if limit <= 0 {
		limit = defaultDueReservationLimit
	}
	const op = "ride_request.list_due_reservations"
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryRideRequestListDue, dueBefore, expiredBefore, limit)
	if err != nil {
		r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("RideRequestRepo.ListDueReservations: query: %w", err)
	}
	defer rows.Close()

	out := make([]DueReservation, 0)
	for rows.Next() {
		rec, scanErr := r.scanDueReservation(pgx.Row(rows))
		if scanErr != nil {
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("RideRequestRepo.ListDueReservations: %w", scanErr)
		}
		out = append(out, rec)
	}
	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("RideRequestRepo.ListDueReservations: rows: %w", err)
	}
	return out, nil
}

// scanDueReservation scans one rideRequestDueColumns row and decrypts the two
// PICKUP coordinate ciphertexts. Only two decrypts per row (the wide record
// scan does four) because the sweeper never touches the dropoff.
func (r *RideRequestRepo) scanDueReservation(row pgx.Row) (DueReservation, error) {
	var (
		rec          DueReservation
		pickupLatEnc string
		pickupLngEnc string
	)
	err := row.Scan(
		&rec.ID, &rec.RiderID, &rec.OwnerID, &rec.VehicleID,
		&pickupLatEnc, &pickupLngEnc, &rec.Pickup.Label, &rec.Pickup.Address,
		&rec.ScheduledFor, &rec.TripVersion,
	)
	if err != nil {
		return DueReservation{}, fmt.Errorf("scan due reservation: %w", err)
	}
	if rec.Pickup.Latitude, err = r.decryptCoord("pickup_lat_enc", pickupLatEnc); err != nil {
		return DueReservation{}, err
	}
	if rec.Pickup.Longitude, err = r.decryptCoord("pickup_lng_enc", pickupLngEnc); err != nil {
		return DueReservation{}, err
	}
	return rec, nil
}

// ClaimReservationDispatch is the reservation-side leg-1 claim: it stamps the
// SAME dispatched_at latch as ClaimDispatch, but only while the ride is still
// `accepted` and still a reservation (see
// queryRideRequestClaimReservationDispatch).
//
// Returns claimed=true when THIS call won. claimed=false means the row was
// already claimed, was cancelled/advanced since the sweep listed it, or is
// gone — all ordinary "not ours to dispatch" outcomes, never an error.
func (r *RideRequestRepo) ClaimReservationDispatch(ctx context.Context, id string) (bool, error) {
	start := time.Now()
	var claimedID string
	err := r.pool.QueryRow(ctx, queryRideRequestClaimReservationDispatch, id).Scan(&claimedID)
	r.metrics.ObserveQueryDuration("ride_request.claim_reservation_dispatch", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		r.metrics.IncQueryError("ride_request.claim_reservation_dispatch")
		return false, fmt.Errorf("RideRequestRepo.ClaimReservationDispatch(%s): %w", id, err)
	}
	return true, nil
}

// VehicleHasActiveInstantRide reports whether the vehicle is currently
// committed to an active INSTANT ride (`accepted`/`arrived`/`enroute` with no
// `scheduled_for`) — the MYR-266 per-vehicle busy predicate.
//
// The reservation sweeper calls it in the worker IMMEDIATELY BEFORE the claim:
// a car mid-ride must not have its navigation re-pointed at a new pickup, so a
// due reservation for a busy car is HELD (no claim, no outcome) and retried on
// the next tick. Running it adjacent to the claim rather than at pass start is
// what keeps the check-to-push window at seconds instead of minutes.
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

// ListLapsedDispatches returns the accepted reservations that WERE dispatched
// and never became a ride: `dispatched_at` set, status still `accepted`,
// `scheduled_for` at or before lapsedBefore, and the ceiling verdict not yet
// recorded. Oldest first. See queryRideRequestListLapsedDispatches for why each
// conjunct is load-bearing.
//
// lapsedBefore is `now - MaxLateness`, derived from the SWEEPER's clock exactly
// as ListDueReservations' bounds are, so one clock governs both passes of a
// tick and the boundary is exactly testable.
//
// Returns an empty (non-nil) slice when nothing has lapsed; that is the steady
// state, not an error.
func (r *RideRequestRepo) ListLapsedDispatches(
	ctx context.Context,
	lapsedBefore time.Time,
	limit int,
) ([]LapsedReservation, error) {
	if limit <= 0 {
		limit = defaultDueReservationLimit
	}
	const op = "ride_request.list_lapsed_dispatches"
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryRideRequestListLapsedDispatches, lapsedBefore, limit)
	if err != nil {
		r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("RideRequestRepo.ListLapsedDispatches: query: %w", err)
	}
	defer rows.Close()

	out := make([]LapsedReservation, 0)
	for rows.Next() {
		var rec LapsedReservation
		if scanErr := rows.Scan(&rec.ID, &rec.VehicleID, &rec.ScheduledFor); scanErr != nil {
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("RideRequestRepo.ListLapsedDispatches: scan: %w", scanErr)
		}
		out = append(out, rec)
	}
	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("RideRequestRepo.ListLapsedDispatches: rows: %w", err)
	}
	return out, nil
}

// ExpireDispatchedReservation records the lateness-ceiling verdict on a
// reservation whose pickup nav was ALREADY dispatched: `failed` /
// `reservation_expired`, with the ride row left at `accepted` exactly as the
// never-dispatched arm leaves it.
//
// It is its own one-winner latch, because the usual one cannot serve: every row
// in this set already has `dispatched_at` stamped, so ClaimReservationDispatch
// can never win here. The guarantee lives in the statement's own predicate —
// see queryRideRequestExpireDispatchedReservation.
//
// Returns expired=true when THIS call recorded the verdict. false means the row
// already carried it, or moved out of `accepted` since the list — both ordinary
// outcomes, never an error.
func (r *RideRequestRepo) ExpireDispatchedReservation(ctx context.Context, id string) (bool, error) {
	const op = "ride_request.expire_dispatched_reservation"
	start := time.Now()
	var expiredID string
	err := r.pool.QueryRow(ctx, queryRideRequestExpireDispatchedReservation, id).Scan(&expiredID)
	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		r.metrics.IncQueryError(op)
		return false, fmt.Errorf("RideRequestRepo.ExpireDispatchedReservation(%s): %w", id, err)
	}
	return true, nil
}
