package store

import (
	"context"
	"fmt"
	"time"
)

// The MEMBER third of the vehicle catalog (MYR-540).
//
// vehicle_repo_list.go answers "cars you own"; vehicle_repo_list_shared.go
// answers "cars somebody shared with you". This file answers the last way a
// car can legitimately be in somebody's catalog: "the car serving a LIVE group
// ride you joined". Without it a joiner's §7.0 read omits the ride's vehicle,
// and the app's watched-vehicle ladder — which resolves against the catalog —
// falls back to "no such car" while the WS access set (internal/auth's third
// UNION leg) would happily admit a subscription. The catalog and the access
// set must name the same cars, and this query is deliberately the catalog
// restatement of that leg's predicate.

// memberSummaryJoin is the live-membership join. Like sharedSummaryJoin above
// it IS the access check, so every predicate that decides access sits in the
// JOIN: only vehicles serving a NON-TERMINAL ride the caller holds a
// membership row on survive. The status list is character-for-character the
// one internal/auth's queryUserVehicleIDs third leg carries — a catalog that
// listed a car the handshake refuses (or vice versa) would be the worst of
// both answers.
//
// Aliases are mr/gm, NOT r: vehicleListHasActiveRideExpr nests an EXISTS whose
// own alias is r, and shadowing it from the outer query would make that
// predicate read as correlated when it is not.
const memberSummaryJoin = `
JOIN go_ride_requests mr
  ON mr.vehicle_id = "Vehicle"."id"
 AND mr.status NOT IN ('completed', 'declined', 'cancelled')
JOIN go_ride_members gm
  ON gm.ride_id = mr.id
 AND gm.user_id = $1`

// queryVehiclesForRideMember is the member-side companion of
// queryVehiclesSharedWithUser: the identical lean projection, with the
// capability column a literal FALSE — membership conveys the ZERO grant
// (viewer tier, no ride capability), exactly what internal/auth's
// resolveRidingMember resolves for the same caller on the same car.
//
// The literal keeps the scan in lockstep with scanSharedVehicleSummaryRow
// rather than forking a second column list that must be maintained in
// parallel; the rows are re-typed at the adapter boundary. Every column the
// other two catalog reads gain has to be added HERE too, in the SAME position,
// for exactly that reason — MYR-592's telemetry-suspension expression is the
// most recent, after MYR-581's owner-name one.
//
// DISTINCT ON collapses the residual double-membership case (a reservation
// plus an instant ride on one car — the same boundary rest-api.md's
// "instant ↔ reservation" bullet documents) to one catalog row.
const queryVehiclesForRideMember = `SELECT DISTINCT ON ("Vehicle"."id") ` + sharedSummaryColumns + `,
	` + vehicleListHasActiveRideExpr + `,
	gcs.service_etc, gcs.service_expected_end_at,
	` + rideShareEnabledExpr + `,
	` + catalogTrimLabelExpr + `,
	` + catalogOwnerNameExpr + `,
	` + catalogTelemetrySuspendedExpr + `,
	` + setupScheduleColumns + `,
	FALSE
FROM "Vehicle"` + memberSummaryJoin + `
LEFT JOIN go_vehicle_control_state gcs ON gcs.vehicle_id = "Vehicle"."id"` +
	catalogTelemetrySuspendedJoin + setupScheduleJoin + `
ORDER BY "Vehicle"."id"`

// ListMemberVehicleSummaries returns the vehicles serving live group rides the
// caller has JOINED, as viewer-shaped catalog rows. An empty result is the
// ordinary state for anyone who has not redeemed a group-ride link — and the
// state every caller returns to when their rides end, with no revocation step:
// the join's status predicate is the whole lifetime of the access.
func (r *VehicleRepo) ListMemberVehicleSummaries(ctx context.Context, userID string) ([]SharedVehicleSummary, error) {
	const op = "vehicle.list_member_summaries"
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryVehiclesForRideMember, userID)
	if err != nil {
		r.metrics.IncQueryError(op)
		r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
		return nil, fmt.Errorf("VehicleRepo.ListMemberVehicleSummaries(%s): %w", userID, err)
	}
	defer rows.Close()

	var out []SharedVehicleSummary
	for rows.Next() {
		v, scanErr := r.scanSharedVehicleSummaryRow(rows)
		if scanErr != nil {
			r.metrics.IncQueryError(op)
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			return nil, fmt.Errorf("VehicleRepo.ListMemberVehicleSummaries(%s): %w", userID, scanErr)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
		return nil, fmt.Errorf("VehicleRepo.ListMemberVehicleSummaries(%s): rows: %w", userID, err)
	}

	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	return out, nil
}
