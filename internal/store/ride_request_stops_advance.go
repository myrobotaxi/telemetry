// LEG ADVANCE across a multi-stop trip (MYR-539): the writes that move a ride
// from one leg to the next when the car is OBSERVED to reach a stop.
//
// This is lifecycle, not editing. It advances stop STATUSES only — it never
// touches trip_version, because the trip's shape did not change; a client that
// wants live leg progress reads the statuses back on the refetch the frame it
// already receives triggers.
//
// EXACTLY-ONCE is the guarded UPDATE's, the discipline every other transition
// in this repo uses: the completion write matches a row only while the stop is
// NOT already completed, so a re-delivered waypoint event (or two replicas
// seeing the same arrival) resolves to one winner and one silent refusal. The
// caller re-shares the car's nav ONLY on the win — which is what keeps a
// duplicate event from re-dialling the dash.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrRideStopNotAdvanced is the ordinary loser's outcome of the leg advance:
// the stop was already completed (a re-delivered event), or it does not belong
// to this ride. Deliberately NOT wrapping sdk.ErrNotFound — nothing is missing,
// somebody else already did the work.
var ErrRideStopNotAdvanced = errors.New("ride stop already advanced")

const (
	// queryRideStopComplete is the exactly-once latch. `status <> 'completed'`
	// is the whole guard: it admits the stop the car is driving to ('current')
	// and also one still marked 'upcoming' — which is reachable if the
	// promotion at Start was lost — so a missed promotion costs a status that
	// was briefly wrong, never a trip that cannot advance past it.
	queryRideStopComplete = `UPDATE go_ride_stops SET
	status = 'completed',
	updated_at = NOW()
WHERE ride_id = $1 AND id = $2 AND status <> 'completed'
RETURNING position`

	// queryRideDropoffTarget reads the ride's final destination — the next
	// target when the completed stop was the last one. Two ciphertext columns
	// and two decrypts, the lean-read shape: the leg advance needs a
	// coordinate, not a RideRequest.
	queryRideDropoffTarget = `SELECT dropoff_lat_enc, dropoff_lng_enc, dropoff_label, dropoff_address
FROM go_ride_requests
WHERE id = $1`
)

// RideStopAdvance is what one won leg advance resolved to: the stop just
// completed, and the ride's NEXT target for the car's nav.
type RideStopAdvance struct {
	// CompletedStopID is the stop this call marked completed.
	CompletedStopID string
	// NextStopID is the stop that became 'current', or EMPTY when the completed
	// stop was the last one and the final drop-off is next.
	NextStopID string
	// NextTarget is the place the car should now be driving to — the new
	// current stop, or the ride's drop-off. P1 GPS data, never logged.
	NextTarget RidePlace
}

// AdvanceStopArrival marks an arrived stop completed, promotes the next
// upcoming stop to current, and returns the ride's next target — all in one
// transaction, so a caller that receives a target knows the statuses behind it
// are committed.
//
// Returns ErrRideStopNotAdvanced when the completion write matched no row: the
// stop is already completed (re-delivery), or the (ride, stop) pair is not a
// pair at all. Callers treat it as an ordinary no-op.
func (r *RideRequestRepo) AdvanceStopArrival(ctx context.Context, rideID, stopID string) (RideStopAdvance, error) {
	const op = "ride_request.advance_stop_arrival"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RideStopAdvance{}, fmt.Errorf("RideRequestRepo.AdvanceStopArrival(%s): begin: %w", rideID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	var position int
	err = tx.QueryRow(ctx, queryRideStopComplete, rideID, stopID).Scan(&position)
	if errors.Is(err, pgx.ErrNoRows) {
		return RideStopAdvance{}, fmt.Errorf(
			"RideRequestRepo.AdvanceStopArrival(%s, %s): %w", rideID, stopID, ErrRideStopNotAdvanced)
	}
	if err != nil {
		r.metrics.IncQueryError(op)
		return RideStopAdvance{}, fmt.Errorf("RideRequestRepo.AdvanceStopArrival(%s, %s): %w", rideID, stopID, err)
	}

	advance := RideStopAdvance{CompletedStopID: stopID}
	next, err := r.promoteFirstUpcomingStop(ctx, tx, rideID)
	if err != nil {
		r.metrics.IncQueryError(op)
		return RideStopAdvance{}, fmt.Errorf("RideRequestRepo.AdvanceStopArrival(%s): %w", rideID, err)
	}
	if next != nil {
		advance.NextStopID, advance.NextTarget = next.ID, next.Place
	} else if advance.NextTarget, err = r.readDropoffTarget(ctx, tx, rideID); err != nil {
		r.metrics.IncQueryError(op)
		return RideStopAdvance{}, fmt.Errorf("RideRequestRepo.AdvanceStopArrival(%s): %w", rideID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return RideStopAdvance{}, fmt.Errorf("RideRequestRepo.AdvanceStopArrival(%s): commit: %w", rideID, err)
	}
	return advance, nil
}

// StartFirstStop makes a ride's earliest upcoming stop CURRENT and returns it —
// the rider's Start, for a trip that has stops (MYR-539). Returns a nil stop for
// a plain two-endpoint trip (and for a ride whose first stop is somehow already
// current), which is the caller's signal to target the drop-off.
//
// Idempotent by construction: the statement matches nothing once a current stop
// exists, so a retried Start promotes nothing a second time.
func (r *RideRequestRepo) StartFirstStop(ctx context.Context, rideID string) (*RideStop, error) {
	const op = "ride_request.start_first_stop"
	start := time.Now()
	stop, err := r.promoteFirstUpcomingStop(ctx, r.pool, rideID)
	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("RideRequestRepo.StartFirstStop(%s): %w", rideID, err)
	}
	return stop, nil
}

// readDropoffTarget decrypts the ride's final destination for the leg advance.
func (r *RideRequestRepo) readDropoffTarget(ctx context.Context, q pgxQuerier, rideID string) (RidePlace, error) {
	var (
		place  RidePlace
		latEnc string
		lngEnc string
	)
	err := q.QueryRow(ctx, queryRideDropoffTarget, rideID).Scan(&latEnc, &lngEnc, &place.Label, &place.Address)
	if errors.Is(err, pgx.ErrNoRows) {
		return RidePlace{}, ErrRideRequestNotFound
	}
	if err != nil {
		return RidePlace{}, fmt.Errorf("read ride dropoff target: %w", err)
	}
	if place.Latitude, err = r.decryptCoord("dropoff_lat_enc", latEnc); err != nil {
		return RidePlace{}, err
	}
	if place.Longitude, err = r.decryptCoord("dropoff_lng_enc", lngEnc); err != nil {
		return RidePlace{}, err
	}
	return place, nil
}
