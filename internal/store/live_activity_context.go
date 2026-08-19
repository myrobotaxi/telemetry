// The lifecycle fan-out's read path (MYR-172).
//
// A status transition already knows which ride changed, so unlike the ticker's
// per-pass list this is a single-row read — but it must project EXACTLY the same
// content-state inputs, or the two send paths would build different cards for
// the same state. It shares navFreshnessJoin and dispatchUnderwaySelect with the
// list query next door for that reason.
//
// Split out of live_activity_leg.go to keep both files under the 300-line cap
// (CLAUDE.md "File Rules").

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// activityContextColumns is the same projection for a SINGLE ride, used by the
// lifecycle fan-out: a status transition already knows the ride id, but still
// needs the car's nickname, the destination label and the carried ETA to build
// the content-state it is about to send.
var queryRideActivityContext = `
SELECT r.status,
       r.vehicle_id,
       COALESCE(v."name", ''),
       r.dropoff_label,
       v."etaMinutes",
       v."tripDistanceRemaining",
       nf.nav_reading_at,
       r.dispatched_at,
       ` + dispatchUnderwaySelect + `
FROM go_ride_requests r
LEFT JOIN "Vehicle" v ON v."id" = r.vehicle_id
` + navFreshnessJoin + `
WHERE r.id = $1`

// RideActivityContext is the per-ride half of a content-state: everything that
// is not the Activity's own token.
type RideActivityContext struct {
	Status       RideRequestStatus
	VehicleID    string
	VehicleName  string
	DropoffLabel string
	ETAMinutes   *int
	// TripMilesRemaining, NavUpdatedAt and DispatchUnderway feed the progress
	// track and its two gates (MYR-398); PickupDispatchedAt joins them for the
	// Arriving alert rung (MYR-409). See the LiveActivityLeg fields of the same
	// name.
	TripMilesRemaining *float64
	NavUpdatedAt       *time.Time
	PickupDispatchedAt *time.Time
	DispatchUnderway   bool
	// Stops is the trip's intermediate stops in travel order, empty on a
	// two-endpoint ride (MYR-587). It is what lets the card name the place the
	// ETA is about rather than the trip's end. See ActivityStopsForRides.
	Stops []ActivityStop
}

// ActivityContextForRide reads the content-state inputs for one ride.
//
// Returns ErrRideRequestNotFound (wrapping sdk.ErrNotFound) when the ride is
// gone — which a terminal-state send can legitimately race with, since owner
// teardown hard-deletes rides.
func (r *LiveActivityRepo) ActivityContextForRide(ctx context.Context, rideRequestID string) (RideActivityContext, error) {
	var out RideActivityContext
	err := r.pool.QueryRow(ctx, queryRideActivityContext, rideRequestID).Scan(
		&out.Status,
		&out.VehicleID,
		&out.VehicleName,
		&out.DropoffLabel,
		&out.ETAMinutes,
		&out.TripMilesRemaining,
		&out.NavUpdatedAt,
		&out.PickupDispatchedAt,
		&out.DispatchUnderway,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RideActivityContext{}, fmt.Errorf("store.ActivityContextForRide(%s): %w", rideRequestID, ErrRideRequestNotFound)
	}
	if err != nil {
		return RideActivityContext{}, fmt.Errorf("store.ActivityContextForRide(%s): %w", rideRequestID, err)
	}

	// The stop list is a SECOND statement for the reason live_activity_stops.go
	// gives, and it is read here rather than by the caller so that this path and
	// the ticker's list cannot end up projecting different cards for one ride.
	// A failure propagates rather than degrading to an empty list: silently
	// dropping the stops is exactly the wrong claim MYR-587 is about, and a
	// database that cannot serve this index scan did not serve the read above
	// either.
	stops, err := r.ActivityStopsForRides(ctx, []string{rideRequestID})
	if err != nil {
		return RideActivityContext{}, fmt.Errorf("store.ActivityContextForRide(%s): %w", rideRequestID, err)
	}
	out.Stops = stops[rideRequestID]
	return out, nil
}
