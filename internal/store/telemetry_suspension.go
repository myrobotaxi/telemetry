// The EPISODE store for owner-inactivity telemetry suspension (MYR-592):
// go_vehicle_telemetry_suspensions (migration 0044), plus the one candidate
// query that decides which cars the sweeper looks at.
//
// ── THE POLICY THIS SERVES ──────────────────────────────────────────────────
//
// Tesla bills Fleet Telemetry streaming PER VEHICLE. A car whose owner stopped
// opening the app streams frames nobody reads, every day, forever. The client's
// cost-control ruling (rest-api.md §7.27):
//
//   * FOUR days of owner silence → one warning push to the owner.
//   * FIVE days → remove the vehicle's fleet-telemetry config. Nothing else:
//     the OAuth grant, the virtual key, the vehicle row and every share survive,
//     which is exactly what makes the owner's re-connect a single call.
//
// ── STRICTLY THE OWNER'S ACTIVITY ───────────────────────────────────────────
//
// EXPLICIT CLIENT RULING, and the reason the candidate query joins
// go_user_activity on `"Vehicle"."userId"` and on nothing else: a RIDER's or a
// VIEWER's usage does NOT defer suspension. A shared car whose owner has been
// away five days is suspended even while a rider is actively watching it. That
// is not an oversight to be "fixed" by widening the join — the platform is
// paying for a stream on the OWNER's behalf, and only the owner can decide to
// keep paying for it. Widening this join to any other party silently reverses
// the client's decision.
//
// ── WHY "CONFIGURED" IS A NEGATIVE PREDICATE ────────────────────────────────
//
// There is no `telemetry_configured BOOLEAN` anywhere in this schema; the state
// is inferred from go_fleet_config_attempts, which is SELF-DRAINING — the
// reconciler DELETES a vehicle's row the moment a config push succeeds
// (migration 0031). So "no row" means the push landed, and the only positive
// evidence of a MISSING config is a surviving row carrying one of the labels
// that says the push did not take. Those labels are enumerated in
// fleetConfigAbsentOutcomes below.
//
// Getting this wrong is cheap in one direction and expensive in the other, which
// is why the predicate is written as an exclusion. A car we wrongly think is
// configured costs one pointless (idempotent, already-best-effort) Tesla DELETE
// and one warning push about a car that was not streaming. A car we wrongly
// think is UNconfigured is never suspended and keeps billing forever, which is
// the entire failure the feature exists to prevent.
//
// P0 in full (data-classification.md §1.22). Opaque cuids and timestamps.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fleetConfigAbsentOutcomes are the go_fleet_config_attempts.last_outcome labels
// that mean NO fleet-telemetry config is installed for the vehicle.
//
// Spelled as a SQL literal list rather than built from the store-side
// SetupOutcome* constants because this is a `const` statement, and the two
// spellings are held together by TestFleetConfigAbsentOutcomesCoverSetupLabels.
//
// THE EMPTY LABEL IS DELIBERATELY ABSENT from this list. SetupOutcomeNone (the
// empty string) is what the link-time hook seeds when the push APPLIED —
// owner_stream_hook_config.go returns it on the nil-error arm — so a car
// carrying it is configured and billing. Listing the empty label here would
// exempt exactly the healthiest cars in the fleet and quietly disable the
// feature for most of it.
const fleetConfigAbsentOutcomes = `('awaiting_virtual_key', 'push_failed', 'token_failed', 'read_failed', 'skipped_other')`

// InactiveOwnerVehicle is one candidate row: a configured, unsuspended vehicle
// whose owner has not authenticated since the warning threshold.
//
// It carries the raw facts and NO decision. The warn-or-suspend choice is made
// in Go by the sweeper against its own clock, so that the whole policy is
// testable without a database and so that widening a threshold never means
// editing SQL.
type InactiveOwnerVehicle struct {
	// VehicleID is the Prisma "Vehicle"."id" cuid — the key the episode row,
	// the fleet-config schedule and the §7.0 catalog all use.
	VehicleID string
	// VIN is what the Tesla config DELETE is addressed to. Redact in logs.
	VIN string
	// OwnerID is who gets the warning push. The rider is never notified: the
	// suspension is not their decision and not their bill.
	OwnerID string
	// VehicleName is the owner's own nickname for the car, for the push copy.
	// May be empty; push.vehicleLabel supplies the "Your car" fallback.
	VehicleName string
	// OwnerLastSeenAt is the stamped instant from go_user_activity, up to the
	// stamper's throttle window stale — always in the conservative direction.
	OwnerLastSeenAt time.Time
	// WarnedAt is nil until this episode's day-4 push has been handed off.
	WarnedAt *time.Time
}

// queryInactiveOwnerVehicles selects configured, not-yet-suspended vehicles
// whose owner has been silent since $1.
//
// $1 = the WARN threshold (now - 4d), $2 = row cap.
//
// THE JOIN TO go_user_activity IS INNER, AND THAT IS THE SAFETY PROPERTY. An
// owner with no row has never been observed — a brand-new account, or any
// account that has not authenticated since migration 0043 shipped — which is
// materially different from an owner observed to be absent. An outer join would
// read a NULL last_seen_at as infinitely stale and suspend the entire fleet on
// the first pass after deploy. The inner join makes that outcome unreachable
// rather than merely unlikely: a vehicle becomes a candidate only once we hold
// positive evidence about its owner.
//
// ALREADY-SUSPENDED VEHICLES ARE EXCLUDED, which is what makes the sweeper
// idempotent. There is no second action to take on them, they must not be warned
// again, and they must not cost a Tesla call every hour for the rest of time.
// They leave this predicate only when the owner reconnects (which deletes the
// row) or unlinks (which deletes it with the car).
const queryInactiveOwnerVehicles = `
SELECT v."id", v."vin", v."userId", v."name", a.last_seen_at, s.warned_at
FROM "Vehicle" v
JOIN go_user_activity a
  ON a.user_id = v."userId"
LEFT JOIN go_vehicle_telemetry_suspensions s
  ON s.vehicle_id = v."id"
LEFT JOIN go_fleet_config_attempts fca
  ON fca.vehicle_id = v."id"
WHERE a.last_seen_at <= $1
  AND v."vin" IS NOT NULL
  AND v."vin" <> ''
  AND s.suspended_at IS NULL
  AND (fca.vehicle_id IS NULL OR fca.last_outcome NOT IN ` + fleetConfigAbsentOutcomes + `)
ORDER BY a.last_seen_at ASC
LIMIT $2`

// queryClearReturnedOwnerEpisodes deletes every OPEN episode whose owner has
// come back. $1 = the warn threshold.
//
// "OPEN" means suspended_at IS NULL, and the asymmetry is the contract's, not a
// convenience. An owner who returns before the fifth day was never disconnected,
// so their episode simply ends and the next one starts from a clean warning. An
// owner who returns AFTER suspension finds a car whose config is gone at Tesla;
// no amount of app usage puts it back, and auto-clearing the stamp here would
// erase the disconnect notice the contract requires the client to show while
// leaving the car just as silent. Only the §7.28 reconnect clears a suspended
// episode, and it clears it only after the config re-create has succeeded.
const queryClearReturnedOwnerEpisodes = `
DELETE FROM go_vehicle_telemetry_suspensions s
USING "Vehicle" v, go_user_activity a
WHERE s.vehicle_id = v."id"
  AND a.user_id = v."userId"
  AND s.suspended_at IS NULL
  AND a.last_seen_at > $1`

// queryMarkTelemetryWarned records that this episode's day-4 push has gone out.
// COALESCE keeps the FIRST warning's instant on a re-run, so a retry cannot
// backdate or advance the notice the owner actually received.
const queryMarkTelemetryWarned = `
INSERT INTO go_vehicle_telemetry_suspensions (vehicle_id, warned_at)
VALUES ($1, $2)
ON CONFLICT (vehicle_id) DO UPDATE
   SET warned_at = COALESCE(go_vehicle_telemetry_suspensions.warned_at, EXCLUDED.warned_at)`

// queryMarkTelemetrySuspended stamps the instant the config was removed.
// COALESCE for the same reason as the warning: the first stamp is the truth, and
// it is the value the §7.0 catalog emits, so a re-run must not move it.
const queryMarkTelemetrySuspended = `
INSERT INTO go_vehicle_telemetry_suspensions (vehicle_id, suspended_at)
VALUES ($1, $2)
ON CONFLICT (vehicle_id) DO UPDATE
   SET suspended_at = COALESCE(go_vehicle_telemetry_suspensions.suspended_at, EXCLUDED.suspended_at)`

// queryClearTelemetrySuspension removes a vehicle's episode outright — both the
// warning bookkeeping and the suspension stamp. The reconnect endpoint's last
// statement, and the teardown's.
const queryClearTelemetrySuspension = `DELETE FROM go_vehicle_telemetry_suspensions WHERE vehicle_id = $1`

// queryTelemetrySuspendedAt reads one vehicle's suspension stamp.
const queryTelemetrySuspendedAt = `SELECT suspended_at FROM go_vehicle_telemetry_suspensions WHERE vehicle_id = $1`

// TelemetrySuspensionRepo owns go_vehicle_telemetry_suspensions and the
// inactivity candidate read.
type TelemetrySuspensionRepo struct {
	pool *pgxpool.Pool
}

// NewTelemetrySuspensionRepo wires the repo to a pool.
func NewTelemetrySuspensionRepo(pool *pgxpool.Pool) *TelemetrySuspensionRepo {
	return &TelemetrySuspensionRepo{pool: pool}
}

// ListInactiveOwnerVehicles returns up to limit candidates whose owner has been
// silent since warnBefore, oldest silence first.
//
// The ordering is not cosmetic: with a row cap, oldest-first guarantees that the
// cars closest to (or past) the suspension threshold are examined before the
// ones merely approaching the warning, so a backlog delays warnings rather than
// suspensions.
func (r *TelemetrySuspensionRepo) ListInactiveOwnerVehicles(
	ctx context.Context, warnBefore time.Time, limit int,
) ([]InactiveOwnerVehicle, error) {
	rows, err := r.pool.Query(ctx, queryInactiveOwnerVehicles, warnBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("store.TelemetrySuspensionRepo.ListInactiveOwnerVehicles: query: %w", err)
	}
	defer rows.Close()

	var out []InactiveOwnerVehicle
	for rows.Next() {
		var v InactiveOwnerVehicle
		if err := rows.Scan(&v.VehicleID, &v.VIN, &v.OwnerID, &v.VehicleName, &v.OwnerLastSeenAt, &v.WarnedAt); err != nil {
			return nil, fmt.Errorf("store.TelemetrySuspensionRepo.ListInactiveOwnerVehicles: scan: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.TelemetrySuspensionRepo.ListInactiveOwnerVehicles: rows: %w", err)
	}
	return out, nil
}

// ClearReturnedOwnerEpisodes deletes open episodes whose owner authenticated
// after warnBefore. Returns how many were cleared, for the pass log.
func (r *TelemetrySuspensionRepo) ClearReturnedOwnerEpisodes(ctx context.Context, warnBefore time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, queryClearReturnedOwnerEpisodes, warnBefore)
	if err != nil {
		return 0, fmt.Errorf("store.TelemetrySuspensionRepo.ClearReturnedOwnerEpisodes: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MarkWarned records that the day-4 push has been handed to the notifier.
func (r *TelemetrySuspensionRepo) MarkWarned(ctx context.Context, vehicleID string, at time.Time) error {
	if _, err := r.pool.Exec(ctx, queryMarkTelemetryWarned, vehicleID, at); err != nil {
		return fmt.Errorf("store.TelemetrySuspensionRepo.MarkWarned(%s): %w", vehicleID, err)
	}
	return nil
}

// MarkSuspended stamps the instant the fleet-telemetry config was removed.
// Call it only AFTER the Tesla-side delete has returned without error — see
// migration 0044 for why the other order is the one unexplainable state.
func (r *TelemetrySuspensionRepo) MarkSuspended(ctx context.Context, vehicleID string, at time.Time) error {
	if _, err := r.pool.Exec(ctx, queryMarkTelemetrySuspended, vehicleID, at); err != nil {
		return fmt.Errorf("store.TelemetrySuspensionRepo.MarkSuspended(%s): %w", vehicleID, err)
	}
	return nil
}

// ClearForVehicle removes the vehicle's episode entirely. Idempotent; a missing
// row is success, because both callers (reconnect, teardown) run against cars
// that may never have had one.
func (r *TelemetrySuspensionRepo) ClearForVehicle(ctx context.Context, vehicleID string) error {
	if _, err := r.pool.Exec(ctx, queryClearTelemetrySuspension, vehicleID); err != nil {
		return fmt.Errorf("store.TelemetrySuspensionRepo.ClearForVehicle(%s): %w", vehicleID, err)
	}
	return nil
}

// SuspendedAt reads one vehicle's suspension stamp. The bool is false both when
// no episode row exists and when the episode is open (warned but not suspended)
// — from a reader's point of view those are the same state, "streaming is
// configured", which is exactly what the catalog emits as null.
func (r *TelemetrySuspensionRepo) SuspendedAt(ctx context.Context, vehicleID string) (time.Time, bool, error) {
	var at *time.Time
	if err := r.pool.QueryRow(ctx, queryTelemetrySuspendedAt, vehicleID).Scan(&at); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("store.TelemetrySuspensionRepo.SuspendedAt(%s): %w", vehicleID, err)
	}
	if at == nil {
		return time.Time{}, false, nil
	}
	return *at, true, nil
}
