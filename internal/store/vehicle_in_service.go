// In-service vehicle listing for the MYR-320 periodic re-poll.

package store

import (
	"context"
	"fmt"
	"time"
)

// queryInServiceVINs lists the VINs of every vehicle currently persisted as
// `in_service`, oldest-updated first so a car that has gone longest without a
// refresh is re-read first when a pass is truncated by the limit.
//
// A READ of the Prisma-owned "Vehicle" table, which needs no carve-out — the
// carve-out rules in data-lifecycle.md §1.4 constrain WRITES. Rows with an empty
// VIN are excluded: the MYR-257 provisioning INSERT can seed a placeholder row
// before Tesla has supplied a VIN, and there is nothing to read from the Fleet
// API for one.
//
// The status literal must equal VehicleStatusInService; it is written out rather
// than interpolated so the statement stays a plain constant string.
const queryInServiceVINs = `
SELECT "vin"
FROM "Vehicle"
WHERE "status" = 'in_service' AND "vin" <> ''
ORDER BY "lastUpdated" ASC NULLS FIRST
LIMIT $1`

// ListInServiceVINs returns up to limit VINs of vehicles currently in service.
//
// Backs the MYR-320 periodic re-poll, which re-runs the connectivity-edge read
// bundle for these cars: a vehicle at a service centre is typically offline for
// days and produces no edge at all, so without this list nothing would ever
// re-read it — and a `service_etc` Tesla publishes mid-visit would be missed
// entirely.
//
// A non-positive limit returns no rows and no error: the caller's config
// defaults already guarantee a positive cap, and refusing to run an unbounded
// scan against the Prisma table is the safer reading of a zero.
func (r *VehicleRepo) ListInServiceVINs(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryInServiceVINs, limit)
	if err != nil {
		r.metrics.IncQueryError("vehicle.list_in_service_vins")
		return nil, fmt.Errorf("VehicleRepo.ListInServiceVINs: %w", err)
	}
	defer rows.Close()

	vins := make([]string, 0, limit)
	for rows.Next() {
		var vin string
		if err := rows.Scan(&vin); err != nil {
			r.metrics.IncQueryError("vehicle.list_in_service_vins")
			return nil, fmt.Errorf("VehicleRepo.ListInServiceVINs: scan: %w", err)
		}
		vins = append(vins, vin)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError("vehicle.list_in_service_vins")
		return nil, fmt.Errorf("VehicleRepo.ListInServiceVINs: rows: %w", err)
	}
	r.metrics.ObserveQueryDuration("vehicle.list_in_service_vins", time.Since(start).Seconds())
	return vins, nil
}
