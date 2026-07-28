package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Migration 0016 (MYR-179) adds one partial index over go_ride_requests so the
// 30s reservation sweep stays an index probe as the ride table grows without
// bound. These tests pin both directions and, crucially, the index PREDICATE:
// an index whose predicate no longer matches the sweep query's conjuncts is
// silently unusable, and the only symptom in production is a sequential scan
// per replica per tick until the pass exceeds its timeout and scheduled
// dispatch stops — log-only, while instant rides keep working.
const reservationDueIndex = "idx_go_ride_requests_reservation_due"

// indexDef returns pg_indexes.indexdef for one index on go_ride_requests, or
// "" when the index does not exist.
func indexDef(t *testing.T, name string) string {
	t.Helper()
	var def string
	err := testPool.QueryRow(context.Background(),
		`SELECT indexdef FROM pg_indexes WHERE tablename = 'go_ride_requests' AND indexname = $1`,
		name,
	).Scan(&def)
	if err != nil {
		return ""
	}
	return def
}

// TestMigration0016_UpAddsTheReservationDueIndex verifies the up-migration
// lands the index with the exact partial predicate the sweep query carries.
func TestMigration0016_UpAddsTheReservationDueIndex(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	def := indexDef(t, reservationDueIndex)
	if def == "" {
		t.Fatalf("%s missing after migrate up", reservationDueIndex)
	}
	for _, want := range []string{
		"scheduled_for", // the indexed column, serving both the range and the ORDER BY
		"WHERE",         // it must stay PARTIAL, not cover every ride ever booked
		"status",        // partial: accepted only
		"dispatched_at", // partial: latch unclaimed only
	} {
		if !strings.Contains(def, want) {
			t.Errorf("%s definition %q is missing %q", reservationDueIndex, def, want)
		}
	}
}

// TestMigration0016_DownDropsTheIndexOnly exercises the rollback and proves it
// is SURGICAL: only the new index goes, and the guards earlier migrations
// installed on the same table survive.
func TestMigration0016_DownDropsTheIndexOnly(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	// Version-targeted, not Steps(-1): a relative step would silently become a
	// test of whatever migration lands next.
	if err := m.Migrate(15); err != nil {
		t.Fatalf("migrate down to 15: %v", err)
	}
	// Restore the schema no matter how the assertions below go, so whatever
	// runs next still sees a head database.
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	if def := indexDef(t, reservationDueIndex); def != "" {
		t.Errorf("%s still present after migrate down: %q", reservationDueIndex, def)
	}
	// Surgical: the ride-table guards from 0004 / 0013 must survive.
	for _, survivor := range []string{
		"uq_go_ride_requests_active_instant_rider",   // MYR-230, migration 0004
		"uq_go_ride_requests_active_instant_vehicle", // MYR-266, migration 0013
	} {
		if def := indexDef(t, survivor); def == "" {
			t.Errorf("%s dropped by the 0016 rollback — the down must be surgical", survivor)
		}
	}
}
