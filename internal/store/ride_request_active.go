// Active-ride read for one owner + one vehicle (MYR-400) — the store half of
// `GET /api/ride-requests/incoming?activeForVehicle={id}`.
//
// The question it answers is "WHAT RIDE IS THIS CAR SERVING RIGHT NOW?", and
// before MYR-400 no owner-scoped read could answer it. The owner's two existing
// slices are both about rides that are NOT live: the default incoming feed is
// status `requested` (undecided — decided rows leave it by construction), and
// `upcomingForVehicle` is `accepted` AND STRICTLY FUTURE. A ride in flight fell
// between them, so an owner whose device did not perform the accept had no way
// to recover it (MYR-396 residual: the iOS pointer only remembers what THAT
// device did, so a ride accepted on a second device, or before a reinstall, was
// unrecoverable).
//
// THE TWO SLICES ARE COMPLEMENTARY. `upcomingForVehicle` is the reservation an
// owner could still spare a rider from; this is the ride they are in the middle
// of. A future reservation appears in `upcoming` and NOT here; the same
// reservation appears here once it is live.
//
// THEY ARE NOT DISJOINT BY CONSTRUCTION, and the doc must not claim they are.
// The overlap set is exactly:
//
//	status = 'accepted' AND scheduled_for > NOW() AND dispatch_status = 'sent'
//
// — a reservation the sweeper has dispatched whose instant is STILL FUTURE. It
// satisfies `upcoming` (accepted + future) and `active` (the dormancy dispatch
// arm) simultaneously.
//
// TODAY THAT SET IS EMPTY IN PRODUCTION, but by a property of the SWEEPER, not
// of these predicates: the sweep only claims rows with `scheduled_for <= NOW()`
// (queryRideRequestListDue), so nothing can be dispatched and future at once.
// The only test that constructs such a row plants it with raw SQL.
//
// IT BECOMES REACHABLE WHEN MYR-192 (reschedule) SHIPS.
// queryRideRequestResolveReschedule moves `scheduled_for` forward on confirm
// and does NOT clear `dispatch_status` / `dispatched_at`, so a dispatched
// reservation rescheduled later would land in the overlap set and appear in
// both slices at once.
//
// REQUIREMENT FOR MYR-192, recorded here because this is where the predicates
// live: resolve-reschedule MUST clear `dispatch_status` and `dispatched_at`
// when it moves the instant. That is not merely tidiness — rest-api.md §7.8
// already promises that "a confirmed reschedule moving the instant later
// correctly re-imposes dormancy", and while the dispatch columns survive the
// move the `dispatch_status = 'sent'` arm DEFEATS that promise: the ride would
// stay pickup-able (and `active`) for a reservation that is days out again.
// Clearing them restores both the documented dormancy and the disjointness.

package store

import (
	"context"
	"fmt"
)

// rideActivePredicate is the LIVE-RIDE predicate: a COMMITTED status, minus a
// reservation that is still DORMANT.
//
// THE COMMITTED SET is `accepted` (leg 1, en route to pickup), `arrived` (rider
// aboard) and `enroute` (leg 2, en route to dropoff) — deliberately the same
// three the per-vehicle one-active-ride index (migration 0013) is partial over,
// because they are the states in which a car is promised to a ride. `requested`
// is excluded: the owner has not committed the car, and many riders may hold
// pending requests to one car (that is the incoming feed). Terminal states fall
// out because the car is free again. A new committed state would belong here,
// in index 0013 and in the §7.8 matrix together — the statuses are spelled with
// the enum constants rather than literals so that pairing stays visible.
//
// Dormancy is NOT re-derived here — it is `rideNotDormantPredicate`, MYR-376's
// definition, reused verbatim (this is its third caller, after the pickup
// transition and the Live Activity progress gate). Two spellings of "is this
// ride live" would be two predicates that drift, and the drift would be silent:
// each would keep passing its own tests while disagreeing about whether a car
// is on its way. So an accepted reservation enters this read at exactly the
// instant it becomes pickup-able, and not one tick earlier.
//
// WHY DORMANCY GATES `accepted` ONLY. A ride that has been PICKED UP is live by
// definition — a rider is in the car. Dormancy asks a question about a
// reservation that has not started yet, so it has no meaning past `arrived`,
// and applying it there would be actively harmful: a confirmed reschedule can
// move `scheduled_for` later (§7.8), and a uniform predicate would then drop a
// ride with a passenger aboard out of the one read that exists to recover it.
// The arms are therefore split rather than ANDed together.
//
// It interpolates only in-package constants — no user input reaches it; the
// owner and vehicle scoping below travel as bound parameters as always.
var rideActivePredicate = fmt.Sprintf(
	`(status IN ('%s', '%s') OR (status = '%s' AND %s))`,
	RideRequestStatusArrived, RideRequestStatusEnroute, RideRequestStatusAccepted,
	fmt.Sprintf(rideNotDormantPredicate, ""),
)

// queryRideRequestsActiveByOwnerVehicle selects the owner's LIVE rides for one
// vehicle, newest first.
//
// Ordering is the feed's DEFAULT `created_at DESC, id DESC` — unlike
// `upcomingForVehicle`, this view has no reason to depart from it. Soonest-first
// was load-bearing there because that dialog names "the NEXT reservation"; here
// the rows are already live and the ordering only has to be a stable total
// order for the keyset to be sound. Reusing the default also means this slice
// reuses the DESCENDING (created_at, id) cursor with no new cursor type — one
// fewer thing that can be fed to the wrong query.
//
// The owner scoping is the whole authorization model, exactly as it is for
// `upcomingForVehicle`: `owner_id` is the JWT subject, so a vehicle the caller
// does not own matches no rows. There is no cross-owner read to express and no
// separate ownership check to forget.
var queryRideRequestsActiveByOwnerVehicle = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE owner_id = $1
  AND vehicle_id = $2
  AND ` + rideActivePredicate + `
ORDER BY created_at DESC, id DESC
LIMIT $3`

// queryRideRequestsActiveByOwnerVehicleCursor is the keyset variant — the same
// descending `(created_at, id) < (…)` row-value comparison the rider list and
// the owner feed use.
var queryRideRequestsActiveByOwnerVehicleCursor = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE owner_id = $1
  AND vehicle_id = $2
  AND ` + rideActivePredicate + `
  AND (created_at, id) < ($3, $4)
ORDER BY created_at DESC, id DESC
LIMIT $5`

// ListActiveByOwnerVehiclePage returns a page of the owner's LIVE rides for one
// vehicle — committed status (`accepted`/`arrived`/`enroute`), reservations
// excluded while dormant — newest first, resuming after cursor. limit <= 0
// applies defaultRideRequestListLimit.
//
// IT RETURNS A LIST, AND THE LIST CAN LEGITIMATELY HOLD MORE THAN ONE ROW.
// Index 0013 guarantees at most one active INSTANT ride per vehicle, but it is
// partial on `scheduled_for IS NULL`, so DUE RESERVATIONS are exempt from it: a
// reservation the sweeper is holding (car busy, owner paused, grant withdrawn)
// stays `accepted` past its due instant and is live by the pickup contract even
// while an instant ride occupies the car. The common case is 0 or 1 rows; the
// pagination is not decoration.
//
// An unknown or unowned vehicleID is NOT an error — it is an empty page, which
// is what lets the HTTP layer answer 200 `items: []` instead of a 403/404 that
// would confirm somebody else's car exists.
func (r *RideRequestRepo) ListActiveByOwnerVehiclePage(
	ctx context.Context,
	ownerID, vehicleID string,
	cursor RideRequestListCursor,
	limit int,
) (RideRequestListPage, error) {
	const op = "ride_request.list_active_by_owner_vehicle"
	limit = normalizeLimit(limit)

	var (
		recs []RideRequestRecord
		err  error
	)
	if cursor.IsZero() {
		recs, err = r.list(ctx, op, queryRideRequestsActiveByOwnerVehicle, ownerID, vehicleID, limit+1)
	} else {
		recs, err = r.list(ctx, op, queryRideRequestsActiveByOwnerVehicleCursor,
			ownerID, vehicleID, cursor.CreatedAt, cursor.ID, limit+1)
	}
	if err != nil {
		return RideRequestListPage{}, fmt.Errorf("RideRequestRepo.ListActiveByOwnerVehiclePage(%s, %s): %w", ownerID, vehicleID, err)
	}
	return trimPage(recs, limit), nil
}
