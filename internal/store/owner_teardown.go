package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OwnerTeardown is the single, audited, transactional path that removes ONE
// of an owner's cars across our backend (MYR-258,
// docs/architecture/car-offboarding.md). It is the exact inverse of
// store.OwnerProvisioner (MYR-257): where the provisioner upserts the
// Vehicle/Account/Settings identity rows on a completed Tesla link, the
// teardown deletes them on an owner "Remove this car".
//
// Like the provisioner it is a distinct, narrowly-scoped writer — NOT the
// identity module (ADR-001 §4 keeps identity's "User" access read-only; the
// User row is NEVER touched here) and NOT AccountRepo. It is gated behind the
// same contract carve-out: docs/contracts/data-lifecycle.md §1.4 grants Go
// owner-scoped DELETE on Vehicle/Account + the user-initiated vehicle_deleted
// AuditLog INSERT, and contract-guard CG-DL-3 requires that audit row inside
// the same transaction as the delete.
//
// Ownership is enforced at the SQL layer: every mutating statement is scoped
// `WHERE "userId" = <caller>`, so a teardown can NEVER touch another user's
// rows even if the HTTP layer's ownership check were bypassed. A single
// pgx.Tx makes the whole teardown atomic (CG-DL-3 / data-lifecycle §3.4): on
// any error nothing is deleted and no audit row is written. It is idempotent
// — removing an already-removed car affects zero rows and is a clean no-op
// success (no duplicate audit row).
type OwnerTeardown struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewOwnerTeardown builds the teardown writer over the given pool.
func NewOwnerTeardown(pool *pgxpool.Pool, logger *slog.Logger) *OwnerTeardown {
	if logger == nil {
		logger = slog.Default()
	}
	return &OwnerTeardown{pool: pool, logger: logger}
}

// TeardownResult is the log-safe (P0) outcome of a RemoveVehicle call.
type TeardownResult struct {
	// Removed is true when this call deleted the vehicle row.
	Removed bool
	// AlreadyGone is true when the vehicle did not exist for this owner
	// (idempotent no-op — no delete, no audit row).
	AlreadyGone bool
	// WasLastVehicle is true when the removed vehicle was the owner's last
	// one, so the Tesla Account tokens were cleared and Settings reset.
	WasLastVehicle bool
	// TeslaTokensCleared mirrors WasLastVehicle: the Account row (provider
	// 'tesla') was deleted only on a last-vehicle removal.
	TeslaTokensCleared bool
	// DriveCount is the number of Drive rows that cascaded away with the
	// vehicle (P0 count; recorded in the audit metadata).
	DriveCount int
	// Tombstoned is true when a removed-vehicle tombstone was written for this
	// car (MYR-261) — i.e. the vehicle had a Tesla vehicle id, so a future
	// re-link sync must not resurrect it. False for a car with no Tesla vehicle
	// id (nothing a Fleet-API sync could re-add).
	Tombstoned bool
}

// queryTeardownLockOwnerVehicles reads AND row-locks the owner's entire
// vehicle set (`FOR UPDATE`) in one shot. This is the load-bearing
// concurrency guard: two teardowns for the SAME owner serialize on these
// locks, so the last-vehicle decision below cannot race (a plain unlocked
// count at Read Committed let two concurrent removals of different cars each
// see count==2, neither clear the Account → orphaned tokens). The returned id
// set gives both the owner-scoped existence check (target ∈ set) and the
// last-vehicle test (len==1), with no second query.
const queryTeardownLockOwnerVehicles = `
SELECT "id" FROM "Vehicle" WHERE "userId" = $1 FOR UPDATE`

// queryTeardownDriveCount counts the drives that will cascade with the
// vehicle. Read before the delete so the count lands in the audit metadata.
const queryTeardownDriveCount = `
SELECT count(*) FROM "Drive" WHERE "vehicleId" = $1`

// queryTeardownVehicleIdentity reads the removed vehicle's Tesla identity so a
// removed-vehicle tombstone can be written (MYR-261). Both columns are nullable
// in the Prisma schema (a car may have no linked Tesla vehicle id yet), so they
// scan into pointers. Read while the row is still FOR UPDATE-locked, before the
// delete.
const queryTeardownVehicleIdentity = `
SELECT "teslaVehicleId", "vin" FROM "Vehicle" WHERE "id" = $1`

// queryTeardownDeleteRideRequests deletes the Go-owned ride-request rows for
// the vehicle. These hold P1 encrypted pickup/dropoff GPS + passenger PII and
// have NO FK to "Vehicle" (CG-DL-9 forbids a Go migration FK-referencing the
// Prisma-owned table), so the Vehicle cascade does NOT reach them — a
// "complete removal" must delete them explicitly. go_ride_requests is the
// only Go-owned table keyed by vehicle_id (0002_ride_requests migration).
const queryTeardownDeleteRideRequests = `
DELETE FROM go_ride_requests WHERE vehicle_id = $1`

// queryTeardownDeleteVehicle deletes the target vehicle, owner-scoped. The
// Prisma FKs cascade Drive / TripStop / vehicle-scoped Invite / the encrypted
// route blobs, and the Next.js-owned AFTER DELETE trigger fires the
// `vehicle_deleted` NOTIFY that closes WS/mTLS streams + evicts caches
// (internal/store/notify_listener.go → vehicleDeletedDispatcher).
const queryTeardownDeleteVehicle = `
DELETE FROM "Vehicle" WHERE "id" = $1 AND "userId" = $2`

// queryTeardownDeleteAccount clears the owner's Tesla OAuth tokens on a
// last-vehicle removal. Owner+provider scoped. This removes OUR access; it
// does NOT revoke the grant at Tesla (owner-confirmed consent page — §1.2).
// #nosec G101 -- column/predicate SQL, not a credential (gosec greps the
// literal 'tesla' + 'Account' and can misflag it).
const queryTeardownDeleteAccount = `
DELETE FROM "Account" WHERE "userId" = $1 AND "provider" = 'tesla'`

// queryTeardownResetSettings clears the link/pairing flags on a last-vehicle
// removal (mirrors the web unlinkTesla). Upsert because an owner may have no
// Settings row yet; only the link/pairing columns are touched.
const queryTeardownResetSettings = `
INSERT INTO "Settings" ("id", "userId", "teslaLinked", "virtualKeyPaired", "keyPairingReminderCount", "updatedAt")
VALUES ($1, $2, FALSE, FALSE, 0, NOW())
ON CONFLICT ("userId") DO UPDATE
SET "teslaLinked" = FALSE, "virtualKeyPaired" = FALSE, "keyPairingReminderCount" = 0, "updatedAt" = NOW()`

// teardownAuditMetadata is the P0-only audit metadata (CG-DL-5): opaque
// counts + an enum-ish bool, never PII/coords/tokens.
type teardownAuditMetadata struct {
	DriveCount     int  `json:"driveCount"`
	WasLastVehicle bool `json:"wasLastVehicle"`
	// Tombstoned records whether a removed-vehicle tombstone was written in this
	// same transaction (MYR-261). It documents the tombstone create on the
	// existing user-initiated vehicle_deleted row rather than emitting a second
	// audit row for the same targetId.
	Tombstoned bool `json:"tombstoned"`
}

// RemoveVehicle removes a single vehicle owned by userID, transactionally,
// idempotently, and ISOLATED against concurrent same-owner teardowns. Sequence
// (one tx):
//
//  1. `SELECT … FOR UPDATE` the owner's vehicle set — row locks serialize
//     concurrent same-owner teardowns, so the last-vehicle test is race-safe
//     (not merely atomic). Target ∉ set → clean no-op success.
//  2. wasLast = the target is the only vehicle in the locked set; read the
//     cascade drive count for the audit metadata.
//  3. INSERT the user-initiated `vehicle_deleted` audit row (CG-DL-3 requires
//     the audit BEFORE the destructive delete, same tx).
//  4. DELETE the Go-owned go_ride_requests rows (P1 GPS + passenger PII, no FK
//     cascade), then DELETE the vehicle (owner-scoped; cascades Drive/TripStop/
//     Invite/route blobs + fires `vehicle_deleted` NOTIFY).
//  5. On a last-vehicle removal: DELETE the Tesla Account tokens + reset the
//     Settings link/pairing flags.
//
// It never touches the User row and never touches another user's rows.
func (t *OwnerTeardown) RemoveVehicle(ctx context.Context, userID, vehicleID string) (TeardownResult, error) {
	if strings.TrimSpace(userID) == "" {
		return TeardownResult{}, fmt.Errorf("store.RemoveVehicle: empty user id")
	}
	if strings.TrimSpace(vehicleID) == "" {
		return TeardownResult{}, fmt.Errorf("store.RemoveVehicle(user=%s): empty vehicle id", userID)
	}

	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return TeardownResult{}, fmt.Errorf("store.RemoveVehicle(user=%s): begin: %w", userID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	// (1) Lock the owner's whole vehicle set. FOR UPDATE serializes concurrent
	// same-owner teardowns so the last-vehicle decision below cannot race. The
	// locked set gives both the existence check and the last-vehicle test.
	ownerCount, exists, err := lockOwnerVehicleSet(ctx, tx, userID, vehicleID)
	if err != nil {
		return TeardownResult{}, err
	}
	if !exists {
		// Already gone (or never owned by this user). Commit the empty tx and
		// report a clean no-op — no delete, no audit row (avoids duplicate
		// audit rows on re-run; car-offboarding.md §5.3).
		if err := tx.Commit(ctx); err != nil {
			return TeardownResult{}, fmt.Errorf("store.RemoveVehicle(user=%s): commit no-op: %w", userID, err)
		}
		return TeardownResult{AlreadyGone: true}, nil
	}

	// (2) The target is in the locked set, so a set size of 1 means it is the
	// owner's last vehicle. Read the cascade drive count for the audit metadata.
	wasLast := ownerCount == 1
	var driveCount int
	if err := tx.QueryRow(ctx, queryTeardownDriveCount, vehicleID).Scan(&driveCount); err != nil {
		return TeardownResult{}, fmt.Errorf("store.RemoveVehicle(user=%s): drive count: %w", userID, err)
	}

	// (2a) Read the car's Tesla identity (still FOR UPDATE-locked) so we can
	// tombstone the removed VIN (MYR-261). A car with no Tesla vehicle id has
	// nothing a Fleet-API sync could resurrect, so tombstoning is skipped for it.
	teslaVehicleID, vin, err := readVehicleTeslaIdentity(ctx, tx, userID, vehicleID)
	if err != nil {
		return TeardownResult{}, err
	}
	tombstone := teslaVehicleID != ""

	// (3) Audit FIRST (CG-DL-3: the audit row must be written before the
	// destructive delete so it captures the action even if the delete fails).
	// The tombstone create is recorded on this same vehicle_deleted row.
	if err := t.insertAudit(ctx, tx, userID, vehicleID, driveCount, wasLast, tombstone); err != nil {
		return TeardownResult{}, err
	}

	// (4-5) Apply the destructive mutations (ride-requests + vehicle delete,
	// removed-vehicle tombstone, last-vehicle Account/Settings cleanup) in this
	// same tx.
	if err := applyTeardownDeletes(ctx, tx, teardownDeletion{
		userID:         userID,
		vehicleID:      vehicleID,
		teslaVehicleID: teslaVehicleID,
		vin:            vin,
		wasLast:        wasLast,
		tombstone:      tombstone,
	}); err != nil {
		return TeardownResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return TeardownResult{}, fmt.Errorf("store.RemoveVehicle(user=%s): commit: %w", userID, err)
	}

	// Audit line (P0 only: opaque cuids + counts; never VIN/tokens/PII).
	t.logger.Info("vehicle_deleted",
		slog.String("event", "vehicle_deleted"),
		slog.String("user_id", userID),
		slog.String("vehicle_id", vehicleID),
		slog.Int("drive_count", driveCount),
		slog.Bool("was_last_vehicle", wasLast),
		slog.Bool("tombstoned", tombstone),
	)

	return TeardownResult{
		Removed:            true,
		WasLastVehicle:     wasLast,
		TeslaTokensCleared: wasLast,
		DriveCount:         driveCount,
		Tombstoned:         tombstone,
	}, nil
}

// teardownDeletion carries the parameters of the destructive teardown phase.
type teardownDeletion struct {
	userID         string
	vehicleID      string
	teslaVehicleID string
	vin            string
	wasLast        bool
	tombstone      bool
}

// applyTeardownDeletes runs the destructive tail of RemoveVehicle inside the
// open transaction: (4a) delete the Go-owned ride-request rows (P1 GPS +
// passenger PII, no FK cascade reaches them), (4b) delete the vehicle
// (owner-scoped; cascades + fires the NOTIFY), (4c) write the removed-vehicle
// tombstone (MYR-261) so a later re-link sync cannot resurrect the still-Tesla-
// owned VIN, and (5) on the owner's last vehicle clear the Tesla tokens + reset
// the link/pairing flags.
func applyTeardownDeletes(ctx context.Context, tx pgx.Tx, d teardownDeletion) error {
	if _, err := tx.Exec(ctx, queryTeardownDeleteRideRequests, d.vehicleID); err != nil {
		return fmt.Errorf("store.RemoveVehicle(user=%s): delete ride requests: %w", d.userID, err)
	}
	if _, err := tx.Exec(ctx, queryTeardownDeleteVehicle, d.vehicleID, d.userID); err != nil {
		return fmt.Errorf("store.RemoveVehicle(user=%s): delete vehicle: %w", d.userID, err)
	}
	if d.tombstone {
		if err := insertRemovedVehicleTombstone(ctx, tx, d.userID, d.teslaVehicleID, d.vin); err != nil {
			return err
		}
	}
	if d.wasLast {
		if err := clearLastVehicleState(ctx, tx, d.userID); err != nil {
			return err
		}
	}
	return nil
}

// clearLastVehicleState deletes the owner's Tesla Account tokens and resets the
// Settings link/pairing flags — the last-vehicle-only tail of a teardown. It
// removes OUR access; it does NOT revoke the grant at Tesla (data-lifecycle
// §1.2). Runs inside the teardown transaction.
func clearLastVehicleState(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx, queryTeardownDeleteAccount, userID); err != nil {
		return fmt.Errorf("store.RemoveVehicle(user=%s): delete account: %w", userID, err)
	}
	if _, err := tx.Exec(ctx, queryTeardownResetSettings, newProvisionID(), userID); err != nil {
		return fmt.Errorf("store.RemoveVehicle(user=%s): reset settings: %w", userID, err)
	}
	return nil
}

// readVehicleTeslaIdentity reads the removed vehicle's Tesla vehicle id and VIN
// within the teardown transaction (the row is already FOR UPDATE-locked). Both
// Prisma columns are nullable, so a car with no linked Tesla vehicle id returns
// an empty teslaVehicleID and the caller skips the tombstone.
func readVehicleTeslaIdentity(ctx context.Context, tx pgx.Tx, userID, vehicleID string) (teslaVehicleID, vin string, err error) {
	var tvid, v *string
	if err := tx.QueryRow(ctx, queryTeardownVehicleIdentity, vehicleID).Scan(&tvid, &v); err != nil {
		return "", "", fmt.Errorf("store.RemoveVehicle(user=%s): read vehicle identity: %w", userID, err)
	}
	if tvid != nil {
		teslaVehicleID = *tvid
	}
	if v != nil {
		vin = *v
	}
	return teslaVehicleID, vin, nil
}

// lockOwnerVehicleSet FOR UPDATE-locks and reads the owner's vehicle ids,
// returning the set size and whether targetID is among them. The row locks
// serialize concurrent same-owner teardowns so the last-vehicle test is
// isolated, not merely atomic.
func lockOwnerVehicleSet(ctx context.Context, tx pgx.Tx, userID, targetID string) (count int, exists bool, err error) {
	rows, err := tx.Query(ctx, queryTeardownLockOwnerVehicles, userID)
	if err != nil {
		return 0, false, fmt.Errorf("store.RemoveVehicle(user=%s): lock owner vehicles: %w", userID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, false, fmt.Errorf("store.RemoveVehicle(user=%s): scan vehicle id: %w", userID, err)
		}
		count++
		if id == targetID {
			exists = true
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("store.RemoveVehicle(user=%s): iterate vehicles: %w", userID, err)
	}
	return count, exists, nil
}

// insertAudit writes the user-initiated `vehicle_deleted` AuditLog row within
// the teardown transaction, reusing the same-package queryAuditInsert column
// list (single source of truth shared with AuditRepo — keeps CG-DL-8 column
// parity automatic). Metadata is P0-only per CG-DL-5.
func (t *OwnerTeardown) insertAudit(ctx context.Context, tx pgx.Tx, userID, vehicleID string, driveCount int, wasLast, tombstone bool) error {
	meta, err := json.Marshal(teardownAuditMetadata{DriveCount: driveCount, WasLastVehicle: wasLast, Tombstoned: tombstone})
	if err != nil {
		return fmt.Errorf("store.RemoveVehicle(user=%s): marshal audit metadata: %w", userID, err)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, queryAuditInsert,
		newProvisionID(),                  // id (cuid)
		userID,                            // userId (owner of the affected data)
		now,                               // timestamp
		string(AuditActionVehicleDeleted), // action
		auditTargetTypeVehicle,            // targetType
		vehicleID,                         // targetId
		auditInitiatorUser,                // initiator
		meta,                              // metadata (P0 counts only)
		now,                               // createdAt
	); err != nil {
		return fmt.Errorf("store.RemoveVehicle(user=%s): insert audit: %w", userID, err)
	}
	return nil
}
