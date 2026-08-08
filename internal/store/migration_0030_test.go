package store_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Migration 0030 (MYR-447) adds one partial index over go_ride_requests so the
// nightly retention sweep stays an index probe as terminal rides accumulate.
// These tests pin both directions and, crucially, the index PREDICATE: an index
// whose predicate no longer matches the claim queries' conjuncts is silently
// unusable, and the only symptom in production is a sequential scan per batch
// per pass until the batch exceeds its timeout and the sweep stops draining —
// log-only, while the table it was meant to bound keeps growing.
const rideRetentionIndex = "idx_go_ride_requests_retention"

// TestMigration0030_UpAddsTheRetentionIndex verifies the up-migration lands the
// index with the exact partial predicate the claim queries carry.
func TestMigration0030_UpAddsTheRetentionIndex(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	def := indexDef(t, rideRetentionIndex)
	if def == "" {
		t.Fatalf("%s missing after migrate up", rideRetentionIndex)
	}
	for _, want := range []string{
		"updated_at", // the indexed column, serving both the range and the ORDER BY
		"WHERE",      // it must stay PARTIAL, not cover every ride ever booked
		"completed",  // partial: the three terminal statuses, spelled out
		"declined",
		"cancelled",
	} {
		if !strings.Contains(def, want) {
			t.Errorf("%s definition %q is missing %q", rideRetentionIndex, def, want)
		}
	}

	// The predicate must NOT admit an open status. This is the assertion that
	// would catch someone "simplifying" the predicate to NOT IN (...) or
	// widening it while chasing a plan — either of which would put live rides in
	// an index whose only consumer is a DELETE.
	for _, forbidden := range []string{"requested", "accepted", "enroute", "arrived"} {
		if strings.Contains(def, forbidden) {
			t.Errorf("%s indexes the OPEN status %q: %s", rideRetentionIndex, forbidden, def)
		}
	}
}

// TestMigration0030_ServesTheRetentionClaim proves the index is actually USABLE
// by the sweep's claim, not merely present. A predicate Postgres cannot prove
// implied is an index that never gets chosen, and nothing else in the suite
// would notice.
func TestMigration0030_ServesTheRetentionClaim(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	ctx := context.Background()
	if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests`); err != nil {
		t.Fatalf("clean go_ride_requests: %v", err)
	}
	// Enough rows that a seq scan is not trivially cheaper, and ANALYZE so the
	// planner is choosing on statistics rather than on defaults.
	for i := range 500 {
		seedRide(t, fmt.Sprintf("plan-ride-%03d", i), "user-plan", "veh-plan", "completed",
			store.RideRetentionWindow+time.Duration(i)*time.Minute, false)
	}
	if _, err := testPool.Exec(ctx, `ANALYZE go_ride_requests`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// FORMAT JSON, so the whole plan tree arrives as ONE row. Plain EXPLAIN
	// returns one row per plan line and QueryRow would silently read only the
	// topmost node — which is never the index scan.
	var plan string
	err := testPool.QueryRow(ctx, `
		EXPLAIN (FORMAT JSON)
		SELECT id, vehicle_id, owner_id, updated_at
		FROM go_ride_requests
		WHERE updated_at < NOW() - make_interval(days => `+strconv.Itoa(store.RideRetentionDays)+`)
		  AND status IN ('completed', 'declined', 'cancelled')
		ORDER BY updated_at ASC
		LIMIT 100`).Scan(&plan)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !strings.Contains(plan, rideRetentionIndex) {
		t.Errorf("the retention claim does not use %s; plan was:\n%s", rideRetentionIndex, plan)
	}
}

// TestMigration0030_DownDropsTheIndexOnly exercises the rollback and proves it
// is SURGICAL: only the new index goes, and the guards earlier migrations
// installed on the same table survive.
func TestMigration0030_DownDropsTheIndexOnly(t *testing.T) {
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
	if err := m.Migrate(29); err != nil {
		t.Fatalf("migrate down to 29: %v", err)
	}
	// Restore the schema no matter how the assertions below go, so whatever runs
	// next still sees a head database.
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	if def := indexDef(t, rideRetentionIndex); def != "" {
		t.Errorf("%s still present after migrate down: %q", rideRetentionIndex, def)
	}
	// Surgical: the ride-table guards and indexes from earlier migrations must
	// survive — 0030 is additive and its rollback must take nothing else.
	for _, survivor := range []string{
		"uq_go_ride_requests_active_instant_rider",   // MYR-230, migration 0004
		"uq_go_ride_requests_active_instant_vehicle", // MYR-266, migration 0013
		"idx_go_ride_requests_reservation_due",       // MYR-179, migration 0016
		"idx_go_ride_requests_active_poll",           // MYR-394, migration 0028
	} {
		if def := indexDef(t, survivor); def == "" {
			t.Errorf("%s dropped by the 0030 rollback — the down must be surgical", survivor)
		}
	}
}
