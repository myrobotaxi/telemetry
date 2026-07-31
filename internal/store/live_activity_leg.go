package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The ETA ticker's read path (MYR-172).
//
// Per MYR-194 the Activity refreshes its ETA every 60–90s while a leg is
// active, which means one pass over every live Activity on every tick, forever.
// That budget is why this is a purpose-built projection and not
// VehicleRepo.GetByID: the snapshot read is ~70 columns plus a join onto
// go_vehicle_control_state, and the ticker needs six values. Per AGENTS.md,
// wide selects belong to detail/edit handlers.
//
// The "Vehicle" table is Prisma-owned and READ-ONLY here (CG-DL-9): this is a
// SELECT and nothing else. Note that CG-DL-9's ban on naming Prisma tables
// applies to migration SQL, not to repository reads — VehicleNameRepo sets the
// same precedent.

// LiveActivityLeg is one live Activity together with everything its next
// content-state needs. It is the join of the Activity, its ride, and the car.
type LiveActivityLeg struct {
	LiveActivity

	// Status is the ride's current lifecycle status.
	Status RideRequestStatus
	// VehicleID is the car serving the ride.
	VehicleID string
	// VehicleName is the owner-chosen nickname, "" when the car has none.
	VehicleName string
	// DropoffLabel is the destination's short name. P1 — see the content-state
	// classification note in data-classification.md §1.18.
	DropoffLabel string
	// ETAMinutes is the car's own carried navigation ETA in whole minutes, or
	// nil when the car has no active nav route.
	//
	// This is the vehicle's number, streamed from Tesla (minutesToArrival) and
	// persisted verbatim — NOT a server-side estimate. There is no
	// route-solving ETA anywhere in this service, so an absent nav route means
	// an absent ETA and the Activity renders no time at all rather than a
	// number we made up.
	ETAMinutes *int
	// TripMilesRemaining is the car's own remaining distance on that route, in
	// miles (Tesla's milesToArrival, stored as tripDistanceRemaining), nil when
	// it reports none. The preferred input to the progress track (MYR-398):
	// a track depicts distance, and distance to a fixed point falls as the car
	// drives rather than being re-estimated in both directions the way minutes
	// are.
	TripMilesRemaining *float64
	// NavUpdatedAt is when the car's row was last written — how OLD the two
	// readings above are. Nil for a car we have never heard from. It gates the
	// progress fraction only: a stale fraction is indistinguishable from a
	// fresh one on a lock screen, whereas a stale ETA visibly slides into the
	// past.
	NavUpdatedAt *time.Time
}

// ActiveLegStatuses is the set of ride statuses that keep an Activity ticking.
//
// All three are "the ride is happening": accepted is leg 1 (car driving to the
// pickup), arrived is the handshake at the kerb, enroute is leg 2 (car driving
// to the dropoff). arrived is included even though a stationary car reports no
// nav ETA — the tick still refreshes the timestamp and the stale-date, which is
// what stops the Activity from drifting into its own "as of X min ago" state
// while the rider is standing next to the car it is describing.
var ActiveLegStatuses = []RideRequestStatus{
	RideRequestStatusAccepted,
	RideRequestStatusArrived,
	RideRequestStatusEnroute,
}

// queryListActiveLegActivities is the ticker's one statement per pass.
//
// LEFT JOIN onto the car, not INNER: a vehicle row that has gone away must not
// silently drop the Activity from the pass, because the ride is still the
// rider's reality and the Activity still needs its timestamp refreshed. The
// nickname degrades to "" and the ETA to NULL, which the content-state builder
// already handles.
//
// Ordered by updated_at so the least recently pushed Activity goes first: when
// a pass is capped by the per-pass limit, the cap sheds the Activities that
// were updated most recently rather than starving the ones that most need it.
const queryListActiveLegActivities = `
SELECT a.ride_request_id,
       a.user_id,
       a.activity_push_token,
       a.sandbox,
       r.status,
       r.vehicle_id,
       COALESCE(v."name", ''),
       r.dropoff_label,
       v."etaMinutes",
       v."tripDistanceRemaining",
       v."lastUpdated",
       ` + progressColumns + `
FROM go_live_activities a
JOIN go_ride_requests r ON r.id = a.ride_request_id
LEFT JOIN "Vehicle" v ON v."id" = r.vehicle_id
WHERE a.ended_at IS NULL
  AND r.status = ANY($1)
ORDER BY a.updated_at ASC
LIMIT $2`

// ListActiveLegActivities returns up to limit live Activities whose ride is
// mid-leg, least recently updated first.
//
// An empty result is the ordinary state — most of the time no ride is in
// flight — so it is not an error.
func (r *LiveActivityRepo) ListActiveLegActivities(ctx context.Context, limit int) ([]LiveActivityLeg, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store.ListActiveLegActivities: non-positive limit %d", limit)
	}

	statuses := make([]string, 0, len(ActiveLegStatuses))
	for _, s := range ActiveLegStatuses {
		statuses = append(statuses, string(s))
	}

	rows, err := r.pool.Query(ctx, queryListActiveLegActivities, statuses, limit)
	if err != nil {
		return nil, fmt.Errorf("store.ListActiveLegActivities: %w", err)
	}
	defer rows.Close()

	var out []LiveActivityLeg
	for rows.Next() {
		var leg LiveActivityLeg
		var pLeg, pSource *string
		var pBaseline, pValue *float64
		if err := rows.Scan(
			&leg.RideRequestID,
			&leg.UserID,
			&leg.ActivityPushToken,
			&leg.Sandbox,
			&leg.Status,
			&leg.VehicleID,
			&leg.VehicleName,
			&leg.DropoffLabel,
			&leg.ETAMinutes,
			&leg.TripMilesRemaining,
			&leg.NavUpdatedAt,
			&pLeg, &pSource, &pBaseline, &pValue,
		); err != nil {
			return nil, fmt.Errorf("store.ListActiveLegActivities: scan: %w", err)
		}
		leg.Progress = scanProgress(pLeg, pSource, pBaseline, pValue)
		out = append(out, leg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListActiveLegActivities: iterate: %w", err)
	}
	return out, nil
}

// activityContextColumns is the same projection for a SINGLE ride, used by the
// lifecycle fan-out: a status transition already knows the ride id, but still
// needs the car's nickname, the destination label and the carried ETA to build
// the content-state it is about to send.
const queryRideActivityContext = `
SELECT r.status,
       r.vehicle_id,
       COALESCE(v."name", ''),
       r.dropoff_label,
       v."etaMinutes",
       v."tripDistanceRemaining",
       v."lastUpdated"
FROM go_ride_requests r
LEFT JOIN "Vehicle" v ON v."id" = r.vehicle_id
WHERE r.id = $1`

// RideActivityContext is the per-ride half of a content-state: everything that
// is not the Activity's own token.
type RideActivityContext struct {
	Status       RideRequestStatus
	VehicleID    string
	VehicleName  string
	DropoffLabel string
	ETAMinutes   *int
	// TripMilesRemaining and NavUpdatedAt feed the progress track and its
	// freshness gate (MYR-398); see the LiveActivityLeg fields of the same name.
	TripMilesRemaining *float64
	NavUpdatedAt       *time.Time
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
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RideActivityContext{}, fmt.Errorf("store.ActivityContextForRide(%s): %w", rideRequestID, ErrRideRequestNotFound)
	}
	if err != nil {
		return RideActivityContext{}, fmt.Errorf("store.ActivityContextForRide(%s): %w", rideRequestID, err)
	}
	return out, nil
}
