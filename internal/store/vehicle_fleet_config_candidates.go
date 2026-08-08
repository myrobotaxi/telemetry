// Candidate listing for the MYR-448 fleet-config reconciler.

package store

import (
	"context"
	"fmt"
	"time"
)

// FleetConfigCandidate is one owned vehicle that looks like it is NOT
// streaming and therefore may be missing its fleet-telemetry config.
type FleetConfigCandidate struct {
	VehicleID string
	VIN       string
	UserID    string
	// LastUpdated is the row's last write of any kind; zero when the car has
	// never produced one. It is a staleness hint for logging only — the
	// reconciler asks Tesla for the authoritative config state.
	LastUpdated time.Time
}

// queryFleetConfigCandidates lists owned vehicles whose row has gone quiet for
// longer than the caller's staleness window, oldest (and never-seen) first so
// the worst-off car is healed first when a pass is truncated by the limit.
//
// Why each predicate is here:
//
//   - `length("vin") = 17` — the MYR-257 provisioning INSERT can seed a row
//     before Tesla supplies a VIN, and there is nothing to push against one.
//   - the `"Account"` join — a config push needs the owner's Tesla OAuth
//     token. A vehicle whose owner has no tesla Account row can never be
//     healed by this reconciler, so it must not occupy a slot in the limit.
//   - the `go_removed_vehicles` NOT EXISTS — tombstone-wins (MYR-261). A car
//     the owner deliberately removed must never be resurrected by a
//     background job; only a deliberate re-add (MYR-262) clears the tombstone.
//   - `"lastUpdated" < cutoff` — a streaming car writes every ~25s, so a row
//     quiet for the staleness window is either asleep or (the case this exists
//     for) was never configured to stream at all. The column is NOT NULL
//     DEFAULT CURRENT_TIMESTAMP, so a car that has never streamed carries the
//     timestamp of its own provisioning INSERT rather than a NULL — the IS NULL
//     arm is kept only as defence against a schema that later relaxes that.
//     The window doubles as a grace period, keeping the reconciler off cars
//     whose link-time push is still in flight.
//
// A READ of the Prisma-owned "Vehicle" and "Account" tables; the
// data-lifecycle.md §1.4 carve-outs constrain WRITES only.
const queryFleetConfigCandidates = `
SELECT v."id", v."vin", v."userId", v."lastUpdated"
FROM "Vehicle" v
JOIN "Account" a
  ON a."userId" = v."userId"
 AND a."provider" = 'tesla'
WHERE length(v."vin") = 17
  AND (v."lastUpdated" IS NULL OR v."lastUpdated" < $1)
  AND NOT EXISTS (
        SELECT 1
        FROM go_removed_vehicles rv
        WHERE rv.user_id = v."userId"
          AND rv.tesla_vehicle_id = v."teslaVehicleId"
      )
ORDER BY v."lastUpdated" ASC NULLS FIRST
LIMIT $2`

// ListFleetConfigCandidates returns up to limit vehicles that have not written
// a row since cutoff and so may be missing their fleet-telemetry config.
//
// Backs the MYR-448 reconciler. The set is deliberately WIDE — a sleeping but
// correctly configured car matches too — because the cheap authoritative check
// is a Fleet API config read per candidate, not a DB predicate. Narrowing here
// would risk excluding the very cars that are broken.
//
// A non-positive limit returns no rows and no error: refusing to run an
// unbounded scan against the Prisma table is the safer reading of a zero.
func (r *VehicleRepo) ListFleetConfigCandidates(
	ctx context.Context,
	cutoff time.Time,
	limit int,
) ([]FleetConfigCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryFleetConfigCandidates, cutoff, limit)
	if err != nil {
		r.metrics.IncQueryError("vehicle.list_fleet_config_candidates")
		return nil, fmt.Errorf("VehicleRepo.ListFleetConfigCandidates: %w", err)
	}
	defer rows.Close()

	out := make([]FleetConfigCandidate, 0, limit)
	for rows.Next() {
		var c FleetConfigCandidate
		var lastUpdated *time.Time
		if err := rows.Scan(&c.VehicleID, &c.VIN, &c.UserID, &lastUpdated); err != nil {
			r.metrics.IncQueryError("vehicle.list_fleet_config_candidates")
			return nil, fmt.Errorf("VehicleRepo.ListFleetConfigCandidates: scan: %w", err)
		}
		if lastUpdated != nil {
			c.LastUpdated = *lastUpdated
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError("vehicle.list_fleet_config_candidates")
		return nil, fmt.Errorf("VehicleRepo.ListFleetConfigCandidates: rows: %w", err)
	}
	r.metrics.ObserveQueryDuration("vehicle.list_fleet_config_candidates", time.Since(start).Seconds())
	return out, nil
}
