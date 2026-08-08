// Pairing-epoch writes for the MYR-489 zero-ops fleet-config self-heal.
//
// MYR-448 gave the reconciler a schedule; MYR-489 gives that schedule a memory
// of WHY it is where it is. Two writes live here, and they are the only two
// that touch the epoch columns added by migration 0036.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// derefTime reads a nullable TIMESTAMPTZ as a time.Time, mapping SQL NULL onto
// the zero Time. Every consumer of the schedule columns treats zero as "never
// observed", which is exactly what NULL means there.
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// queryResetFleetConfigScheduleOnPairing opens a NEW pairing epoch for the
// vehicle owning vin and makes it immediately due.
//
// WHY AN UPDATE AND NOT AN UPSERT. go_fleet_config_attempts is deliberately
// self-draining — a row exists only while a car is failing to stream, and a
// successful push DELETEs it (migration 0031). A car with no row is therefore
// either healthy or not yet examined, and in both cases there is no backoff to
// reset and nothing to escalate later. Inserting one on every applied command
// would turn a table bounded by "cars currently broken" into one bounded by
// lifetime command volume, for no behavioural gain.
//
// WHY THE OUTCOME IS NOT FILTERED. Every row in this table is by construction
// an UNSUCCESSFUL attempt, so "reset only pre-key outcomes" and "reset any
// outcome" differ only for token_failed / read_failed / push_failed — and an
// owner whose signed command just APPLIED has, by that very fact, a working
// token and a reachable car, which is precisely when those three deserve
// another look. Filtering would exclude the cases the evidence most clearly
// contradicts.
//
// THE DEBOUNCE IS LOAD-BEARING. An owner poking their new car sends commands
// in bursts. Without `signed_command_at < $3` a ten-tap burst would trigger ten
// immediate per-vehicle reconciles — ten signed Tesla round-trips — and would
// also keep sliding the epoch forward, which is the timestamp the escalation
// budget is measured against. One reset per debounce window per car.
const queryResetFleetConfigScheduleOnPairing = `
UPDATE go_fleet_config_attempts fa
SET attempt_count   = 0,
    next_attempt_at = $2,
    signed_command_at = $2
FROM "Vehicle" v
WHERE v."vin" = $1
  AND fa.vehicle_id = v."id"
  AND (fa.signed_command_at IS NULL OR fa.signed_command_at < $3)
RETURNING v."id", v."vin", v."userId", v."lastUpdated",
          fa.last_outcome, fa.last_attempt_at, fa.forced_repush_at`

// ResetFleetConfigScheduleOnPairing records that a signed vehicle command was
// successfully applied to vin, clears that vehicle's backoff, and returns the
// candidate so the caller can reconcile it immediately.
//
// found is false — with a nil error — when there is nothing to do: the vehicle
// has no schedule row (healthy, or never examined), the VIN is unknown, or the
// reset was debounced. Callers treat all three identically.
//
// last_attempt_at is deliberately NOT advanced. It records when we last ASKED
// TESLA about this car, and a command applied by the owner is not that; the
// escalation gate reads it as "how long has this car been quiet since we saw
// its config", and moving it here would reset that clock on every command.
func (r *VehicleRepo) ResetFleetConfigScheduleOnPairing(
	ctx context.Context,
	vin string,
	now time.Time,
	debounceBefore time.Time,
) (FleetConfigCandidate, bool, error) {
	start := time.Now()
	row := r.pool.QueryRow(ctx, queryResetFleetConfigScheduleOnPairing, vin, now, debounceBefore)

	var c FleetConfigCandidate
	var lastAttemptAt, forcedRepushAt *time.Time
	err := row.Scan(&c.VehicleID, &c.VIN, &c.UserID, &c.LastUpdated,
		&c.LastOutcome, &lastAttemptAt, &forcedRepushAt)
	if errors.Is(err, pgx.ErrNoRows) {
		r.metrics.ObserveQueryDuration("vehicle.reset_fleet_config_on_pairing", time.Since(start).Seconds())
		return FleetConfigCandidate{}, false, nil
	}
	if err != nil {
		r.metrics.IncQueryError("vehicle.reset_fleet_config_on_pairing")
		return FleetConfigCandidate{}, false, fmt.Errorf("VehicleRepo.ResetFleetConfigScheduleOnPairing: %w", err)
	}

	c.AttemptCount = 0 // just reset by the UPDATE above
	c.LastAttemptAt = derefTime(lastAttemptAt)
	c.SignedCommandAt = now
	c.ForcedRepushAt = derefTime(forcedRepushAt)

	r.metrics.ObserveQueryDuration("vehicle.reset_fleet_config_on_pairing", time.Since(start).Seconds())
	return c, true, nil
}

// queryRecordForcedFleetConfigRepush records an attempt AND stamps the epoch
// budget as spent.
//
// It is an upsert for the same reason RecordFleetConfigAttempt is — the row is
// guaranteed to exist in practice (a forced re-push only ever follows a
// recorded synced-not-streaming attempt) but a schedule write must not depend
// on that to avoid silently losing the spend.
//
// attempt_count still INCREMENTS. That is the whole anti-loop story: the forced
// path does not clear the schedule the way an ordinary successful push does, so
// even if the car stays silent the next attempt is a normal backed-off one and
// forced_repush_at now blocks a second force until a new epoch opens.
const queryRecordForcedFleetConfigRepush = `
INSERT INTO go_fleet_config_attempts
    (vehicle_id, attempt_count, last_attempt_at, next_attempt_at, last_outcome, forced_repush_at)
VALUES ($1, 1, $2, $3, $4, $2)
ON CONFLICT (vehicle_id) DO UPDATE
SET attempt_count    = go_fleet_config_attempts.attempt_count + 1,
    last_attempt_at  = EXCLUDED.last_attempt_at,
    next_attempt_at  = EXCLUDED.next_attempt_at,
    last_outcome     = EXCLUDED.last_outcome,
    forced_repush_at = EXCLUDED.forced_repush_at`

// RecordForcedFleetConfigRepush notes that the reconciler spent this vehicle's
// one forced re-push for the current pairing epoch, and schedules the next
// ordinary attempt for nextAttemptAt.
//
// outcome is an internal label, never a raw Tesla body.
func (r *VehicleRepo) RecordForcedFleetConfigRepush(
	ctx context.Context,
	vehicleID string,
	attemptedAt, nextAttemptAt time.Time,
	outcome string,
) error {
	start := time.Now()
	_, err := r.pool.Exec(ctx, queryRecordForcedFleetConfigRepush,
		vehicleID, attemptedAt, nextAttemptAt, outcome)
	if err != nil {
		r.metrics.IncQueryError("vehicle.record_forced_fleet_config_repush")
		return fmt.Errorf("VehicleRepo.RecordForcedFleetConfigRepush(%s): %w", vehicleID, err)
	}
	r.metrics.ObserveQueryDuration("vehicle.record_forced_fleet_config_repush", time.Since(start).Seconds())
	return nil
}
