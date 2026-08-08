package store_test

import (
	"context"
	"testing"
	"time"
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

// TestMigration0032_ExistingRowsAreStamped pins the backfill decision: rows
// that predate the column are stamped at migration time, NOT left NULL.
//
// The migration argues NULL would assert "never modified" about rows that may
// well have been patched before the column existed — a claim the data cannot
// support. The default makes the value a truthful LOWER BOUND instead. This
// test is what stops a later edit from "cleaning up" the default and
// reintroducing the ambiguity.
func TestMigration0032_ExistingRowsAreStamped(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanVehicleShares(t)

	before := time.Now().UTC().Add(-time.Minute)
	if err := insertShare(t, "share-0032", "veh-0032", "owner-0032", "code-0032",
		"pending", time.Now().UTC().Add(24*time.Hour), nil); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	var updatedAt time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT updated_at FROM go_vehicle_shares WHERE id = $1`, "share-0032",
	).Scan(&updatedAt); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	if updatedAt.Before(before) {
		t.Errorf("updated_at = %s, want a stamp at or after %s — the DEFAULT now() is missing",
			updatedAt, before)
	}
}
