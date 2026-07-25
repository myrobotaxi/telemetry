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

// go_removed_vehicles (migration 0006, MYR-261) is the per-owner removed-VIN
// tombstone that stops a passive Tesla re-link from resurrecting a car the
// owner deliberately removed. The MYR-258 teardown deletes the local Vehicle
// row but does NOT revoke access at Tesla, so the next link's best-effort sync
// (ownerStreamHook.AfterLink -> UpsertOwnedVehicle) used to re-INSERT the
// still-owned VIN. Now the teardown writes a tombstone row in the SAME
// transaction as the delete, UpsertOwnedVehicle SKIPS any tombstoned id, and a
// deliberate re-add clears the tombstone via ClearTombstone.
//
// The table is keyed by the natural composite (user_id, tesla_vehicle_id) and
// carries NO Prisma FK (CG-DL-9): the ids are stored as plain columns.

// AuditActionVehicleReaddAllowed records a deliberate owner re-add of a
// previously removed car: the tombstone for (userId, teslaVehicleId) was
// cleared so the next Tesla sync may provision the VIN again. It is the exact
// inverse of the tombstone written by the vehicle_deleted teardown (MYR-261,
// data-lifecycle.md §4.2). Emitted by RemovedVehicleRegistry.ClearTombstone
// inside the same transaction as the tombstone DELETE (CG-DL-3 audit-with-
// mutation pattern). targetType='vehicle', targetId=teslaVehicleId,
// initiator='user'.
const AuditActionVehicleReaddAllowed AuditAction = "vehicle_readd_allowed"

// queryInsertRemovedVehicle writes (or refreshes) a tombstone. Idempotent on
// the composite primary key: re-tombstoning the same car just bumps removed_at.
// Called with the teardown's tx so the tombstone is atomic with the delete.
const queryInsertRemovedVehicle = `
INSERT INTO go_removed_vehicles (user_id, tesla_vehicle_id, vin, removed_at)
VALUES ($1, $2, NULLIF($3, ''), NOW())
ON CONFLICT (user_id, tesla_vehicle_id) DO UPDATE
SET removed_at = NOW(),
    vin        = COALESCE(EXCLUDED.vin, go_removed_vehicles.vin)`

// queryIsVehicleRemoved is the sync-path existence check: a single indexed
// lookup on the composite primary key.
const queryIsVehicleRemoved = `
SELECT EXISTS (
    SELECT 1 FROM go_removed_vehicles
    WHERE user_id = $1 AND tesla_vehicle_id = $2
)`

// queryDeleteRemovedVehicle clears a tombstone on a deliberate re-add. The
// RowsAffected() count reports whether a tombstone actually existed.
const queryDeleteRemovedVehicle = `
DELETE FROM go_removed_vehicles
WHERE user_id = $1 AND tesla_vehicle_id = $2`

// pgxQuerier is the read surface shared by *pgxpool.Pool and pgx.Tx, so the
// tombstone existence check can run either standalone (sync path, on the pool)
// or inside an open transaction.
type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// insertRemovedVehicleTombstone writes the removed-VIN tombstone within an
// already-open transaction (the teardown's). teslaVehicleID MUST be non-empty;
// the caller skips the write for a car with no Tesla vehicle id (there is
// nothing a Fleet-API sync could resurrect). vin is best-effort (stored for
// operator correlation; NULLIF collapses an empty string to NULL).
func insertRemovedVehicleTombstone(ctx context.Context, tx pgx.Tx, userID, teslaVehicleID, vin string) error {
	if _, err := tx.Exec(ctx, queryInsertRemovedVehicle, userID, teslaVehicleID, vin); err != nil {
		return fmt.Errorf("store: write removed-vehicle tombstone (user=%s): %w", userID, err)
	}
	return nil
}

// isVehicleTombstoned reports whether (userID, teslaVehicleID) currently has a
// removed-vehicle tombstone. Used by the provisioning sync path to skip a
// deliberately-removed car. A missing tesla vehicle id is never tombstoned.
func isVehicleTombstoned(ctx context.Context, q pgxQuerier, userID, teslaVehicleID string) (bool, error) {
	if strings.TrimSpace(teslaVehicleID) == "" {
		return false, nil
	}
	var removed bool
	if err := q.QueryRow(ctx, queryIsVehicleRemoved, userID, teslaVehicleID).Scan(&removed); err != nil {
		return false, fmt.Errorf("store: check removed-vehicle tombstone (user=%s): %w", userID, err)
	}
	return removed, nil
}

// RemovedVehicleRegistry is the deliberate-re-add entry point for the
// removed-VIN tombstones (MYR-261). It is deliberately separated from the
// passive AfterLink sync: the sync path can NEVER clear a tombstone (that would
// let a re-link resurrect a removed car), so un-tombstoning is an explicit,
// owner-initiated action routed here. Clearing a tombstone lets the NEXT Tesla
// sync provision the VIN again.
type RemovedVehicleRegistry struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewRemovedVehicleRegistry builds the registry over the given pool.
func NewRemovedVehicleRegistry(pool *pgxpool.Pool, logger *slog.Logger) *RemovedVehicleRegistry {
	if logger == nil {
		logger = slog.Default()
	}
	return &RemovedVehicleRegistry{pool: pool, logger: logger}
}

// readdAuditMetadata is the P0-only audit metadata for a re-add allow: an
// opaque redacted VIN tail is NOT recorded here (VIN is P1); only the
// boolean existed flag is kept.
type readdAuditMetadata struct {
	// Existed is true when a tombstone was actually present and cleared.
	Existed bool `json:"existed"`
}

// ClearTombstone removes the removed-vehicle tombstone for (userID,
// teslaVehicleID) — the deliberate "add this Tesla back" signal — so the next
// Tesla sync may provision the car again. Transactional and audited: on an
// actual clear it writes a vehicle_readd_allowed AuditLog row in the same
// transaction as the DELETE (CG-DL-3). Idempotent: clearing an absent
// tombstone is a clean no-op success with no audit row (mirrors the teardown's
// already-gone semantics). Returns whether a tombstone was cleared.
func (r *RemovedVehicleRegistry) ClearTombstone(ctx context.Context, userID, teslaVehicleID string) (bool, error) {
	if strings.TrimSpace(userID) == "" {
		return false, fmt.Errorf("store.ClearTombstone: empty user id")
	}
	if strings.TrimSpace(teslaVehicleID) == "" {
		return false, fmt.Errorf("store.ClearTombstone(user=%s): empty teslaVehicleId", userID)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store.ClearTombstone(user=%s): begin: %w", userID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	tag, err := tx.Exec(ctx, queryDeleteRemovedVehicle, userID, teslaVehicleID)
	if err != nil {
		return false, fmt.Errorf("store.ClearTombstone(user=%s): delete tombstone: %w", userID, err)
	}
	cleared := tag.RowsAffected() > 0
	if !cleared {
		// No tombstone existed — commit the empty tx and report a clean no-op
		// (no audit row, mirroring the teardown's already-gone path).
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("store.ClearTombstone(user=%s): commit no-op: %w", userID, err)
		}
		return false, nil
	}

	if err := r.insertReaddAudit(ctx, tx, userID, teslaVehicleID); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store.ClearTombstone(user=%s): commit: %w", userID, err)
	}

	r.logger.Info("vehicle_readd_allowed",
		slog.String("event", "vehicle_readd_allowed"),
		slog.String("user_id", userID),
		slog.String("tesla_vehicle_id", teslaVehicleID),
	)
	return true, nil
}

// insertReaddAudit writes the user-initiated vehicle_readd_allowed AuditLog row
// inside the clear transaction, reusing the same-package queryAuditInsert column
// list (single source of truth shared with AuditRepo). Metadata is P0-only
// (CG-DL-5). targetId is the Tesla vehicle id (the tombstone key); the local
// Vehicle row does not exist at re-add time.
func (r *RemovedVehicleRegistry) insertReaddAudit(ctx context.Context, tx pgx.Tx, userID, teslaVehicleID string) error {
	meta, err := json.Marshal(readdAuditMetadata{Existed: true})
	if err != nil {
		return fmt.Errorf("store.ClearTombstone(user=%s): marshal audit metadata: %w", userID, err)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, queryAuditInsert,
		newProvisionID(),                       // id (cuid)
		userID,                                 // userId (owner)
		now,                                    // timestamp
		string(AuditActionVehicleReaddAllowed), // action
		auditTargetTypeVehicle,                 // targetType
		teslaVehicleID,                         // targetId (Tesla vehicle id)
		auditInitiatorUser,                     // initiator
		meta,                                   // metadata (P0 only)
		now,                                    // createdAt
	); err != nil {
		return fmt.Errorf("store.ClearTombstone(user=%s): insert audit: %w", userID, err)
	}
	return nil
}
