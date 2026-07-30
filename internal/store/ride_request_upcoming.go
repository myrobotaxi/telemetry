// Upcoming-reservations read for one owner + one vehicle (MYR-360) — the
// store half of `GET /api/ride-requests/incoming?upcomingForVehicle={id}`.
//
// Why this is not just a filter on ListByOwnerPage: it answers a DIFFERENT
// question in a DIFFERENT order. The owner incoming feed is "what have I not
// decided yet" (status `requested`, newest first). This is "what have I already
// PROMISED on this car" (status `accepted`, SOONEST first) — the set an owner
// would silently break by pausing ride sharing. Keeping it in its own query
// keeps both orderings honest and leaves the feed's shape untouched.

package store

import (
	"context"
	"fmt"
	"time"
)

// RideRequestUpcomingCursor is the ASCENDING (scheduledFor, id) anchor pair the
// HTTP layer encodes into the opaque cursor. The zero value means "first page"
// — the repo treats an empty ID as "no anchor" and runs the un-anchored query.
// Distinct from RideRequestListCursor because the two walk opposite directions
// over different columns and must never be substituted for one another.
type RideRequestUpcomingCursor struct {
	ScheduledFor time.Time
	ID           string
}

// IsZero reports whether the cursor carries no anchor (first page).
func (c RideRequestUpcomingCursor) IsZero() bool {
	return c.ID == "" && c.ScheduledFor.IsZero()
}

// queryRideRequestsUpcomingByOwnerVehicle selects an owner's ACCEPTED, still
// FUTURE reservations for ONE vehicle, soonest first.
//
// `scheduled_for > NOW()` is STRICT on purpose: a reservation already due is
// not something the owner can still spare the rider from — the reservation
// sweeper owns it from that instant, and the hold-then-expire backstop is what
// resolves it. Warning an owner about a reservation they can no longer get
// ahead of would be noise.
//
// The owner scoping is not decoration: it is the whole authorization model for
// this read. `owner_id` is the JWT subject, so a vehicle the caller does not
// own simply matches no rows — there is no cross-owner read to express and no
// separate ownership check to forget.
const queryRideRequestsUpcomingByOwnerVehicle = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE owner_id = $1
  AND vehicle_id = $2
  AND status = 'accepted'
  AND scheduled_for IS NOT NULL
  AND scheduled_for > NOW()
ORDER BY scheduled_for ASC, id ASC
LIMIT $3`

// queryRideRequestsUpcomingByOwnerVehicleCursor is the keyset variant. The
// `(scheduled_for, id) > (…)` row-value comparison is the same Postgres keyset
// discipline as the (created_at, id) cursors, just ASCENDING — and every row in
// this predicate has a non-null scheduled_for, so the comparison never meets a
// NULL and can never silently drop a row.
const queryRideRequestsUpcomingByOwnerVehicleCursor = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE owner_id = $1
  AND vehicle_id = $2
  AND status = 'accepted'
  AND scheduled_for IS NOT NULL
  AND scheduled_for > NOW()
  AND (scheduled_for, id) > ($3, $4)
ORDER BY scheduled_for ASC, id ASC
LIMIT $5`

// ListUpcomingByOwnerVehiclePage returns a page of the owner's accepted, still
// future reservations for one vehicle, SOONEST first, resuming after cursor.
// limit <= 0 applies defaultRideRequestListLimit.
//
// An unknown or unowned vehicleID is NOT an error — it is an empty page, which
// is what lets the HTTP layer answer 200 `items: []` instead of a 403/404 that
// would confirm somebody else's car exists.
func (r *RideRequestRepo) ListUpcomingByOwnerVehiclePage(
	ctx context.Context,
	ownerID, vehicleID string,
	cursor RideRequestUpcomingCursor,
	limit int,
) (RideRequestListPage, error) {
	const op = "ride_request.list_upcoming_by_owner_vehicle"
	limit = normalizeLimit(limit)

	var (
		recs []RideRequestRecord
		err  error
	)
	if cursor.IsZero() {
		recs, err = r.list(ctx, op, queryRideRequestsUpcomingByOwnerVehicle, ownerID, vehicleID, limit+1)
	} else {
		recs, err = r.list(ctx, op, queryRideRequestsUpcomingByOwnerVehicleCursor,
			ownerID, vehicleID, cursor.ScheduledFor, cursor.ID, limit+1)
	}
	if err != nil {
		return RideRequestListPage{}, fmt.Errorf("RideRequestRepo.ListUpcomingByOwnerVehiclePage(%s, %s): %w", ownerID, vehicleID, err)
	}
	return trimPage(recs, limit), nil
}
