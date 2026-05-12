package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// auditSchemaSQL re-creates the AuditLog table, indexes, and append-only
// triggers installed by the Phase 1 Prisma migration:
//
//	../my-robo-taxi/prisma/migrations/20260508211924_auditlog_table_and_append_only_triggers/migration.sql
//
// This duplication is intentional. Reading the sibling repo's migration
// file at test time would couple this test to a sibling-repo path layout
// (CI may not even check that repo out). Instead, the canonical SQL is
// duplicated here verbatim. The cross-repo coupling note at the top of
// internal/store/audit_repo.go applies to this constant too: any change
// to the Prisma migration MUST be mirrored here in the same PR, and
// contract-guard CG-DL-8 enforces it on every PR. The exception text
// "AuditLog rows are append-only" is asserted on by the trigger tests
// below — keep the strings in sync.
const auditSchemaSQL = `
CREATE TABLE "AuditLog" (
    "id" TEXT NOT NULL,
    "userId" TEXT NOT NULL,
    "timestamp" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "action" TEXT NOT NULL,
    "targetType" TEXT NOT NULL,
    "targetId" TEXT NOT NULL,
    "initiator" TEXT NOT NULL,
    "metadata" JSONB NOT NULL DEFAULT '{}',
    "createdAt" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT "AuditLog_pkey" PRIMARY KEY ("id")
);

CREATE INDEX "AuditLog_userId_idx" ON "AuditLog"("userId");
CREATE INDEX "AuditLog_action_idx" ON "AuditLog"("action");
CREATE INDEX "AuditLog_timestamp_idx" ON "AuditLog"("timestamp");

CREATE OR REPLACE FUNCTION prevent_audit_log_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'AuditLog rows are append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER prevent_audit_log_update
    BEFORE UPDATE ON "AuditLog"
    FOR EACH ROW
    EXECUTE FUNCTION prevent_audit_log_mutation();

CREATE TRIGGER prevent_audit_log_delete
    BEFORE DELETE ON "AuditLog"
    FOR EACH ROW
    EXECUTE FUNCTION prevent_audit_log_mutation();
`

// auditSchemaOnce ensures the AuditLog DDL is applied exactly once per
// test process even if multiple AuditRepo tests run concurrently or
// before TestMain's createSchema is amended. The shared testPool from
// db_test.go is reused.
var auditSchemaOnce sync.Once

// ensureAuditSchema applies auditSchemaSQL to the shared testPool.
// Idempotent across repeated test invocations within one go test process
// because of the sync.Once guard.
func ensureAuditSchema(t *testing.T) {
	t.Helper()
	auditSchemaOnce.Do(func() {
		ctx := context.Background()
		if _, err := testPool.Exec(ctx, auditSchemaSQL); err != nil {
			t.Fatalf("apply AuditLog schema: %v", err)
		}
	})
}

// cleanAuditLog removes all rows from AuditLog using a TRUNCATE that
// disables the BEFORE DELETE trigger. Per the migration's operator
// notes, TRUNCATE bypasses the row-level trigger by design — exactly
// what we need for cross-test cleanup. NFR-3.29 forbids TRUNCATE in
// production at the policy level; the test-only use here is within
// the policy carve-out for ephemeral test databases.
func cleanAuditLog(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE "AuditLog"`); err != nil {
		t.Fatalf("truncate AuditLog: %v", err)
	}
}

// validEntry returns a fully-populated AuditEntry for use in happy-path
// tests. Sub-tests override the fields they want to vary.
func validEntry(id string) store.AuditEntry {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	return store.AuditEntry{
		ID:         id,
		UserID:     "user_001",
		Timestamp:  now,
		Action:     store.AuditActionDrivesPruned,
		TargetType: "drive",
		TargetID:   "veh_001",
		Initiator:  "system_pruner",
		Metadata:   json.RawMessage(`{"driveCount":7}`),
		CreatedAt:  now,
	}
}

func TestAuditRepo_InsertAuditLog(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available; skipping AuditLog integration test")
	}
	ensureAuditSchema(t)
	cleanAuditLog(t, testPool)

	repo := store.NewAuditRepo(testPool)
	ctx := context.Background()

	tests := []struct {
		name    string
		entry   store.AuditEntry
		wantErr bool
	}{
		{
			name:    "valid entry inserts successfully",
			entry:   validEntry("audit_001"),
			wantErr: false,
		},
		{
			name: "empty metadata is normalized to empty object",
			entry: func() store.AuditEntry {
				e := validEntry("audit_002")
				e.Metadata = nil
				return e
			}(),
			wantErr: false,
		},
		{
			name: "duplicate primary key fails",
			entry: func() store.AuditEntry {
				e := validEntry("audit_001") // same ID as first test
				e.TargetID = "veh_002"
				return e
			}(),
			wantErr: true,
		},
		{
			name: "mask_applied action is accepted",
			entry: func() store.AuditEntry {
				e := validEntry("audit_003")
				e.Action = store.AuditActionMaskApplied
				e.TargetType = "ws_broadcast"
				e.Initiator = "system_auth"
				return e
			}(),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := repo.InsertAuditLog(ctx, tt.entry)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify the row is queryable and persisted as written.
			var (
				gotAction     string
				gotTargetType string
				gotMetadata   json.RawMessage
			)
			row := testPool.QueryRow(ctx,
				`SELECT "action", "targetType", "metadata"
				 FROM "AuditLog" WHERE "id" = $1`,
				tt.entry.ID)
			if err := row.Scan(&gotAction, &gotTargetType, &gotMetadata); err != nil {
				t.Fatalf("readback failed: %v", err)
			}
			if gotAction != string(tt.entry.Action) {
				t.Errorf("action: got %q, want %q", gotAction, tt.entry.Action)
			}
			if gotTargetType != tt.entry.TargetType {
				t.Errorf("targetType: got %q, want %q", gotTargetType, tt.entry.TargetType)
			}
			if len(gotMetadata) == 0 {
				t.Errorf("metadata: got empty, want non-empty (default '{}')")
			}
		})
	}
}

func TestAuditRepo_AppendOnlyTriggers(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available; skipping AuditLog integration test")
	}
	ensureAuditSchema(t)
	cleanAuditLog(t, testPool)

	repo := store.NewAuditRepo(testPool)
	ctx := context.Background()

	// Seed one row that the trigger tests will try to mutate.
	if err := repo.InsertAuditLog(ctx, validEntry("audit_trigger_001")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(ctx context.Context) error
		wantSub string
	}{
		{
			name: "raw UPDATE is blocked by trigger",
			mutate: func(ctx context.Context) error {
				_, err := testPool.Exec(ctx,
					`UPDATE "AuditLog" SET "action" = 'tampered'
					 WHERE "id" = 'audit_trigger_001'`)
				return err
			},
			wantSub: "append-only",
		},
		{
			name: "raw DELETE is blocked by trigger",
			mutate: func(ctx context.Context) error {
				_, err := testPool.Exec(ctx,
					`DELETE FROM "AuditLog" WHERE "id" = 'audit_trigger_001'`)
				return err
			},
			wantSub: "append-only",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.mutate(ctx)
			if err == nil {
				t.Fatal("expected trigger exception, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantSub)
			}
		})
	}

	// Sanity: the seeded row is still present after both rejected
	// mutations. Append-only means UPDATE / DELETE are not just refused
	// loudly but also leave the row unchanged.
	var stillThere int
	row := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM "AuditLog" WHERE "id" = 'audit_trigger_001'`)
	if err := row.Scan(&stillThere); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if stillThere != 1 {
		t.Fatalf("expected seeded row to survive failed mutations, got count=%d", stillThere)
	}
}

func TestAuditRepo_NotNullColumnsRejectMissingValues(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available; skipping AuditLog integration test")
	}
	ensureAuditSchema(t)
	cleanAuditLog(t, testPool)

	ctx := context.Background()

	// Drive the NOT NULL constraints directly with raw SQL so the test is
	// independent of any normalization the Go layer might do. Each
	// statement omits or NULLs one required column. Postgres returns a
	// "null value in column \"X\"" error for NOT NULL violations.
	tests := []struct {
		name    string
		sql     string
		args    []any
		wantSub string
	}{
		{
			name: "NULL userId is rejected",
			sql: `INSERT INTO "AuditLog" ("id","userId","action","targetType","targetId","initiator")
				  VALUES ($1, NULL, 'drives_pruned', 'drive', 'veh_001', 'system_pruner')`,
			args:    []any{"audit_null_user"},
			wantSub: "userId",
		},
		{
			name: "NULL action is rejected",
			sql: `INSERT INTO "AuditLog" ("id","userId","action","targetType","targetId","initiator")
				  VALUES ($1, 'user_001', NULL, 'drive', 'veh_001', 'system_pruner')`,
			args:    []any{"audit_null_action"},
			wantSub: "action",
		},
		{
			name: "NULL targetType is rejected",
			sql: `INSERT INTO "AuditLog" ("id","userId","action","targetType","targetId","initiator")
				  VALUES ($1, 'user_001', 'drives_pruned', NULL, 'veh_001', 'system_pruner')`,
			args:    []any{"audit_null_target_type"},
			wantSub: "targetType",
		},
		{
			name: "NULL targetId is rejected",
			sql: `INSERT INTO "AuditLog" ("id","userId","action","targetType","targetId","initiator")
				  VALUES ($1, 'user_001', 'drives_pruned', 'drive', NULL, 'system_pruner')`,
			args:    []any{"audit_null_target_id"},
			wantSub: "targetId",
		},
		{
			name: "NULL initiator is rejected",
			sql: `INSERT INTO "AuditLog" ("id","userId","action","targetType","targetId","initiator")
				  VALUES ($1, 'user_001', 'drives_pruned', 'drive', 'veh_001', NULL)`,
			args:    []any{"audit_null_initiator"},
			wantSub: "initiator",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := testPool.Exec(ctx, tt.sql, tt.args...)
			if err == nil {
				t.Fatal("expected NOT NULL violation, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("error %q does not mention column %q", err.Error(), tt.wantSub)
			}
		})
	}
}
