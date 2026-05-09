package store

// CROSS-REPO COUPLING — READ BEFORE EDITING
// =========================================
// The AuditLog table is owned by the Next.js app's Prisma schema at
//
//   ../my-robo-taxi/prisma/schema.prisma  (model AuditLog)
//
// and provisioned by migration
//
//   ../my-robo-taxi/prisma/migrations/20260508211924_auditlog_table_and_append_only_triggers/
//
// per docs/contracts/data-lifecycle.md §1.4 / §4 (FR-10.2, NFR-3.29). The
// telemetry server holds Insert-only access to this table for system-
// initiated rows: drives_pruned, mask_applied, tokens_refreshed.
//
// Authority: the Prisma model is the schema source of truth. The columns
// declared in AuditEntry below MUST mirror the Prisma model exactly. Any
// change to either side (column add/rename/retype, classification tier,
// nullability) requires updating BOTH files in the SAME PR. contract-guard
// rule CG-DL-7 enforces this on every PR (see .github/workflows/
// contract-guard.yml and .claude/agents/contract-guard.md). Drift is a
// contract violation that blocks merge.
//
// Append-only invariant (NFR-3.29 / CG-DL-2):
//   * The DB enforces this via the prevent_audit_log_mutation() function
//     and the prevent_audit_log_update / prevent_audit_log_delete triggers
//     installed by the Phase 1 migration. Any UPDATE / DELETE raises
//     "AuditLog rows are append-only".
//   * This Go type ALSO refuses to expose mutation methods: AuditRepo
//     intentionally has only InsertAuditLog. Do NOT add Update / Delete /
//     Get / List methods here. If a query path is ever needed, callers
//     should read AuditLog through Prisma (the owner) — not by extending
//     this repo.
//
// IDs:
//   * Callers MUST supply a cuid-format id on every entry. This matches
//     the convention used by DriveRepo.Create / VehicleRepo upsert paths
//     elsewhere in this package (Go-side id generation, never relying on
//     Prisma's @default(cuid()) DB-side fallback). Generating the id at
//     the call site means the audit row's id is known to the caller for
//     downstream correlation (e.g., logging the inserted audit id in the
//     same transaction that mutated the affected entity).
//
// Classification:
//   * All columns are P0 per data-lifecycle.md §4.4. The metadata JSONB
//     MUST contain only opaque IDs / counts / enum values — never P1
//     values like coordinates, addresses, names, tokens, or emails.
//     contract-guard rule CG-DL-5 enforces this on every PR.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditAction is the enum-like string written to the AuditLog.action column.
// The full enum is defined by docs/contracts/data-lifecycle.md §4.2; the
// constants below cover the actions emitted by the Go telemetry server.
// User-initiated actions (account_deleted, vehicle_deleted, drive_deleted,
// invite_revoked) are emitted by the Next.js app inside its Prisma
// $transaction and are intentionally NOT exposed here.
type AuditAction string

const (
	// AuditActionAccountDeleted records a user-initiated account deletion
	// per FR-10.1. Emitted by the Next.js app, NOT by the Go telemetry
	// server. Defined here for symmetry with the contract enum and so
	// downstream Go consumers (e.g., metric labels) can use the same
	// constant set.
	AuditActionAccountDeleted AuditAction = "account_deleted"

	// AuditActionMaskApplied records a 1%-sampled event in which a
	// role-based field mask removed at least one field from a REST
	// response or WebSocket broadcast. See rest-api.md §5.3.
	AuditActionMaskApplied AuditAction = "mask_applied"

	// AuditActionTokensRefreshed records a Tesla OAuth2 token rotation
	// initiated by the Go telemetry server's auto-refresh path.
	AuditActionTokensRefreshed AuditAction = "tokens_refreshed"

	// AuditActionDrivesPruned records a batch of drives deleted by the
	// NFR-3.27 retention pruning job.
	AuditActionDrivesPruned AuditAction = "drives_pruned"
)

// AuditEntry mirrors the AuditLog table one-to-one. Every field maps to a
// Prisma column of the same case-folded name. See the cross-repo coupling
// note at the top of this file before changing anything here.
//
// Field-by-field cross-walk to prisma/schema.prisma model AuditLog:
//
//	ID         -> id          @id @default(cuid())   -- caller-provided cuid
//	UserID     -> userId      String                 -- NOT a FK to User (§4.5)
//	Timestamp  -> timestamp   DateTime @default(now())
//	Action     -> action      String                 -- enum-like, see §4.2
//	TargetType -> targetType  String                 -- enum-like, see §4.2
//	TargetID   -> targetId    String
//	Initiator  -> initiator   String                 -- enum-like, see §4.2
//	Metadata   -> metadata    Json @default("{}")    -- P0 only (CG-DL-5)
//	CreatedAt  -> createdAt   DateTime @default(now())
type AuditEntry struct {
	ID         string
	UserID     string
	Timestamp  time.Time
	Action     AuditAction
	TargetType string
	TargetID   string
	Initiator  string
	Metadata   json.RawMessage // valid JSON; pass json.RawMessage("{}") when empty
	CreatedAt  time.Time
}

// AuditRepo provides Insert-only access to the Prisma-owned AuditLog
// table. The append-only invariant is enforced both at the database level
// (Phase 1 migration triggers) and at the API surface (this type exposes
// only InsertAuditLog). See the cross-repo coupling note at the top of
// this file.
type AuditRepo struct {
	pool *pgxpool.Pool
}

// NewAuditRepo creates an AuditRepo backed by the given connection pool.
func NewAuditRepo(pool *pgxpool.Pool) *AuditRepo {
	return &AuditRepo{pool: pool}
}

// queryAuditInsert names every column explicitly so column-order changes
// in the Prisma migration would break this query loudly rather than
// silently writing into the wrong column. The column list mirrors the
// Phase 1 CREATE TABLE statement.
const queryAuditInsert = `INSERT INTO "AuditLog" (
	"id", "userId", "timestamp", "action", "targetType",
	"targetId", "initiator", "metadata", "createdAt"
) VALUES (
	$1, $2, $3, $4, $5,
	$6, $7, $8, $9
)`

// InsertAuditLog appends a single audit entry. Callers MUST supply a
// caller-generated cuid id. The metadata field MUST be a valid JSON
// document containing only P0 values (see §4.4 / CG-DL-5); pass
// json.RawMessage("{}") when there is no metadata.
//
// This is the only mutation method on AuditRepo by design — see the
// append-only invariant note at the top of this file.
func (r *AuditRepo) InsertAuditLog(ctx context.Context, entry AuditEntry) error {
	metadata := entry.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}

	_, err := r.pool.Exec(ctx, queryAuditInsert,
		entry.ID,
		entry.UserID,
		entry.Timestamp,
		string(entry.Action),
		entry.TargetType,
		entry.TargetID,
		entry.Initiator,
		metadata,
		entry.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store.AuditRepo.InsertAuditLog: %w", err)
	}
	return nil
}
