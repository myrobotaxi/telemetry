package store_test

// MYR-550 — what the auto-arrival detector is actually WATCHING once a ride
// under way has stops.
//
// This read is the pointer the whole downstream chain hangs off: the waypoint
// it names decides which coordinate the dwell test measures against, which
// ride.waypoint_arrived is published, which leg advances, and whether MYR-542
// flashes. When it names the DESTINATION for a ride whose car is driving to a
// stop, none of that happens and nothing anywhere logs a complaint — the
// failure mode MYR-550 is titled after. There was no test on it at all.

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

const (
	arrivalVehicle = "clarrveh00000000000001"
	arrivalVIN     = "7SAYGDET7TA614001"
	arrivalRider   = "clarrrider000000000001"
)

// enrouteRideWithStops seeds one ride on the arrival vehicle, drives it to
// `enroute`, and gives it the supplied stops through the real guarded trip
// write — so the stop statuses under test are the ones the production path
// produces, never ones the fixture stamped by hand.
func enrouteRideWithStops(
	t *testing.T, repo *store.RideRequestRepo, labels ...string,
) store.RideRequestRecord {
	t.Helper()
	ctx := context.Background()

	created, err := repo.Create(ctx, store.RideRequestRecord{
		RiderID:   arrivalRider,
		OwnerID:   "clowner1234567890abcdef",
		VehicleID: arrivalVehicle,
		Pickup:    store.RidePlace{Latitude: 37.7749, Longitude: -122.4194, Label: "Pickup"},
		Dropoff:   store.RidePlace{Latitude: 37.7766, Longitude: -122.3946, Label: "Dropoff"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec, err := repo.UpdateStatus(ctx, created.ID, store.RideRequestStatusEnroute)
	if err != nil {
		t.Fatalf("UpdateStatus(enroute): %v", err)
	}
	if len(labels) == 0 {
		return rec
	}

	stops := make([]store.RideStopEdit, 0, len(labels))
	for i, label := range labels {
		stops = append(stops, store.RideStopEdit{
			Place: store.RidePlace{
				Latitude:  37.80 + float64(i)/100,
				Longitude: -122.42 + float64(i)/100,
				Label:     label,
			},
		})
	}
	rec, err = repo.UpdateTrip(ctx, rec.ID,
		store.RideTripEdit{Stops: &stops, ExpectVersion: rec.TripVersion}, tripEditableFrom)
	if err != nil {
		t.Fatalf("UpdateTrip: %v", err)
	}
	return rec
}

// onlyCandidate reads the candidate set and insists on exactly one row — a
// second would mean the fixture leaked a ride and every assertion below would
// be measuring the wrong car.
func onlyCandidate(t *testing.T, repo *store.RideRequestRepo) store.ArrivalCandidate {
	t.Helper()
	cands, err := repo.ListArrivalCandidates(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListArrivalCandidates: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want exactly 1 (%+v)", len(cands), cands)
	}
	return cands[0]
}

// setupArrivalRide wires the vehicle the candidate read joins through.
func setupArrivalRide(t *testing.T) *store.RideRequestRepo {
	t.Helper()
	repo, _ := setupRideRequestRepo(t)
	seedVehicle(t, testPool, arrivalVehicle, arrivalVIN)
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM "Vehicle"`) })
	return repo
}

// A stop promoted by the trip write becomes the watched waypoint immediately —
// the join between MYR-550's promotion and the MYR-538 detector, and the step
// whose absence made the promotion pointless.
func TestListArrivalCandidates_WatchesThePromotedStop(t *testing.T) {
	repo := setupArrivalRide(t)
	rec := enrouteRideWithStops(t, repo, "Bakery", "Pharmacy")
	if rec.Stops[0].Status != store.RideStopCurrent {
		t.Fatalf("fixture: first stop = %q, want current", rec.Stops[0].Status)
	}

	got := onlyCandidate(t, repo)
	if want := "stop:" + rec.Stops[0].ID; got.Waypoint != want {
		t.Errorf("waypoint = %q, want %q", got.Waypoint, want)
	}
	if got.TargetLatitude != rec.Stops[0].Place.Latitude ||
		got.TargetLongitude != rec.Stops[0].Place.Longitude {
		t.Errorf("target = (%v, %v), want the current stop's coordinate (%v, %v)",
			got.TargetLatitude, got.TargetLongitude,
			rec.Stops[0].Place.Latitude, rec.Stops[0].Place.Longitude)
	}
}

// THE REGRESSION MYR-550 CLOSES. An enroute ride whose stops are all upcoming
// is a broken invariant — reachable when StartFirstStop's promotion is lost,
// which the Start deliberately tolerates rather than stranding a rider aboard.
// Before the fallback the detector watched the DESTINATION here, so the stop
// could never be reached, never advance the leg and never flash; now it watches
// the place the dispatcher's own leg target names.
func TestListArrivalCandidates_FallsBackToTheEarliestUpcomingStop(t *testing.T) {
	repo := setupArrivalRide(t)
	rec := enrouteRideWithStops(t, repo, "Bakery", "Pharmacy")

	// Simulate the lost promotion directly: nothing in the write paths can
	// produce this state on purpose, which is exactly why it needs a test.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE go_ride_stops SET status = 'upcoming' WHERE ride_id = $1`, rec.ID); err != nil {
		t.Fatalf("demote stops: %v", err)
	}

	got := onlyCandidate(t, repo)
	if want := "stop:" + rec.Stops[0].ID; got.Waypoint != want {
		t.Fatalf("waypoint = %q, want the earliest upcoming stop %q", got.Waypoint, want)
	}
	if got.TargetLatitude != rec.Stops[0].Place.Latitude {
		t.Errorf("target lat = %v, want the earliest upcoming stop's %v",
			got.TargetLatitude, rec.Stops[0].Place.Latitude)
	}
}

// A COMPLETED stop is behind the car and must never be watched again. The
// fallback admits 'upcoming' only, so a trip whose stops are all completed
// falls through to the destination exactly as it always did — otherwise the
// last leg of every multi-stop ride would re-detect an arrival it already had.
func TestListArrivalCandidates_CompletedStopsFallThroughToTheDestination(t *testing.T) {
	repo := setupArrivalRide(t)
	rec := enrouteRideWithStops(t, repo, "Bakery")

	if _, err := testPool.Exec(context.Background(),
		`UPDATE go_ride_stops SET status = 'completed' WHERE ride_id = $1`, rec.ID); err != nil {
		t.Fatalf("complete stops: %v", err)
	}

	got := onlyCandidate(t, repo)
	if got.Waypoint != "destination" {
		t.Errorf("waypoint = %q, want destination once every stop is behind the car", got.Waypoint)
	}
	if got.TargetLatitude != rec.Dropoff.Latitude || got.TargetLongitude != rec.Dropoff.Longitude {
		t.Errorf("target = (%v, %v), want the drop-off", got.TargetLatitude, got.TargetLongitude)
	}
}

// A stopless enroute ride is unchanged by any of this: the destination is the
// leg, as it has been since MYR-538.
func TestListArrivalCandidates_StoplessRideWatchesTheDestination(t *testing.T) {
	repo := setupArrivalRide(t)
	rec := enrouteRideWithStops(t, repo)

	got := onlyCandidate(t, repo)
	if got.Waypoint != "destination" {
		t.Errorf("waypoint = %q, want destination", got.Waypoint)
	}
	if got.TargetLatitude != rec.Dropoff.Latitude {
		t.Errorf("target lat = %v, want the drop-off's %v", got.TargetLatitude, rec.Dropoff.Latitude)
	}
}
