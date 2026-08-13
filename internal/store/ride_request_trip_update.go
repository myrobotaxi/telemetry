// The guarded TRIP-SHAPE write (MYR-541): change the pickup and/or drop-off
// of a live ride, versioned so two editors cannot silently clobber each other.
//
// ONE statement decides everything, the standing MYR-174/175/522 discipline:
// the status window, the version match and the place writes ride the same
// UPDATE, so a ride that moved on — or a trip edited between this caller's
// read and its write — loses in the database, never in a pre-check race.

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

// RideTripEdit is one trip-shape edit: either endpoint, or both in one write.
type RideTripEdit struct {
	Pickup  *RidePlace
	Dropoff *RidePlace
	// ExpectVersion is the trip_version the editor holds; the write loses
	// against any other.
	ExpectVersion int
}

// UpdateTrip applies a trip edit while the ride's status is inside the
// caller's window, against the expected version. No-row means the guard
// refused — status moved, version stale, or the ride vanished — and the
// CALLER re-reads to say which; this layer reports only ErrRideRequestConflict.
func (r *RideRequestRepo) UpdateTrip(
	ctx context.Context,
	id string,
	edit RideTripEdit,
	from []string,
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
	row := r.pool.QueryRow(ctx, queryRideRequestUpdateTrip,
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
