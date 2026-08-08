package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-451 migration 0032: `updated_at` on go_vehicle_shares — the mutation
// timestamp whose absence made a live permissions incident untriageable.

// migration0032Columns is the shape 0032 ADDS, with its information_schema
// data_type. It joins the union counted by the undocumented-column guard in
// migration_0020_test.go.
//
// TIMESTAMPTZ, NOT NULL. The type matters for the same reason `suspended_at`'s
// does one migration over: this column exists to answer WHEN, and the whole
// point of adding it was that a boolean-only record of a capability change
// destroys the history the incident needed.
var migration0032Columns = map[string]string{
	"updated_at": "timestamp with time zone",
}

func TestMigration0032_AddsGrantUpdatedAt(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)

	got := vehicleShareColumnTypes(t)
	for col, wantType := range migration0032Columns {
		actual, ok := got[col]
		if !ok {
			t.Fatalf("column %s missing after migrate up", col)
		}
		if actual != wantType {
			t.Errorf("column %s: data_type = %q, want %q", col, actual, wantType)
		}
	}

	// NOT NULL is asserted because every reader is written without a nil
	// branch. A nullable column would compile identically and fail only where
	// it is read.
	nullability := shareColumnNullability(t)
	if nullability["updated_at"] != "NO" {
		t.Errorf("updated_at is_nullable = %q, want \"NO\"", nullability["updated_at"])
	}
}

// TestMigration0032_ExistingRowsAreStamped pins the BACKFILL decision: a row
// that genuinely predates the column comes out of the migration stamped, NOT
// NULL.
//
// The migration argues NULL would assert "never modified" about rows that may
// well have been patched before the column existed — a claim the data cannot
// support — while the migration instant is honest as a lower bound. This test
// is what stops a later edit from "cleaning up" the default and reintroducing
// the ambiguity.
//
// It rolls the schema back to 31 and seeds THERE, rather than inserting a fresh
// row at head. Seeding at head would only ever exercise `DEFAULT now()` on a
// new INSERT and would pass even if the ALTER left every pre-existing row NULL
// — which is the entire failure this test is named for.
func TestMigration0032_ExistingRowsAreStamped(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	if err := m.Migrate(31); err != nil {
		t.Fatalf("migrate down to 31: %v", err)
	}
	t.Cleanup(func() {
		if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	const legacyShare = "clmigration0032legacyrow"
	before := time.Now().UTC().Add(-time.Minute)
	if err := insertShare(t, legacyShare, "veh-0032", "owner-0032", "code-0032",
		"pending", time.Now().UTC().Add(24*time.Hour), nil); err != nil {
		t.Fatalf("seed pre-0032 share row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM go_vehicle_shares WHERE id = $1`, legacyShare)
	})

	if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
		t.Fatalf("migrate back up: %v", err)
	}

	var updatedAt time.Time
	if err := testPool.QueryRow(ctx,
		`SELECT updated_at FROM go_vehicle_shares WHERE id = $1`, legacyShare,
	).Scan(&updatedAt); err != nil {
		t.Fatalf("read back the backfilled row: %v", err)
	}
	if updatedAt.Before(before) {
		t.Errorf("updated_at = %s, want a stamp at or after %s — a row that predates the column came out unstamped",
			updatedAt, before)
	}
}

// TestMigration0032_DownDropsOnlyTheStamp exercises the rollback and proves it
// is SURGICAL — it takes the MYR-451 column and nothing else, in particular not
// the 0024 capability flags sitting beside it, whose loss would silently
// restore every withdrawn ride capability.
func TestMigration0032_DownDropsOnlyTheStamp(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	if err := m.Migrate(31); err != nil {
		t.Fatalf("migrate down to 31: %v", err)
	}
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	got := vehicleShareColumnTypes(t)
	for col := range migration0032Columns {
		if _, present := got[col]; present {
			t.Errorf("column %s survived the down-migration", col)
		}
	}
	for _, survivor := range []string{
		"id", "vehicle_id", "owner_user_id", "label", "permission", "code",
		"status", "accepted_by_user_id", "revoked_at", "allow_rides", "suspended_at",
	} {
		if _, present := got[survivor]; !present {
			t.Errorf("column %s was dropped by the 0032 down-migration — it must be surgical", survivor)
		}
	}
}
