package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-587 — the Live Activity's view of a multi-stop trip, against a real
// database. The copy rule itself lives in internal/push and is unit-tested
// there; what these assert is that both content-state read paths actually
// deliver it the stop list, in travel order, and that a two-endpoint ride
// still delivers nothing.

// seedActivityStop inserts one stop at the given position.
func seedActivityStop(t *testing.T, rideID, id string, position int, label, status string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO go_ride_stops (id, ride_id, position, lat_enc, lng_enc, label, status)
		 VALUES ($1, $2, $3, 'enc', 'enc', $4, $5)`,
		id, rideID, position, label, status); err != nil {
		t.Fatalf("seed stop %s: %v", id, err)
	}
}

// TestActivityStopsForRides_TravelOrderAndAbsence covers both shapes the copy
// rule reads: a ride with stops, in position order, and a ride with none, which
// must be ABSENT from the map rather than present and empty — the caller reads
// a missing key as "the drop-off is the only place left".
func TestActivityStopsForRides_TravelOrderAndAbsence(t *testing.T) {
	const withStops = "cride0587s1"
	const noStops = "cride0587s2"
	repo := setupLiveActivities(t, withStops)
	seedActivityRide(t, noStops)
	ctx := context.Background()

	// Inserted out of order on purpose: `position` is the travel order, not the
	// insertion order.
	seedActivityStop(t, withStops, "crs0587b", 1, "Nordstrom", "upcoming")
	seedActivityStop(t, withStops, "crs0587a", 0, "TARGET", "current")

	got, err := repo.ActivityStopsForRides(ctx, []string{withStops, noStops})
	if err != nil {
		t.Fatalf("ActivityStopsForRides: %v", err)
	}

	if _, present := got[noStops]; present {
		t.Errorf("a two-endpoint ride appears in the map: %+v", got[noStops])
	}
	stops := got[withStops]
	if len(stops) != 2 {
		t.Fatalf("got %d stops, want 2", len(stops))
	}
	if stops[0].Label != "TARGET" || stops[1].Label != "Nordstrom" {
		t.Errorf("stops = %+v, want travel order TARGET then Nordstrom", stops)
	}
	if stops[0].Status != store.RideStopCurrent {
		t.Errorf("first stop status = %q, want %q", stops[0].Status, store.RideStopCurrent)
	}
}

// TestActivityStopsForRides_NoRidesIssuesNoQuery pins the empty-input arm: the
// ticker's ordinary state is a pass with no live Activity at all, and it must
// not send a statement to find that out.
func TestActivityStopsForRides_NoRidesIssuesNoQuery(t *testing.T) {
	repo := setupLiveActivities(t, "cride0587s3")

	got, err := repo.ActivityStopsForRides(context.Background(), nil)
	if err != nil {
		t.Fatalf("ActivityStopsForRides(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries for no rides, want none", len(got))
	}
}

// TestBothContentStateReadsCarryTheStops is the invariant the projection rests
// on: the lifecycle fan-out's single-row read and the ticker's per-pass list
// must deliver the SAME inputs, or the two send paths would compose different
// cards for one ride.
func TestBothContentStateReadsCarryTheStops(t *testing.T) {
	const ride = "cride0587s4"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	seedActivityStop(t, ride, "crs0587c", 0, "TARGET", "current")
	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests SET status = 'enroute' WHERE id = $1`, ride); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-1", false); err != nil {
		t.Fatalf("RegisterActivity: %v", err)
	}

	rc, err := repo.ActivityContextForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivityContextForRide: %v", err)
	}
	if len(rc.Stops) != 1 || rc.Stops[0].Label != "TARGET" {
		t.Errorf("lifecycle read stops = %+v, want the one current stop", rc.Stops)
	}

	legs, err := repo.ListActiveLegActivities(ctx, 10)
	if err != nil {
		t.Fatalf("ListActiveLegActivities: %v", err)
	}
	if len(legs) != 1 {
		t.Fatalf("got %d legs, want 1", len(legs))
	}
	if len(legs[0].Stops) != 1 || legs[0].Stops[0].Label != "TARGET" {
		t.Errorf("ticker read stops = %+v, want the one current stop", legs[0].Stops)
	}
	// The drop-off is still carried alongside: the final leg goes back to it.
	if legs[0].DropoffLabel != rc.DropoffLabel {
		t.Errorf("dropoff labels disagree across the two reads: %q vs %q",
			legs[0].DropoffLabel, rc.DropoffLabel)
	}
}
