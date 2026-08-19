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
//
// `s.suspended_at IS NULL` excludes SUSPENDED grants (MYR-369), and it belongs
// in the JOIN rather than the WHERE for a reason that is not stylistic: this
// join IS the access check, so every predicate that decides access must sit in
// it. A suspended grant therefore produces no catalog row at all — the vehicle
// vanishes from the viewer's list exactly as if the grant had been revoked,
// which is the specified behaviour. There is deliberately no "suspended" marker
// on the wire for a viewer to render: the row is absent, not decorated.
const sharedSummaryJoin = `
JOIN go_vehicle_shares s
  ON s.vehicle_id = "Vehicle"."id"
 AND s.accepted_by_user_id = $1
 AND s.status = 'accepted'
 AND s.suspended_at IS NULL`

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
	"Vehicle"."chargeLevel", "Vehicle"."estimatedRange", "Vehicle"."lastUpdated",
	"Vehicle"."latitudeEnc", "Vehicle"."longitudeEnc"`

// queryVehiclesSharedWithUser is the viewer-side companion of
// queryVehiclesByUserList: identical lean projection plus the grant's RIDE
// CAPABILITY, from which the handler DERIVES VehicleSummary.sharePermission
// without a second lookup (MYR-369).
//
// It selects `s.allow_rides`, NOT `s.permission`. The stored preset is not what
// the grant conveys — an owner may have patched the capability since, and a
// legacy row may still carry the retired live_history — so projecting the preset
// would emit a tier the server no longer enforces and, for that legacy row, one
// it no longer recognizes.
//
// MYR-581 threads the owner-name ladder through here too, and THIS is the role
// the field exists for: an owner reading their own list already knows whose car
// it is, while a viewer had no way at all to learn the name of the person whose
// car they were granted. Emitting it on both roles rather than the viewer alone
// keeps one projection for three producers — the alternative, a viewer-only
// column, would fork the scan.
//
// $2 is an OPTIONAL id filter. NULL means "every vehicle shared with me" (the
// catalog list); a non-null array narrows to a specific set (the redeem
// response, which reports only what that redemption granted). Expressing it as
// one statement keeps the two callers provably on the same access predicate —
// a second copy of this SQL is exactly where an access-control drift would hide.
const queryVehiclesSharedWithUser = `SELECT ` + sharedSummaryColumns + `,
	` + vehicleListHasActiveRideExpr + `,
	gcs.service_etc, gcs.service_expected_end_at,
	` + rideShareEnabledExpr + `,
	` + catalogTrimLabelExpr + `,
	` + catalogOwnerNameExpr + `,
	` + catalogTelemetrySuspendedExpr + `,
	` + setupScheduleColumns + `,
	s.allow_rides
FROM "Vehicle"` + sharedSummaryJoin + `
LEFT JOIN go_vehicle_control_state gcs ON gcs.vehicle_id = "Vehicle"."id"` +
	catalogTelemetrySuspendedJoin + setupScheduleJoin + `
WHERE ($2::text[] IS NULL OR "Vehicle"."id" = ANY($2))
ORDER BY "Vehicle"."name", "Vehicle"."vin"`

// SharedVehicleSummary is a catalog row the caller sees as a VIEWER, carrying
// the ride capability the grant conveys. The join guarantees the grant exists
// AND is live, so the handler never has to invent a default and never has to
// re-check suspension: a suspended grant produced no row to begin with.
type SharedVehicleSummary struct {
	VehicleSummary
	AllowRides bool
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
		v, scanErr := r.scanSharedVehicleSummaryRow(rows)
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

// scanSharedVehicleSummaryRow reads the lean catalog columns plus the grant's
// ride capability.
// A METHOD as of MYR-515, for the same reason as its owner-side twin: the
// projection carries the position ciphertext pair and the scan needs the repo's
// encryptor to resolve it. Both scans go through the SAME summaryGPSScan
// helper, so the viewer's coordinate and the owner's are decrypted identically.
func (r *VehicleRepo) scanSharedVehicleSummaryRow(row rowScanner) (SharedVehicleSummary, error) {
	var (
		v      SharedVehicleSummary
		status string
		gps    summaryGPSScan
		ss     setupScheduleScan
		// Raw ladder answer; reduced to a first name below. Same discipline as
		// the owner-side scan — the full name never lands on the struct.
		ownerName *string
	)
	dests := append([]any{
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
		gps.latDest(),
		gps.lngDest(),
		&v.HasActiveRide,
		&v.ServiceETC,
		&v.ServiceExpectedEndAt,
		&v.RideShareEnabled,
		&v.TrimLabel,
		&v.Trim,
		&ownerName,
		&v.TelemetrySuspendedAt,
	}, ss.dests()...)
	// `allow_rides` is selected AFTER the setup block, so it is appended last —
	// the scan order is the SELECT order, not the struct order.
	dests = append(dests, &v.AllowRides)
	if err := row.Scan(dests...); err != nil {
		return SharedVehicleSummary{}, fmt.Errorf("scan shared vehicle summary: %w", err)
	}
	v.Status = VehicleStatus(status)
	v.SetupSchedule = ss.value()
	v.Latitude, v.Longitude = gps.resolve(r)
	v.OwnerFirstName = ownerFirstNameToken(ownerName)
	return v, nil
}
