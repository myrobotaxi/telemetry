// The guarded TRIP-SHAPE write (MYR-541, extended with the stop list by
// MYR-539): change the pickup, the drop-off and/or the whole ordered list of
// stops of a live ride, versioned so two editors cannot silently clobber each
// other.
//
// ONE statement decides everything, the standing MYR-174/175/522 discipline:
// the status window, the version match and the place writes ride the same
// UPDATE, so a ride that moved on — or a trip edited between this caller's
// read and its write — loses in the database, never in a pre-check race.
//
// The STOPS ride the same decision (MYR-539). They are rows rather than
// columns, so they cannot join that statement; they join its TRANSACTION
// instead, applied only after the guard has already admitted the edit. There is
// one trip_version for the whole shape, so the version bump the endpoints' UPDATE
// performs is the stop list's bump too, and an edit that touches only stops is
// serialized against a concurrent endpoint edit exactly as two endpoint edits
// are. See ride_request_stops_write.go for the replacement rules.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// queryRideRequestUpdateTrip conditionally rewrites either endpoint. The
// boolean binds ($4/$9) select which places move — a CASE per column keeps
// the unedited endpoint byte-identical — and trip_version increments exactly
// once whichever combination was edited.
//
//	$1 id · $2 allowed-from statuses · $3 expected trip_version
//	$4 setPickup · $5..$8 pickup latEnc/lngEnc/label/address
//	$9 setDropoff · $10..$13 dropoff latEnc/lngEnc/label/address
const queryRideRequestUpdateTrip = `UPDATE go_ride_requests SET
	pickup_lat_enc  = CASE WHEN $4  THEN $5  ELSE pickup_lat_enc  END,
	pickup_lng_enc  = CASE WHEN $4  THEN $6  ELSE pickup_lng_enc  END,
	pickup_label    = CASE WHEN $4  THEN $7  ELSE pickup_label    END,
	pickup_address  = CASE WHEN $4  THEN $8  ELSE pickup_address  END,
	dropoff_lat_enc = CASE WHEN $9  THEN $10 ELSE dropoff_lat_enc END,
	dropoff_lng_enc = CASE WHEN $9  THEN $11 ELSE dropoff_lng_enc END,
	dropoff_label   = CASE WHEN $9  THEN $12 ELSE dropoff_label   END,
	dropoff_address = CASE WHEN $9  THEN $13 ELSE dropoff_address END,
	trip_version = trip_version + 1,
	updated_at = NOW()
WHERE id = $1
  AND status = ANY($2)
  AND trip_version = $3
RETURNING ` + rideRequestColumns

// RideTripEdit is one trip-shape edit: either endpoint, the stop list, or any
// combination of them in one write.
type RideTripEdit struct {
	Pickup  *RidePlace
	Dropoff *RidePlace
	// Stops is the FULL desired list, in travel order — REPLACE semantics
	// (MYR-539). NIL means "leave the stop list exactly as it is"; a non-nil
	// EMPTY slice clears every stop back to a plain two-endpoint trip.
	Stops *[]RideStopEdit
	// ExpectVersion is the trip_version the editor holds; the write loses
	// against any other.
	ExpectVersion int
}

// UpdateTrip applies a trip edit while the ride's status is inside the
// caller's window, against the expected version. No-row means the guard
// refused — status moved, version stale, or the ride vanished — and the
// CALLER re-reads to say which; this layer reports only ErrRideRequestConflict.
//
// A stop replacement the guard admits can STILL be refused, by the stop rules
// themselves: ErrRideRequestConflict for a removed/reordered frozen stop (the
// caller refetches and re-applies, the same loop a stale version runs) and
// ErrRideStopUnknown for an id the ride does not have (a 400 — retrying it
// unchanged can only fail again). Either rolls the whole edit back, endpoints
// included: the client sent one desired trip, not three independent writes.
func (r *RideRequestRepo) UpdateTrip(
	ctx context.Context,
	id string,
	edit RideTripEdit,
	from []string,
) (RideRequestRecord, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateTrip(%s): begin: %w", id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	rec, err := r.updateTripRow(ctx, tx, id, edit, from)
	if err != nil {
		return RideRequestRecord{}, err
	}

	if edit.Stops != nil {
		if rec.Stops, err = r.replaceStops(ctx, tx, id, rec.Status, *edit.Stops); err != nil {
			return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateTrip(%s): %w", id, err)
		}
	} else if rec, err = r.withStops(ctx, tx, rec); err != nil {
		// The response is the whole trip either way: an edit that moved only
		// the drop-off still answers with the stops the ride has.
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateTrip(%s): %w", id, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateTrip(%s): commit: %w", id, err)
	}
	return rec, nil
}

// updateTripRow runs the guarded endpoint UPDATE — the statement that carries
// the whole concurrency argument — and classifies its outcome.
func (r *RideRequestRepo) updateTripRow(
	ctx context.Context, tx pgx.Tx, id string, edit RideTripEdit, from []string,
) (RideRequestRecord, error) {
	var (
		pickupLatEnc, pickupLngEnc   string
		pickupLabel                  string
		pickupAddress                *string
		dropoffLatEnc, dropoffLngEnc string
		dropoffLabel                 string
		dropoffAddress               *string
		err                          error
	)
	if edit.Pickup != nil {
		if pickupLatEnc, pickupLngEnc, err = r.encryptPlace(*edit.Pickup); err != nil {
			return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateTrip(%s): pickup: %w", id, err)
		}
		pickupLabel, pickupAddress = edit.Pickup.Label, edit.Pickup.Address
	}
	if edit.Dropoff != nil {
		if dropoffLatEnc, dropoffLngEnc, err = r.encryptPlace(*edit.Dropoff); err != nil {
			return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateTrip(%s): dropoff: %w", id, err)
		}
		dropoffLabel, dropoffAddress = edit.Dropoff.Label, edit.Dropoff.Address
	}

	const op = "ride_request.update_trip"
	start := time.Now()
	row := tx.QueryRow(ctx, queryRideRequestUpdateTrip,
		id, from, edit.ExpectVersion,
		edit.Pickup != nil, pickupLatEnc, pickupLngEnc, pickupLabel, pickupAddress,
		edit.Dropoff != nil, dropoffLatEnc, dropoffLngEnc, dropoffLabel, dropoffAddress,
	)
	rec, err := r.scanRideRequest(row)
	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	if err != nil {
		// scanRideRequest wraps with %w, so no-rows stays visible here: the
		// guard refused (status window, stale version, or a vanished row).
		if errors.Is(err, pgx.ErrNoRows) {
			return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateTrip(%s): %w", id, ErrRideRequestConflict)
		}
		r.metrics.IncQueryError(op)
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateTrip(%s): %w", id, err)
	}
	return rec, nil
}
