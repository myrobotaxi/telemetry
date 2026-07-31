// The PLAN test for the MYR-383 window predicate (MYR-385).
//
// WHY A TEST ABOUT EXPLAIN OUTPUT EXISTS AT ALL. Every other test in this
// package asks what the two window statements RETURN, and both spellings of the
// predicate — the one with the half-width on the anchor and the one with it on
// the bounds — return identical rows. The difference is invisible to a
// correctness test and enormous in production: with the half-width on the
// anchor, `COALESCE(r.scheduled_for, NOW()) ± W` is an EXPRESSION, Postgres
// cannot match it to idx_go_ride_requests_vehicle_window's `scheduled_for`
// column, and the range drops out of Index Cond into a heap Filter. The conflict
// probe then reads every open reservation on the car — inside the per-vehicle
// advisory booking lock, on the create and accept paths a rider is waiting on —
// and the picker read scans the whole calendar to answer about two days of it.
//
// So the assertion here is about the PLAN, and it has teeth in two places: the
// `scheduled_for` bounds must appear in Index Cond, and the index scan must
// return a handful of rows rather than every reservation seeded below. A
// "simplification" back to the shared COALESCE expression fails both.
//
// See rideWindowOverlap in ride_request_conflict_queries.go for the algebra.

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// planFixtureReservations is how many OPEN reservations the hot vehicle carries.
// Chosen to match the volume the regression was measured at, and large enough
// that a full scan of the car's calendar cannot be mistaken for a range probe.
const planFixtureReservations = 1241

// planWindowIndex is the index whose Index Cond the range must reach.
const planWindowIndex = "idx_go_ride_requests_vehicle_window"

// planProbeInstant / planRange are the parameters the two statements are
// EXPLAINed at: one instant in the middle of the seeded calendar, and a
// two-day slice of it.
var (
	planBase         = time.Date(2029, 6, 12, 15, 0, 0, 0, time.UTC)
	planProbeInstant = planBase.Add(600 * time.Hour)
	planRangeFrom    = planBase.Add(600 * time.Hour)
	planRangeTo      = planRangeFrom.Add(48 * time.Hour)
)

// TestRideWindowQueries_RangeRidesTheVehicleWindowIndex is the SARGability
// tripwire for both landing sites of the shared predicate.
func TestRideWindowQueries_RangeRidesTheVehicleWindowIndex(t *testing.T) {
	// Only for its side effects: migrations applied, ride tables emptied.
	setupRideRequestRepo(t)
	seedOpenReservations(t, planFixtureReservations)

	secs := store.RideConflictWindow.Seconds()

	tests := []struct {
		name string
		sql  string
		args []any
	}{
		{
			// The gate probe. $1 vehicle, $2 proposed instant, $3 excluded ride,
			// $4 half-width seconds, $5 count-pending.
			name: "conflict probe (create + accept, inside the booking lock)",
			sql:  store.QueryRideWindowConflictForTest,
			args: []any{windowsVehicle, planProbeInstant, "", secs, true},
		},
		{
			// The picker read. $2/$3 are a RANGE here; $6 is the caller.
			name: "booked-windows read (the picker)",
			sql:  store.QueryVehicleBookedWindowsForTest,
			args: []any{windowsVehicle, planRangeFrom, planRangeTo, secs, true, windowsRider},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := explainJSON(t, tt.sql, tt.args...)
			conds, rows := indexCondsFor(plan, planWindowIndex)

			if len(conds) == 0 {
				t.Fatalf("no scan of %s in the plan — the predicate stopped being SARGable.\n%s",
					planWindowIndex, plan)
			}
			// The load-bearing assertion: the range is an INDEX condition, not a
			// filter applied after the heap read.
			for i, cond := range conds {
				if !strings.Contains(cond, "scheduled_for >") || !strings.Contains(cond, "scheduled_for <") {
					t.Errorf("Index Cond[%d] carries no scheduled_for range — the range degraded to a Filter.\ncond: %s\n%s",
						i, cond, plan)
				}
			}
			// And it is a RANGE probe, not a scan of the whole car's calendar
			// dressed as one. Without the bare-column spelling this reads every
			// one of the seeded reservations.
			for i, got := range rows {
				if got > planFixtureReservations/4 {
					t.Errorf("index scan[%d] returned %d rows of %d seeded — the range is not narrowing the scan.\n%s",
						i, got, planFixtureReservations, plan)
				}
			}
		})
	}
}

// seedOpenReservations writes n OPEN reservations for windowsVehicle straight
// through SQL, deliberately bypassing the repository: the MYR-383 gate refuses
// reservations closer together than RideConflictWindow, and this fixture is
// about VOLUME on one car, not about what Create permits. Terminal rows and
// other vehicles are seeded alongside so the partial index has something to
// exclude and the planner has a reason to prefer a scan if the predicate stops
// being SARGable.
func seedOpenReservations(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()

	const insert = `INSERT INTO go_ride_requests (
		id, rider_id, owner_id, vehicle_id,
		pickup_lat_enc, pickup_lng_enc, pickup_label,
		dropoff_lat_enc, dropoff_lng_enc, dropoff_label,
		status, scheduled_for)
	SELECT $1 || g, $2, 'clowner1234567890abcdef', $3,
		'enc', 'enc', 'Home', 'enc', 'enc', 'Work',
		($4::text[])[1 + (g % cardinality($4::text[]))],
		$5::timestamptz + (g * $6::interval)
	FROM generate_series(1, $7) g`

	openStatuses := []string{"requested", "accepted", "arrived", "enroute"}
	terminal := []string{"completed", "cancelled", "declined"}

	seeds := []struct {
		prefix   string
		rider    string
		vehicle  string
		statuses []string
		step     string
		count    int
	}{
		// The hot vehicle's live calendar.
		{"open-", windowsRider, windowsVehicle, openStatuses, "3 hours", n},
		// Terminal debris on the SAME car — the partial index excludes it, and
		// a sequential scan would not.
		{"done-", windowsRider, windowsVehicle, terminal, "37 minutes", 4 * n},
		// Other cars, so vehicle_id actually has selectivity to prove.
		{"other-", windowsOther, "clotherveh1234567890", openStatuses, "41 minutes", 4 * n},
	}
	for _, s := range seeds {
		if _, err := testPool.Exec(ctx, insert,
			s.prefix, s.rider, s.vehicle, s.statuses, planBase, s.step, s.count,
		); err != nil {
			t.Fatalf("seed %s rows: %v", s.prefix, err)
		}
	}
	if _, err := testPool.Exec(ctx, `ANALYZE go_ride_requests`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
}

// explainJSON runs EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) over one statement
// and returns the plan. ANALYZE (rather than a bare EXPLAIN) so the assertions
// can read ACTUAL row counts — an estimate would let a mis-costed plan pass.
func explainJSON(t *testing.T, sql string, args ...any) planNode {
	t.Helper()
	var raw []byte
	err := testPool.QueryRow(context.Background(),
		"EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&raw)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var wrapper []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatalf("decode plan: %v (raw=%s)", err, raw)
	}
	if len(wrapper) != 1 {
		t.Fatalf("expected one plan, got %d", len(wrapper))
	}
	return wrapper[0].Plan
}

// planNode is the slice of EXPLAIN's JSON this test reads. String() renders the
// node back as indented JSON so a failure prints the plan that caused it.
type planNode struct {
	NodeType   string     `json:"Node Type"`
	IndexName  string     `json:"Index Name"`
	IndexCond  string     `json:"Index Cond"`
	Filter     string     `json:"Filter"`
	ActualRows float64    `json:"Actual Rows"`
	Plans      []planNode `json:"Plans"`
}

func (p planNode) String() string {
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Sprintf("<unrenderable plan: %v>", err)
	}
	return string(out)
}

// indexCondsFor walks the plan and returns the Index Cond and actual row count
// of every scan of the named index.
func indexCondsFor(node planNode, index string) (conds []string, rows []int) {
	if node.IndexName == index {
		conds = append(conds, node.IndexCond)
		rows = append(rows, int(node.ActualRows))
	}
	for _, child := range node.Plans {
		childConds, childRows := indexCondsFor(child, index)
		conds = append(conds, childConds...)
		rows = append(rows, childRows...)
	}
	return conds, rows
}
