// Candidate listing and attempt scheduling for the MYR-448 fleet-config
// reconciler.

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
	// LastUpdated is the row's last write of any kind. It is a staleness hint
	// for logging only — the reconciler asks Tesla for the authoritative
	// config state.
	LastUpdated time.Time
	// Status is "Vehicle"."status" — the Prisma column a streamed frame folds a
	// motion value into (MYR-454). Carried since MYR-529 so the reconciler can
	// answer "is this car already streaming?" from the listing row it already
	// holds, with NO Tesla call. That free check is what lets the hot
	// pairing-epoch schedule below examine a freshly paired car every minute
	// without costing Fleet API traffic for the cars that come up on their own.
	Status string
	// AttemptCount is how many consecutive unsuccessful attempts this vehicle
	// has already had; 0 for a car never attempted. Drives the backoff the
	// caller computes when recording the next attempt.
	AttemptCount int
	// LastOutcome is the label recorded by the previous attempt (''
	// for a car never attempted). MYR-489 needs it to tell a FIRST
	// synced-but-quiet observation from a REPEAT one — only the repeat earns
	// an escalation.
	LastOutcome string
	// LastAttemptAt is when that previous attempt ran; zero when never
	// attempted.
	LastAttemptAt time.Time
	// SignedCommandAt is when a signed vehicle command was last successfully
	// APPLIED to this car — proof the virtual key is paired and the car was
	// reachable then. Zero when never observed. Opens the pairing epoch.
	SignedCommandAt time.Time
	// ForcedRepushAt is when the reconciler last performed a forced
	// (delete-then-create) config re-push. Zero when never. Compared against
	// SignedCommandAt to ration escalations to one per epoch.
	ForcedRepushAt time.Time
	// ScheduleCreated is set only by ResetFleetConfigScheduleOnPairing, and only
	// when that call CREATED this vehicle's schedule row rather than resetting
	// an existing one (MYR-517). It distinguishes "a car we were already tracking
	// as broken" from "a car we had never recorded anything about", which is the
	// difference between a signal worth a Tesla round-trip and one worth only a
	// durable epoch stamp. Always false on the periodic candidate listing.
	ScheduleCreated bool
}

// queryFleetConfigCandidates lists owned vehicles that have gone quiet and are
// DUE another attempt, soonest-due first.
//
// Why each predicate is here:
//
//   - `length("vin") = 17` — the MYR-257 provisioning INSERT can seed a row
//     before Tesla supplies a VIN, and there is nothing to push against one.
//     ("lastUpdated" is NOT NULL DEFAULT CURRENT_TIMESTAMP, so a car that has
//     never streamed carries its provisioning timestamp rather than a NULL;
//     there is deliberately no IS NULL arm, which would also defeat an ordered
//     index scan.)
//   - the `"Account"` EXISTS — a config push needs the owner's Tesla OAuth
//     token, so a vehicle whose owner has no tesla Account can never be healed
//     here and must not occupy a slot in the limit. A SEMI-join, not a plain
//     JOIN: the "Account" unique index is (provider, providerAccountId), NOT
//     (userId, provider), so one user may legitimately hold several tesla rows
//     and a plain join would emit the same vehicle once per row — doubling the
//     Tesla calls and eating the budget.
//   - the `go_removed_vehicles` NOT EXISTS — tombstone-wins (MYR-261). Matched
//     on tesla_vehicle_id OR vin, and that OR is load-bearing rather than
//     belt-and-braces: "Vehicle"."teslaVehicleId" is NULLABLE, and a NULL makes
//     `rv.tesla_vehicle_id = v."teslaVehicleId"` evaluate to NULL, which makes
//     NOT EXISTS true and ADMITS the row. A guard that opens when its join key
//     is missing is inverted, and the consequence is resurrecting a car its
//     owner deliberately removed. go_removed_vehicles carries `vin` for exactly
//     this correlation.
//   - the attempt-schedule arm — a car is due when it has never been attempted
//     or its backoff has elapsed. Ordering by OUR schedule rather than by
//     "lastUpdated" is what stops unfixable cars permanently occupying the
//     LIMIT (see migration 0031).
//
// THE STALENESS ARM HAS AN EXEMPTION AS OF MYR-529, AND IT IS THE FIX FOR THE
// BUG THAT ISSUE WAS FILED ABOUT. `v."lastUpdated" < $1` demands thirty minutes
// of total silence on the "Vehicle" row before a car may be examined at all —
// which is correct for the steady state and catastrophic for the first half
// hour after a link, because that is the ONLY window in which virtual-key
// pairing happens. Joey Verrone's car (linked 2026-08-10 01:22Z) proved the key
// paired at 01:23:10 and was scheduled for 01:38, but its "Vehicle" row kept
// being written by ordinary provisioning and web-sync traffic (01:41:33), so
// the staleness arm excluded it from every pass — the schedule said "due" and
// the listing never returned it. A car with a LIVE PAIRING EPOCH
// (`signed_command_at >= $4`) is therefore admitted on its schedule alone. It
// is a strictly narrower exemption than it looks: the epoch is stamped only by
// proof of pairing, it ages out of the hot window in minutes, and the caller's
// first act on such a candidate is a free row-shaped streaming check that costs
// Tesla nothing (see telemetry.reconcileOne).
//
// A READ of the Prisma-owned "Vehicle" and "Account" tables; the
// data-lifecycle.md §1.4 carve-outs constrain WRITES only.
// The MYR-489 columns (last_outcome, last_attempt_at, signed_command_at,
// forced_repush_at) come from the SAME left-joined row the backoff already
// reads, so the escalation decision costs no extra query — and, because it is
// one row, cannot be inconsistent with the attempt_count it is judged against.
const queryFleetConfigCandidates = `
SELECT v."id", v."vin", v."userId", v."lastUpdated", v."status",
       COALESCE(fa.attempt_count, 0),
       COALESCE(fa.last_outcome, ''),
       fa.last_attempt_at, fa.signed_command_at, fa.forced_repush_at
FROM "Vehicle" v
LEFT JOIN go_fleet_config_attempts fa ON fa.vehicle_id = v."id"
WHERE length(v."vin") = 17
  AND (v."lastUpdated" < $1
       OR (fa.signed_command_at IS NOT NULL AND fa.signed_command_at >= $4))
  AND (fa.vehicle_id IS NULL OR fa.next_attempt_at <= $2)
  AND EXISTS (
        SELECT 1
        FROM "Account" a
        WHERE a."userId" = v."userId"
          AND a."provider" = 'tesla'
      )
  AND NOT EXISTS (
        SELECT 1
        FROM go_removed_vehicles rv
        WHERE rv.user_id = v."userId"
          AND (rv.tesla_vehicle_id = v."teslaVehicleId" OR rv.vin = v."vin")
      )
ORDER BY COALESCE(fa.next_attempt_at, TO_TIMESTAMP(0)) ASC, v."lastUpdated" ASC
LIMIT $3`

// ListFleetConfigCandidates returns up to limit vehicles that are due another
// reconcile attempt as of now and are either quiet since cutoff OR inside a
// pairing epoch opened at or after hotSince (MYR-529).
//
// The set is deliberately WIDE — a sleeping but correctly configured car
// matches too — because the cheap authoritative check is a Fleet API config
// read per candidate, not a DB predicate. Narrowing here would risk excluding
// the very cars that are broken.
//
// A zero hotSince disables the pairing-epoch exemption and restores the
// pre-MYR-529 staleness-only behaviour, which is what callers that want a plain
// steady-state sweep should pass.
//
// A non-positive limit returns no rows and no error: refusing to run an
// unbounded scan against the Prisma table is the safer reading of a zero.
func (r *VehicleRepo) ListFleetConfigCandidates(
	ctx context.Context,
	cutoff time.Time,
	now time.Time,
	hotSince time.Time,
	limit int,
) ([]FleetConfigCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	if hotSince.IsZero() {
		// A zero time.Time is year 1, which would admit EVERY row that has ever
		// recorded a pairing epoch. Pushed to the far future instead, so the
		// exemption matches nothing rather than everything — a disabled guard
		// must fail closed.
		hotSince = now.Add(100 * 365 * 24 * time.Hour)
	}
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryFleetConfigCandidates, cutoff, now, limit, hotSince)
	if err != nil {
		r.metrics.IncQueryError("vehicle.list_fleet_config_candidates")
		return nil, fmt.Errorf("VehicleRepo.ListFleetConfigCandidates: %w", err)
	}
	defer rows.Close()

	out := make([]FleetConfigCandidate, 0, limit)
	for rows.Next() {
		var c FleetConfigCandidate
		// The three schedule timestamps are NULL for a car never attempted (no
		// joined row) and, for signed_command_at / forced_repush_at, NULL until
		// the event they name actually happens. Scanned through pointers and
		// left as the zero Time, which every caller reads as "never".
		var lastAttemptAt, signedCommandAt, forcedRepushAt *time.Time
		if err := rows.Scan(&c.VehicleID, &c.VIN, &c.UserID, &c.LastUpdated, &c.Status,
			&c.AttemptCount, &c.LastOutcome,
			&lastAttemptAt, &signedCommandAt, &forcedRepushAt); err != nil {
			r.metrics.IncQueryError("vehicle.list_fleet_config_candidates")
			return nil, fmt.Errorf("VehicleRepo.ListFleetConfigCandidates: scan: %w", err)
		}
		c.LastAttemptAt = derefTime(lastAttemptAt)
		c.SignedCommandAt = derefTime(signedCommandAt)
		c.ForcedRepushAt = derefTime(forcedRepushAt)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError("vehicle.list_fleet_config_candidates")
		return nil, fmt.Errorf("VehicleRepo.ListFleetConfigCandidates: rows: %w", err)
	}
	r.metrics.ObserveQueryDuration("vehicle.list_fleet_config_candidates", time.Since(start).Seconds())
	return out, nil
}

// queryRecordFleetConfigAttempt upserts a vehicle's attempt schedule. The
// count is incremented server-side so two racing passes cannot both read 3 and
// both write 4.
const queryRecordFleetConfigAttempt = `
INSERT INTO go_fleet_config_attempts
    (vehicle_id, attempt_count, last_attempt_at, next_attempt_at, last_outcome)
VALUES ($1, 1, $2, $3, $4)
ON CONFLICT (vehicle_id) DO UPDATE
SET attempt_count   = go_fleet_config_attempts.attempt_count + 1,
    last_attempt_at = EXCLUDED.last_attempt_at,
    next_attempt_at = EXCLUDED.next_attempt_at,
    last_outcome    = EXCLUDED.last_outcome`

// RecordFleetConfigAttempt notes that a vehicle was attempted and will not be
// eligible again until nextAttemptAt. Called for every UNSUCCESSFUL outcome;
// success calls ClearFleetConfigAttempts instead.
//
// outcome is an internal label ('awaiting_virtual_key', 'push_failed', …) —
// never a raw Tesla response body, which can echo a full VIN.
func (r *VehicleRepo) RecordFleetConfigAttempt(
	ctx context.Context,
	vehicleID string,
	attemptedAt, nextAttemptAt time.Time,
	outcome string,
) error {
	start := time.Now()
	_, err := r.pool.Exec(ctx, queryRecordFleetConfigAttempt,
		vehicleID, attemptedAt, nextAttemptAt, outcome)
	if err != nil {
		r.metrics.IncQueryError("vehicle.record_fleet_config_attempt")
		return fmt.Errorf("VehicleRepo.RecordFleetConfigAttempt(%s): %w", vehicleID, err)
	}
	r.metrics.ObserveQueryDuration("vehicle.record_fleet_config_attempt", time.Since(start).Seconds())
	return nil
}

// ClearFleetConfigAttempts drops a vehicle's schedule row after a successful
// push. Success needs no schedule: the car should now stream, and if it ever
// goes quiet again it deserves a fresh attempt count rather than inheriting an
// old backoff.
func (r *VehicleRepo) ClearFleetConfigAttempts(ctx context.Context, vehicleID string) error {
	start := time.Now()
	_, err := r.pool.Exec(ctx, `DELETE FROM go_fleet_config_attempts WHERE vehicle_id = $1`, vehicleID)
	if err != nil {
		r.metrics.IncQueryError("vehicle.clear_fleet_config_attempts")
		return fmt.Errorf("VehicleRepo.ClearFleetConfigAttempts(%s): %w", vehicleID, err)
	}
	r.metrics.ObserveQueryDuration("vehicle.clear_fleet_config_attempts", time.Since(start).Seconds())
	return nil
}
