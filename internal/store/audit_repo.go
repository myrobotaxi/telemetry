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
// rule CG-DL-8 enforces this on every PR (see .github/workflows/
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
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store/auditsidecar"
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
//
// Sidecar (MYR-77): after every successful DB INSERT, the repo calls
// sidecar.Emit to enqueue the entry for async S3 upload. Sidecar failures
// are logged and metered but NEVER propagate back to the caller — the DB
// is canonical and the sidecar is best-effort, at-most-once.
type AuditRepo struct {
	pool    *pgxpool.Pool
	sidecar auditsidecar.Sidecar
	logger  *slog.Logger
}

// NewAuditRepo creates an AuditRepo backed by the given connection pool
// and the provided sidecar. Pass auditsidecar.NoopSidecar{} (or use
// NewAuditRepoWithSidecar) when sidecar mirroring is not required.
func NewAuditRepo(pool *pgxpool.Pool) *AuditRepo {
	return &AuditRepo{pool: pool, sidecar: auditsidecar.NoopSidecar{}}
}

// NewAuditRepoWithSidecar creates an AuditRepo wired with a live sidecar.
// Used by the composition root when AUDIT_SIDECAR_BUCKET is set.
func NewAuditRepoWithSidecar(pool *pgxpool.Pool, sc auditsidecar.Sidecar, logger *slog.Logger) *AuditRepo {
	return &AuditRepo{pool: pool, sidecar: sc, logger: logger}
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
// Zero-time fallback: if Timestamp or CreatedAt is the zero value
// (time.Time{}), it is replaced with time.Now().UTC() at insert time.
// This guards against silent data-quality bugs where a caller forgets
// to set the timestamp and Postgres would otherwise write
// 0001-01-01T00:00:00Z. The Prisma model declares @default(now()) on
// these columns, but because this writer always passes them as bind
// parameters that DB-side default never fires — the Go-side fallback
// is what enforces the non-zero invariant.
//
// This is the only mutation method on AuditRepo by design — see the
// append-only invariant note at the top of this file.
func (r *AuditRepo) InsertAuditLog(ctx context.Context, entry AuditEntry) error {
	metadata := entry.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}

	now := time.Now().UTC()
	timestamp := entry.Timestamp
	if timestamp.IsZero() {
		timestamp = now
	}
	createdAt := entry.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}

	_, err := r.pool.Exec(ctx, queryAuditInsert,
		entry.ID,
		entry.UserID,
		timestamp,
		string(entry.Action),
		entry.TargetType,
		entry.TargetID,
		entry.Initiator,
		metadata,
		createdAt,
	)
	if err != nil {
		return fmt.Errorf("store.AuditRepo.InsertAuditLog: %w", err)
	}

	// Best-effort sidecar emit (MYR-77). The DB INSERT has already succeeded;
	// sidecar failure MUST NOT fail the audit row from the caller's perspective.
	// Emit is non-blocking (enqueues to a bounded channel). ErrQueueFull means
	// the channel was at capacity — the entry is dropped for the sidecar but the
	// DB row persists.
	sidecarEntry := auditsidecar.AuditEntry{
		ID:         entry.ID,
		UserID:     entry.UserID,
		Timestamp:  timestamp,
		Action:     string(entry.Action),
		TargetType: entry.TargetType,
		TargetID:   entry.TargetID,
		Initiator:  entry.Initiator,
		Metadata:   metadata,
		CreatedAt:  createdAt,
	}
	if emitErr := r.sidecar.Emit(sidecarEntry); emitErr != nil {
		if r.logger != nil {
			r.logger.Warn("audit sidecar emit failed — DB row persisted, sidecar entry dropped",
				slog.String("audit_log_id", entry.ID),
				slog.String("error", emitErr.Error()))
		}
	}

	return nil
}
