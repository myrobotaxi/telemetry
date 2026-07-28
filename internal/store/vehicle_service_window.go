// Service-window write path (MYR-316).
//
// The two service-window columns on the Go-owned go_vehicle_control_state side
// table (migration 0017) are the ONLY columns in that table with dedicated
// writers rather than a slot in ControlStateUpdate. The reason is NULL.
//
// Every other column reaches the table through queryUpsertControlState, whose
// per-column form is `col = COALESCE(EXCLUDED.col, existing.col)`. That is
// exactly right for telemetry — a frame that does not carry a field must not
// erase it — but it makes a NULL write INEXPRESSIBLE, because a nil pointer
// means "leave alone", not "clear". The service window has to be clearable in
// two ordinary situations:
//
//   - the car leaves in_service, and the wire contract says a
//     driving/parked/charging/offline vehicle NEVER carries a non-null
//     serviceEstimatedEndAt (consumers must never have to age the value out);
//   - the owner submits an empty body to PUT …/service-window to retract an
//     estimate they typed earlier.
//
// So each source gets a statement that ASSIGNS UNCONDITIONALLY, and the two
// stay independent: Tesla arriving late must not erase what the owner typed,
// and Tesla going quiet must fall back to the owner's value rather than to
// null. Emission is COALESCE(service_etc, service_expected_end_at), computed on
// the read side.
//
// Naming/ownership (CG-DL-9): this is the Go-owned side table, so unlike the
// MYR-286 plate writer no Prisma carve-out is involved.

package store

import (
	"context"
	"fmt"
	"time"
)

// queryUpsertServiceETC writes Tesla's own service estimate
// (service_data.service_etc). Assigns unconditionally — a NULL here is a real
// observation ("Tesla has no appointment record for this visit"), not an
// absence of one, and MUST overwrite a stale estimate.
//
// The INSERT arm is required because a car whose only side-table interaction so
// far is a service visit has no row yet.
const queryUpsertServiceETC = `
INSERT INTO go_vehicle_control_state (vehicle_id, service_etc, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (vehicle_id) DO UPDATE
SET service_etc = EXCLUDED.service_etc,
    updated_at  = NOW()`

// queryUpsertServiceExpectedEndAt writes the OWNER-ENTERED "expected back"
// value. Same unconditional-assign rule: a NULL is the owner retracting their
// estimate.
const queryUpsertServiceExpectedEndAt = `
INSERT INTO go_vehicle_control_state (vehicle_id, service_expected_end_at, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (vehicle_id) DO UPDATE
SET service_expected_end_at = EXCLUDED.service_expected_end_at,
    updated_at              = NOW()`

// queryClearServiceWindow drops BOTH sources at once. Used when the monitor
// observes the car leaving service: the visit is over, so neither Tesla's
// estimate nor the owner's guess about that visit means anything any more, and
// leaving either behind would resurrect a stale "expected back" the next time
// the car happened to enter service.
//
// No INSERT arm: with no row there is nothing to clear, and manufacturing an
// all-NULL row would be pure noise.
const queryClearServiceWindow = `
UPDATE go_vehicle_control_state
SET service_etc             = NULL,
    service_expected_end_at = NULL,
    updated_at              = NOW()
WHERE vehicle_id = $1
  AND (service_etc IS NOT NULL OR service_expected_end_at IS NOT NULL)`

// SetServiceETC records Tesla's estimated service completion time for a
// vehicle. A nil etc clears the Tesla-sourced value while leaving any
// owner-entered fallback untouched.
func (r *VehicleRepo) SetServiceETC(ctx context.Context, vehicleID string, etc *time.Time) error {
	if _, err := r.pool.Exec(ctx, queryUpsertServiceETC, vehicleID, etc); err != nil {
		return fmt.Errorf("VehicleRepo.SetServiceETC(%s): %w", vehicleID, err)
	}
	return nil
}

// SetServiceExpectedEndAt records the owner-entered expected service end for a
// vehicle. A nil expectedEndAt clears the owner value while leaving any
// Tesla-sourced estimate untouched.
//
// Ownership is NOT enforced here: the side table has no userId column, so the
// caller (the PUT handler) MUST resolve and verify ownership against the
// Prisma-owned "Vehicle" row before calling. Unlike the plate writer there is
// no second SQL-layer check available to fall back on.
func (r *VehicleRepo) SetServiceExpectedEndAt(ctx context.Context, vehicleID string, expectedEndAt *time.Time) error {
	if _, err := r.pool.Exec(ctx, queryUpsertServiceExpectedEndAt, vehicleID, expectedEndAt); err != nil {
		return fmt.Errorf("VehicleRepo.SetServiceExpectedEndAt(%s): %w", vehicleID, err)
	}
	return nil
}

// ClearServiceWindow drops both the Tesla-sourced and owner-entered service
// estimates for a vehicle, and reports whether anything was actually cleared.
//
// The `false` return is what keeps this cheap to call unconditionally: the
// monitor persists a status on every connectivity edge, and the overwhelming
// majority of those are for cars that were never in service, whose rows already
// hold NULL in both columns. The WHERE clause makes those a no-op that touches
// no row and does not churn updated_at.
func (r *VehicleRepo) ClearServiceWindow(ctx context.Context, vehicleID string) (bool, error) {
	tag, err := r.pool.Exec(ctx, queryClearServiceWindow, vehicleID)
	if err != nil {
		return false, fmt.Errorf("VehicleRepo.ClearServiceWindow(%s): %w", vehicleID, err)
	}
	return tag.RowsAffected() > 0, nil
}
