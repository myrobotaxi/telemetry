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
	// NavUpdatedAt is when the car's ROW was last written — an upper bound on
	// how old the two readings above are, not a stamp on the readings
	// themselves. Nil for a car we have never heard from. It gates the progress
	// fraction only, as a cheap pre-filter; see RideContext.NavUpdatedAt in
	// internal/push for why the sender does not trust it on its own.
	NavUpdatedAt *time.Time
	// DispatchUnderway is MYR-376's reservation-dormancy predicate, evaluated
	// in SQL: false while a reservation is still sleeping between accept and
	// the earlier of dispatch and its due instant.
	//
	// The progress track needs it because ride status alone cannot tell a car
	// that is DRIVING TO THE RIDER from a reservation accepted for tomorrow —
	// both read `accepted` — and the owner's private errands in between would
	// otherwise anchor and advance a track toward a pickup a day away
	// (MYR-398). See computeProgress.
	DispatchUnderway bool
}

// ActiveLegStatuses is the set of ride statuses that keep an Activity ticking.
//
// All four are "the ride is happening": requested is the rider waiting for an
// answer, accepted is leg 1 (car driving to the pickup), arrived is the
// handshake at the kerb, enroute is leg 2 (car driving to the dropoff).
//
// TWO OF THE FOUR HAVE NOTHING TO REFRESH, AND ARE HERE ANYWAY. A stationary
// `arrived` car reports no nav ETA and a `requested` ride has no car assigned
// at all, so neither tick changes a number. What each tick DOES change is the
// timestamp and the stale-date, and that is the point: an Activity nobody
// pushes to slides into ActivityKit's own "as of X min ago" rendering after
// three minutes. For `arrived` that means a card apologising for being out of
// date while the rider stands next to the car it describes; for `requested` it
// means "Finding your ride" going stale while the search is genuinely still
// running, which is the v3 Dispatch state accusing itself of a fault it does
// not have.
//
// `requested` JOINED WITH THE v3 CARD (MYR-398), which starts the Activity at
// REQUEST rather than at accept. Nothing else about it is special-cased: the
// progress track already returns no key for a status with no leg
// (push.legForStatus), and `eta` is withheld because the car the projection
// reads navigation from has not been assigned to this ride yet
// (push.etaKnown). What the rider sees is a Dispatch card that stays fresh
// until the owner answers.
//
// SCHEDULED RIDES ARE UNAFFECTED. A reservation is `accepted` from the moment
// it is booked, and its dormancy is handled where it always was — by the
// DispatchUnderway predicate gating the track, not by this list. `requested` is
// the INSTANT-ride case: a rider who has just asked, waiting on an answer.
var ActiveLegStatuses = []RideRequestStatus{
	RideRequestStatusRequested,
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
var queryListActiveLegActivities = `
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
       ` + dispatchUnderwaySelect + `,
       ` + progressColumns + `
FROM go_live_activities a
JOIN go_ride_requests r ON r.id = a.ride_request_id
LEFT JOIN "Vehicle" v ON v."id" = r.vehicle_id
WHERE a.ended_at IS NULL
  AND r.status = ANY($1)
ORDER BY a.updated_at ASC
LIMIT $2`

// dispatchUnderwaySelect renders the shared dormancy predicate as a selectable
// boolean over the ride aliased `r`.
//
// COALESCE is load-bearing rather than defensive: for the undispatched
// reservation the gate exists for, `r.dispatch_status = 'sent'` is NULL and the
// OR chain evaluates to NULL, which would fail the scan into a bool. Folding
// NULL to FALSE reproduces exactly what the same predicate means in a WHERE
// clause — unknown dispatch is not proof of dispatch.
var dispatchUnderwaySelect = `COALESCE(` + fmt.Sprintf(rideNotDormantPredicate, "r.") + `, FALSE)`

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
		var pBaseline, pValue, pReading *float64
		var pReadingAt *time.Time
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
			&leg.DispatchUnderway,
			&pLeg, &pSource, &pBaseline, &pValue, &pReading, &pReadingAt,
			&leg.AlertedPhase,
		); err != nil {
			return nil, fmt.Errorf("store.ListActiveLegActivities: scan: %w", err)
		}
		leg.Progress = scanProgress(pLeg, pSource, pBaseline, pValue, pReading, pReadingAt)
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
var queryRideActivityContext = `
SELECT r.status,
       r.vehicle_id,
       COALESCE(v."name", ''),
       r.dropoff_label,
       v."etaMinutes",
       v."tripDistanceRemaining",
       v."lastUpdated",
       ` + dispatchUnderwaySelect + `
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
	// TripMilesRemaining, NavUpdatedAt and DispatchUnderway feed the progress
	// track and its two gates (MYR-398); see the LiveActivityLeg fields of the
	// same name.
	TripMilesRemaining *float64
	NavUpdatedAt       *time.Time
	DispatchUnderway   bool
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
		&out.DispatchUnderway,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RideActivityContext{}, fmt.Errorf("store.ActivityContextForRide(%s): %w", rideRequestID, ErrRideRequestNotFound)
	}
	if err != nil {
		return RideActivityContext{}, fmt.Errorf("store.ActivityContextForRide(%s): %w", rideRequestID, err)
	}
	return out, nil
}
