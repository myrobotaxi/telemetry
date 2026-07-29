package store

import (
	"context"
	"fmt"
	"time"
)

// The VIEWER half of the vehicle catalog (MYR-184 / MYR-91 viewer merge).
//
// ListSummariesByUser (vehicle_repo_list.go) answers "cars you own" and is
// deliberately left untouched: owner rows must not change shape or cost because
// sharing shipped. This file answers the other half — "cars somebody shared
// with you" — as a separate lean read, and the handler concatenates the two.
// Two statements rather than one OR-joined statement keeps the owner path's
// index plan exactly as MYR-122 tuned it.

// sharedSummaryJoin is the accepted-grant join. The share row is the FILTER,
// not a decoration: only rows with a live accepted grant for this caller
// survive, so there is no way for the projection to emit a vehicle the caller
// was not granted. `status = 'accepted'` excludes both unredeemed invites and
// revoked tombstones.
const sharedSummaryJoin = `
JOIN go_vehicle_shares s
  ON s.vehicle_id = "Vehicle"."id"
 AND s.accepted_by_user_id = $1
 AND s.status = 'accepted'`

// sharedSummaryColumns is vehicleListSummaryColumns with every name qualified
// to the vehicle relation.
//
// The qualification is REQUIRED, not stylistic: go_vehicle_shares also has `id`
// and `status` columns, so the unqualified list is ambiguous the moment the
// grant join is added and Postgres refuses the statement outright (42702).
// Listing the columns again rather than reusing the shared constant is the cost
// of joining a table with overlapping names; the two lists must be kept in
// lockstep, and the scan function asserts the order.
const sharedSummaryColumns = `"Vehicle"."id", "Vehicle"."userId", "Vehicle"."vin", "Vehicle"."name",
	"Vehicle"."model", "Vehicle"."year", "Vehicle"."color", "Vehicle"."licensePlate", "Vehicle"."status",
	"Vehicle"."chargeLevel", "Vehicle"."estimatedRange", "Vehicle"."lastUpdated"`

// queryVehiclesSharedWithUser is the viewer-side companion of
// queryVehiclesByUserList: identical lean projection plus the grant's tier, so
// the handler can stamp VehicleSummary.sharePermission without a second lookup.
//
// $2 is an OPTIONAL id filter. NULL means "every vehicle shared with me" (the
// catalog list); a non-null array narrows to a specific set (the redeem
// response, which reports only what that redemption granted). Expressing it as
// one statement keeps the two callers provably on the same access predicate —
// a second copy of this SQL is exactly where an access-control drift would hide.
const queryVehiclesSharedWithUser = `SELECT ` + sharedSummaryColumns + `,
	` + vehicleListHasActiveRideExpr + `,
	gcs.service_etc, gcs.service_expected_end_at,
	s.permission
FROM "Vehicle"` + sharedSummaryJoin + `
LEFT JOIN go_vehicle_control_state gcs ON gcs.vehicle_id = "Vehicle"."id"
WHERE ($2::text[] IS NULL OR "Vehicle"."id" = ANY($2))
ORDER BY "Vehicle"."name", "Vehicle"."vin"`

// SharedVehicleSummary is a catalog row the caller sees as a VIEWER, carrying
// the tier the grant conveys. Permission is always one of the three tiers —
// the join guarantees a grant row exists — so the handler never has to invent
// a default.
type SharedVehicleSummary struct {
	VehicleSummary
	Permission string
}

// ListSharedSummariesByUser returns every vehicle shared with the caller, as
// viewer-shaped catalog rows. An empty result is the ordinary state for someone
// who has not been invited to anything.
func (r *VehicleRepo) ListSharedSummariesByUser(ctx context.Context, userID string) ([]SharedVehicleSummary, error) {
	return r.listSharedSummaries(ctx, userID, nil)
}

// ListSharedSummariesByIDs narrows the same access-checked projection to a
// specific vehicle set — what a single redemption granted.
//
// The id list is a FILTER APPLIED ON TOP OF the grant join, never a substitute
// for it: passing ids the caller has no accepted grant on returns nothing. That
// ordering is what makes the redeem response safe to build from ids the redeem
// transaction just produced.
func (r *VehicleRepo) ListSharedSummariesByIDs(ctx context.Context, userID string, vehicleIDs []string) ([]SharedVehicleSummary, error) {
	if len(vehicleIDs) == 0 {
		return nil, nil
	}
	return r.listSharedSummaries(ctx, userID, vehicleIDs)
}

// listSharedSummaries runs the shared-catalog projection. vehicleIDs nil means
// "no id filter".
func (r *VehicleRepo) listSharedSummaries(ctx context.Context, userID string, vehicleIDs []string) ([]SharedVehicleSummary, error) {
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryVehiclesSharedWithUser, userID, vehicleIDs)
	if err != nil {
		r.metrics.IncQueryError("vehicle.list_shared_summaries")
		r.metrics.ObserveQueryDuration("vehicle.list_shared_summaries", time.Since(start).Seconds())
		return nil, fmt.Errorf("VehicleRepo.listSharedSummaries(%s): %w", userID, err)
	}
	defer rows.Close()

	var out []SharedVehicleSummary
	for rows.Next() {
		v, scanErr := scanSharedVehicleSummaryRow(rows)
		if scanErr != nil {
			r.metrics.IncQueryError("vehicle.list_shared_summaries")
			r.metrics.ObserveQueryDuration("vehicle.list_shared_summaries", time.Since(start).Seconds())
			return nil, fmt.Errorf("VehicleRepo.listSharedSummaries(%s): %w", userID, scanErr)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError("vehicle.list_shared_summaries")
		r.metrics.ObserveQueryDuration("vehicle.list_shared_summaries", time.Since(start).Seconds())
		return nil, fmt.Errorf("VehicleRepo.listSharedSummaries(%s): rows: %w", userID, err)
	}

	r.metrics.ObserveQueryDuration("vehicle.list_shared_summaries", time.Since(start).Seconds())
	return out, nil
}

// scanSharedVehicleSummaryRow reads the lean catalog columns plus the tier.
func scanSharedVehicleSummaryRow(row rowScanner) (SharedVehicleSummary, error) {
	var (
		v      SharedVehicleSummary
		status string
	)
	if err := row.Scan(
		&v.ID,
		&v.UserID,
		&v.VIN,
		&v.Name,
		&v.Model,
		&v.Year,
		&v.Color,
		&v.LicensePlate,
		&status,
		&v.ChargeLevel,
		&v.EstimatedRange,
		&v.LastUpdated,
		&v.HasActiveRide,
		&v.ServiceETC,
		&v.ServiceExpectedEndAt,
		&v.Permission,
	); err != nil {
		return SharedVehicleSummary{}, fmt.Errorf("scan shared vehicle summary: %w", err)
	}
	v.Status = VehicleStatus(status)
	return v, nil
}
