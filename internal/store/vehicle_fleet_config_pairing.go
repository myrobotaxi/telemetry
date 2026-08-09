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
// vehicle owning vin and makes it immediately due, CREATING the schedule row
// when there is not one yet.
//
// WHY IT BECAME AN UPSERT (MYR-517). It was an UPDATE, on the reasoning that a
// car with no row is "healthy or not yet examined" and so has no backoff to
// reset and nothing to escalate later. The first half of that is true; the
// second half is what prod disproved. A car with no row that is NOT streaming
// is the exact population MYR-489 exists for, and dropping the write meant the
// strongest evidence the system ever receives — a signed command Tesla APPLIED,
// which proves both an enrolled virtual key and a reachable car — was thrown
// away for precisely those cars. Spencer White's onboarding is the worked
// example: no row, so his lock tap stamped nothing, and the pairing epoch that
// arms both the escalation budget and the MYR-491 `configuring` card was never
// opened.
//
// THE ROW IT CREATES IS A SEED, NOT AN ATTEMPT: attempt_count 0, last_outcome
// ” (no claim on the wire), next_attempt_at now (admitted by the candidate
// query at exactly the same instant no row would have been). The ONLY new fact
// it records is signed_command_at, which is the fact worth keeping. Cardinality
// stays bounded by vehicles — the primary key is vehicle_id, so a thousand
// commands against one car still produce one row — and the caller does not turn
// a CREATED row into an immediate Tesla round-trip (see
// telemetry.handlePairingSignal), so this costs one INSERT and no upstream
// traffic.
//
// WHY THE OUTCOME IS NOT FILTERED. Every row in this table is by construction
// an UNSUCCESSFUL attempt or a seed, so "reset only pre-key outcomes" and
// "reset any outcome" differ only for token_failed / read_failed / push_failed
// — and an owner whose signed command just APPLIED has, by that very fact, a
// working token and a reachable car, which is precisely when those three
// deserve another look. Filtering would exclude the cases the evidence most
// clearly contradicts.
//
// THE DEBOUNCE IS LOAD-BEARING. An owner poking their new car sends commands
// in bursts. Without `signed_command_at < $3` a ten-tap burst would trigger ten
// immediate per-vehicle reconciles — ten signed Tesla round-trips — and would
// also keep sliding the epoch forward, which is the timestamp the escalation
// budget is measured against. One reset per debounce window per car. It rides
// the ON CONFLICT ... WHERE clause, so a debounced repeat returns no row and
// reads as "nothing to do" exactly as it did before.
//
// `created` comes back as the xmax test rather than as a second query: on a
// freshly inserted row the system column xmax is 0, and on a row updated by the
// conflict arm it carries the updating transaction. It is the standard way to
// tell an INSERT from an UPDATE inside one statement, and it keeps this a
// single round-trip.
const queryResetFleetConfigScheduleOnPairing = `
WITH veh AS (
    SELECT "id", "vin", "userId", "lastUpdated"
    FROM "Vehicle"
    WHERE "vin" = $1
), ups AS (
    INSERT INTO go_fleet_config_attempts
        (vehicle_id, attempt_count, last_attempt_at, next_attempt_at, last_outcome, signed_command_at)
    SELECT veh."id", 0, $2, $2, '', $2
    FROM veh
    ON CONFLICT (vehicle_id) DO UPDATE
    SET attempt_count     = 0,
        next_attempt_at   = $2,
        signed_command_at = $2
    WHERE go_fleet_config_attempts.signed_command_at IS NULL
       OR go_fleet_config_attempts.signed_command_at < $3
    RETURNING vehicle_id, last_outcome, last_attempt_at, forced_repush_at,
              (xmax = 0) AS created
)
SELECT veh."id", veh."vin", veh."userId", veh."lastUpdated",
       ups.last_outcome, ups.last_attempt_at, ups.forced_repush_at, ups.created
FROM ups JOIN veh ON veh."id" = ups.vehicle_id`

// ResetFleetConfigScheduleOnPairing records that a signed vehicle command was
// successfully applied to vin, clears that vehicle's backoff, and returns the
// candidate so the caller can decide whether to reconcile it immediately.
//
// found is false — with a nil error — when there is nothing to do: the VIN is
// unknown, or the reset was debounced. A vehicle with NO schedule row is no
// longer one of those cases — it gets a seed row and comes back found, with
// ScheduleCreated set so the caller can tell "this car had a live schedule I
// just reset" from "this car had none and now has a seed".
//
// last_attempt_at is deliberately NOT advanced on an existing row. It records
// when we last ASKED TESLA about this car, and a command applied by the owner
// is not that; the escalation gate reads it as "how long has this car been
// quiet since we saw its config", and moving it here would reset that clock on
// every command. A newly created row has never asked Tesla at all, so its
// last_attempt_at is the seed instant — the same convention
// SeedFleetConfigSchedule uses, and ignored by every reader while last_outcome
// is ”.
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
		&c.LastOutcome, &lastAttemptAt, &forcedRepushAt, &c.ScheduleCreated)
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
