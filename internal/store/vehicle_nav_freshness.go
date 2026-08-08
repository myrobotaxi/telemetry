// Nav-reading freshness write path (MYR-409).
//
// One column, one statement, one rule: nav_reading_at records when the car last
// told us something about its NAVIGATION, and it only ever moves FORWARD.
//
// It has a dedicated writer rather than a slot in ControlStateUpdate for two
// reasons, following the MYR-316 service-window precedent next door.
//
// The shared queryUpsertControlState is `col = COALESCE(EXCLUDED.col,
// existing.col)` -- last-writer-wins per field. Last-writer-wins is the wrong
// rule for a freshness stamp. The writer coalesces per VIN and flushes on a
// ticker, and two replicas write the same row, so "last writer" is decided by
// scheduling rather than by which observation is actually newer. A stamp that
// can move BACKWARDS makes a genuinely fresh nav reading report stale, and a
// client gate keyed on it flickers off and back on -- the failure mode MYR-485
// names in as many words. GREATEST makes the write monotone, which also makes
// it idempotent and safe to retry.
//
// And the shared upsert is gated on ControlStateUpdate.HasAny(). A pure
// navigation frame carries no owner-control read-back at all, so routing the
// stamp through that gate would drop it for exactly the frames that should set
// it.

package store

import (
	"context"
	"fmt"
	"time"
)

// queryUpsertNavReadingAt records a navigation observation for one vehicle.
//
// GREATEST ignores NULL operands in PostgreSQL, so the DO UPDATE arm collapses
// correctly on a row whose stamp has never been set: GREATEST(new, NULL) is
// new. The INSERT arm is required because a car whose only side-table
// interaction so far is navigation has no row in this table yet.
//
// updated_at is deliberately NOT bumped. It means "when an owner-control
// read-back last landed", the whole table's original purpose, and a car
// streaming a route would otherwise churn it once per flush while no control
// value changed at all.
const queryUpsertNavReadingAt = `
INSERT INTO go_vehicle_control_state (vehicle_id, nav_reading_at)
VALUES ($1, $2)
ON CONFLICT (vehicle_id) DO UPDATE
SET nav_reading_at = GREATEST(EXCLUDED.nav_reading_at, go_vehicle_control_state.nav_reading_at)`

// SetNavReadingAt stamps the vehicle's navigation freshness at readingAt.
//
// Callers pass the flush timestamp of a telemetry frame that actually carried a
// navigation-group member. Passing the current time for a frame that carried
// none is the one way to misuse this method, and it reinstates the exact defect
// MYR-409 closed -- see mapTelemetryToUpdate for where that decision is made.
//
// A zero readingAt is a no-op rather than an error: it means the caller had no
// navigation observation to record, which is the ordinary case for the majority
// of frames.
func (r *VehicleRepo) SetNavReadingAt(ctx context.Context, vehicleID string, readingAt time.Time) error {
	if readingAt.IsZero() {
		return nil
	}
	if _, err := r.pool.Exec(ctx, queryUpsertNavReadingAt, vehicleID, readingAt); err != nil {
		return fmt.Errorf("VehicleRepo.SetNavReadingAt(%s): %w", vehicleID, err)
	}
	return nil
}
