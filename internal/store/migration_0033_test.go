package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Migration 0033 (MYR-452) adds go_identity_convergence: the record of every
// re-point of an Apple binding from one user id to another.
//
// The table is load-bearing for a privacy guarantee, and a row in it is an
// unconditional grant of DELETE authority over another user id — the deletion
// scope runs every destructive step over the whole closure. So these tests pin
// the structural properties that keep that authority from being handed out by
// accident, and the one-off repair's guards.

// TestMigration0033_UpCreatesTheConvergenceTable pins the shape.
func TestMigration0033_UpCreatesTheConvergenceTable(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	ctx := context.Background()
	if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	// from_user_id is the PRIMARY KEY, not merely indexed: one abandoned id
	// may point at exactly one canonical id. Without the uniqueness, a second
	// convergence would fan the closure out rather than be refused.
	var pkCols int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage k ON k.constraint_name = tc.constraint_name
		WHERE tc.table_name = 'go_identity_convergence'
		  AND tc.constraint_type = 'PRIMARY KEY' AND k.column_name = 'from_user_id'`).Scan(&pkCols); err != nil {
		t.Fatalf("read primary key: %v", err)
	}
	if pkCols != 1 {
		t.Errorf("from_user_id is not the primary key (matched %d)", pkCols)
	}

	// The reverse-direction index the closure walk depends on. Queried
	// directly rather than through indexDef, which is pinned to
	// go_ride_requests.
	var def string
	if err := testPool.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE tablename = 'go_identity_convergence' AND indexname = $1`,
		"idx_go_identity_convergence_to").Scan(&def); err != nil {
		t.Fatalf("idx_go_identity_convergence_to missing after migrate up: %v", err)
	}
	if !strings.Contains(def, "to_user_id") {
		t.Errorf("reverse index does not cover to_user_id: %s", def)
	}
}

// A self-edge is always a bug — it makes the closure walk pointless — and the
// CHECK is what stops one being written rather than being reasoned about.
func TestMigration0033_RejectsASelfEdge(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	ctx := context.Background()
	if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	_, err := testPool.Exec(ctx,
		`INSERT INTO go_identity_convergence (from_user_id, to_user_id) VALUES ($1, $1)`,
		"cselfedge000000000000001")
	if err == nil {
		_, _ = testPool.Exec(ctx,
			`DELETE FROM go_identity_convergence WHERE from_user_id = 'cselfedge000000000000001'`)
		t.Fatal("a self-edge was accepted; the CHECK constraint is missing")
	}
	if !strings.Contains(err.Error(), "go_identity_convergence_no_self") {
		t.Errorf("rejected for the wrong reason: %v", err)
	}
}

// The one-off repair must be INERT wherever its subjects do not match. It names
// production ids directly (there is deliberately no email heuristic — see the
// migration comment), so the guards are the only thing standing between it and
// an environment it was never meant to touch.
func TestMigration0033_OneOffRepairIsInertWithoutItsSubjects(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	ctx := context.Background()
	if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	var n int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM go_identity_convergence
		WHERE from_user_id = 'cd38cb3eba91a3b9b69ff6eee5aedcc29'`).Scan(&n); err != nil {
		t.Fatalf("count repair row: %v", err)
	}
	if n != 0 {
		t.Errorf("the production one-off repair inserted %d rows into a test database; "+
			"its guards are not holding", n)
	}
}
