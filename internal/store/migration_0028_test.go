package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Migration 0028 (MYR-394) adds one partial index over go_ride_requests so the
// active-ride poll-target read stays an index probe as the ride table grows
// without bound.
//
// The PREDICATE is what these tests really pin. An index whose predicate no
// longer matches the query's conjuncts is silently unusable, and the failure is
// invisible until it is expensive: the reconcile seq-scans and sorts the whole
// ride table on every replica every five minutes, and when the pass eventually
// exceeds ReconcileTimeout it returns an error and — by design — changes
// nothing. So the safety net that reaps leaked pollers and re-adopts live rides
// after a restart just quietly stops working, log-only, while the Fleet API
// budget keeps being spent on rides that ended.
const activePollIndex = "idx_go_ride_requests_active_poll"

// TestMigration0028_UpAddsTheActivePollIndex verifies the up-migration lands
// the index with the column and partial predicate the query depends on.
func TestMigration0028_UpAddsTheActivePollIndex(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	def := indexDef(t, activePollIndex)
	if def == "" {
		t.Fatalf("%s missing after migrate up", activePollIndex)
	}
	for _, want := range []string{
		// The key column: serves BOTH the six-hour age bound (as a range) and
		// the ORDER BY ... DESC (as a backwards ordered scan).
		"updated_at",
		// It must stay PARTIAL rather than covering every ride ever taken —
		// that is what makes the indexed set self-draining, tracking concurrent
		// live rides instead of lifetime ride volume.
		"WHERE",
		"status",
	} {
		if !strings.Contains(def, want) {
			t.Errorf("%s definition %q is missing %q", activePollIndex, def, want)
		}
	}
	// Terminal rows must NOT be in the index; if they were, it would grow
	// forever and the whole point would be lost.
	for _, unwanted := range []string{"completed", "cancelled", "declined", "requested"} {
		if strings.Contains(def, unwanted) {
			t.Errorf("%s definition %q includes terminal status %q; the index must be self-draining",
				activePollIndex, def, unwanted)
		}
	}
}

// TestMigration0028_DownDropsTheActivePollIndex — the down migration is purely
// additive in reverse: it must remove the index and nothing else. Correctness
// lives in the predicate, so the query still returns the same rows without it.
func TestMigration0028_DownDropsTheActivePollIndex(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	if indexDef(t, activePollIndex) == "" {
		t.Fatalf("%s missing before down", activePollIndex)
	}

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	// Version-targeted, not Steps(-1): a relative step would silently become a
	// test of whatever migration lands next.
	//
	// The target is 26 rather than 27 because 0027 does not exist on this
	// branch — it is added by the unmerged PR #360 (MYR-398), which claimed the
	// number concurrently. golang-migrate tolerates the gap. Once #360 lands
	// and this branch rebases, rolling to 26 will also undo 0027; the
	// assertions below stay correct either way, and RunMigrations restores head
	// in the cleanup.
	if err := m.Migrate(26); err != nil {
		t.Fatalf("migrate down to 26: %v", err)
	}
	// Restore the schema no matter how the assertions below go, so whatever
	// runs next still sees a head database.
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	if def := indexDef(t, activePollIndex); def != "" {
		t.Errorf("%s still present after migrate down: %q", activePollIndex, def)
	}
	// Surgical: the other ride-table indexes must survive.
	for _, survivor := range []string{
		"uq_go_ride_requests_active_instant_vehicle", // MYR-266, migration 0013
		"idx_go_ride_requests_reservation_due",       // MYR-179, migration 0016
		"idx_go_ride_requests_vehicle_window",        // MYR-383, migration 0026
	} {
		if def := indexDef(t, survivor); def == "" {
			t.Errorf("%s dropped by the 0028 rollback — the down must be surgical", survivor)
		}
	}
}
