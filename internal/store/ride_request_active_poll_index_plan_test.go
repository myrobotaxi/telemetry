// The PLAN test for the MYR-394 active-ride poll-target read.
//
// WHY A TEST ABOUT EXPLAIN OUTPUT EXISTS AT ALL. Every other test in
// ride_request_active_poll_test.go asks what the query RETURNS, and it returns
// exactly the same rows with or without migration 0028's index — just by
// reading the whole table instead of ~50 index entries. The difference is
// invisible to a correctness test and compounding in production: this read runs
// SYNCHRONOUSLY at startup, before the HTTP server accepts traffic, and then
// every ~5m on every replica, against a table that retains terminal rows
// forever.
//
// And it fails SILENTLY. When the pass eventually exceeds ReconcileTimeout the
// reconcile returns an error and — deliberately — changes nothing, so the
// safety net that reaps leaked pollers and re-adopts live rides after a restart
// just stops working, log-only, while the Fleet API budget keeps being spent on
// rides that ended hours ago.
//
// So the assertion is about the PLAN, and it has teeth in three places: the
// index must be used at all, the six-hour age bound must reach Index Cond
// rather than degrading to a heap Filter, and the scan must return a handful of
// rows rather than every ride seeded below.

package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// pollPlanFixtureRides is how many rides the fixture seeds per bucket. Large
// enough that a full scan cannot be mistaken for an index probe.
const pollPlanFixtureRides = 1200

// TestActiveRidePollQuery_RidesTheActivePollIndex is the SARGability tripwire.
func TestActiveRidePollQuery_RidesTheActivePollIndex(t *testing.T) {
	// Only for its side effects: migrations applied, ride tables emptied.
	setupRideRequestRepo(t)
	seedPollPlanRides(t, pollPlanFixtureRides)

	plan := explainJSON(t, store.QueryActiveRidePollTargetsForTest, 50)
	conds, rows := indexCondsFor(plan, activePollIndex)

	if len(conds) == 0 {
		t.Fatalf("no scan of %s in the plan — the query stopped being able to use it.\n%s",
			activePollIndex, plan)
	}
	// The load-bearing assertion: the age bound is an INDEX condition, not a
	// filter applied after the heap read. The bare-column spelling
	// (`updated_at > NOW() - INTERVAL '6 hours'`) is what earns this; the
	// algebraically identical `updated_at + INTERVAL '6 hours' > NOW()` makes
	// the indexed column an expression and drops the range into a Filter.
	for i, cond := range conds {
		if !strings.Contains(cond, "updated_at") {
			t.Errorf("Index Cond[%d] carries no updated_at bound — the age range degraded to a Filter.\ncond: %s\n%s",
				i, cond, plan)
		}
	}
	// And the LIMIT actually short-circuits: an ordered index scan stops after
	// the rows it needs instead of sorting every open ride.
	for i, got := range rows {
		if got > pollPlanFixtureRides/4 {
			t.Errorf("index scan[%d] returned %d rows of %d seeded — the scan is not being narrowed.\n%s",
				i, got, pollPlanFixtureRides, plan)
		}
	}
}

// pollPlanVehicles is how many cars the fixture spreads its rides across, so
// the join onto "Vehicle" has real selectivity and the planner has a genuine
// alternative (driving from "Vehicle" into idx_go_ride_requests_vehicle) to
// reject.
const pollPlanVehicles = 200

// seedPollPlanRides writes the fixture straight through SQL, bypassing the
// repository: the MYR-266 per-rider and MYR-383 per-vehicle guards refuse most
// of these combinations, and this fixture is about VOLUME, not about what
// Create permits. Every ride carries a scheduled_for so the partial UNIQUE
// guards from 0004 / 0013 — both partial on `scheduled_for IS NULL` — do not
// apply, and a status of arrived/enroute so the poll predicate matches
// regardless.
//
// Three buckets, each of which the index must handle differently:
//   - OPEN and recent: the rows the query should actually return.
//   - OPEN but STALE (well past the six-hour ceiling): in the partial index,
//     excluded by the age range. These are what prove the range is an Index
//     Cond — if it degraded to a Filter, the scan would read them all.
//   - TERMINAL: outside the partial index entirely. These are what prove the
//     index stays self-draining rather than growing with lifetime ride volume.
func seedPollPlanRides(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()

	if _, err := testPool.Exec(ctx,
		`INSERT INTO "Vehicle" ("id", "userId", "vin", "name", "status")
		 SELECT 'clplanveh' || lpad(g::text, 14, '0'), 'user_001',
		        '7SAYGDET7TA' || lpad(g::text, 6, '0'), 'Plan car', 'parked'
		 FROM generate_series(1, $1) g`, pollPlanVehicles,
	); err != nil {
		t.Fatalf("seed plan vehicles: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(ctx, `DELETE FROM "Vehicle"`) })

	const insert = `INSERT INTO go_ride_requests (
		id, rider_id, owner_id, vehicle_id,
		pickup_lat_enc, pickup_lng_enc, pickup_label,
		dropoff_lat_enc, dropoff_lng_enc, dropoff_label,
		status, scheduled_for, updated_at)
	SELECT $1 || g, 'clplanrider' || g, 'clowner1234567890abcdef',
		'clplanveh' || lpad((1 + (g % $2))::text, 14, '0'),
		'enc', 'enc', 'Home', 'enc', 'enc', 'Work',
		($3::text[])[1 + (g % cardinality($3::text[]))],
		NOW() + (g * interval '1 minute'),
		NOW() - $4::interval - (g * interval '1 second')
	FROM generate_series(1, $5) g`

	openStatuses := []string{"arrived", "enroute"}
	terminal := []string{"completed", "cancelled", "declined"}

	seeds := []struct {
		prefix   string
		statuses []string
		age      string
		count    int
	}{
		// Live rides, all inside the six-hour ceiling.
		{"live-", openStatuses, "0 seconds", n},
		// Open-status rides abandoned days ago: in the partial index, out of
		// the age range.
		{"stale-", openStatuses, "48 hours", 3 * n},
		// Terminal debris: out of the partial index altogether.
		{"done-", terminal, "0 seconds", 4 * n},
	}

	for _, s := range seeds {
		if _, err := testPool.Exec(ctx, insert,
			s.prefix, pollPlanVehicles, s.statuses, s.age, s.count,
		); err != nil {
			t.Fatalf("seed %s rides: %v", s.prefix, err)
		}
	}

	// The planner will not choose an index it has no statistics for.
	if _, err := testPool.Exec(ctx, `ANALYZE go_ride_requests, "Vehicle"`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
}
