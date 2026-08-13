package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-550 — the PROMOTION half of the guarded trip write, through the real
// transaction.
//
// The rules it pins are the ones a pure planner test cannot reach, because the
// promotion is not a planning decision: it is a statement that runs after the
// plan has been applied, reads the list it just wrote, and must be reflected in
// the row the write RETURNS. Everything downstream of a stop — the MYR-538
// detector's target, the leg advance, the MYR-542 flash — hangs off the single
// bit these tests assert, and it had none before.

// tripEditableFrom mirrors the handler's drop-off window, which is the window a
// stops edit takes (a stop is the trip AHEAD of the car).
var tripEditableFrom = []string{"requested", "accepted", "arrived", "enroute"}

// startedRide creates a ride and walks it to `enroute` — the status in which
// the rider is aboard and leg 2 is the car's business. Returns the ride.
func startedRide(t *testing.T, repo *store.RideRequestRepo) store.RideRequestRecord {
	t.Helper()
	ctx := context.Background()
	rec, err := repo.Create(ctx, scheduledRideRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ladder := []struct{ from, to store.RideRequestStatus }{
		{store.RideRequestStatusRequested, store.RideRequestStatusAccepted},
		{store.RideRequestStatusAccepted, store.RideRequestStatusArrived},
		{store.RideRequestStatusArrived, store.RideRequestStatusEnroute},
	}
	for _, step := range ladder {
		if rec, err = repo.UpdateStatusFrom(ctx, rec.ID,
			[]store.RideRequestStatus{step.from}, step.to); err != nil {
			t.Fatalf("advance %s -> %s: %v", step.from, step.to, err)
		}
	}
	return rec
}

// acceptedRide creates a ride and stops at `accepted` — the car is still
// driving to the pickup and no stop of it is the current leg.
func acceptedRide(t *testing.T, repo *store.RideRequestRepo) store.RideRequestRecord {
	t.Helper()
	ctx := context.Background()
	rec, err := repo.Create(ctx, scheduledRideRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec, err = repo.UpdateStatusFrom(ctx, rec.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusAccepted)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return rec
}

// place builds a distinguishable stop place.
func place(label string, lat, lng float64) store.RidePlace {
	return store.RidePlace{Latitude: lat, Longitude: lng, Label: label}
}

// wantStopStatuses asserts the whole list's statuses in travel order, which is
// the only way to say "the FIRST one was promoted" without the assertion being
// true of several different wrong answers.
func wantStopStatuses(t *testing.T, got []store.RideStop, want ...store.RideStopStatus) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("stops = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Status != want[i] {
			t.Errorf("stop %d (%s) status = %q, want %q", i, got[i].Place.Label, got[i].Status, want[i])
		}
	}
}

// A stop added to a LIVE leg becomes the current one in the same transaction —
// the MYR-550 fix's whole point. The assertion is on the RETURNED record
// because that row is the wire answer the editor folds and the source the
// TripEdit event's LegTarget is computed from: a promotion the database made
// but the response did not show would still leave the client and the
// dispatcher aiming at the drop-off.
func TestUpdateTrip_StopAddedWhileEnroutePromotesToCurrent(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	rec := startedRide(t, repo)

	stops := []store.RideStopEdit{{Place: place("Bakery", 37.81, -122.41)}}
	updated, err := repo.UpdateTrip(ctx, rec.ID,
		store.RideTripEdit{Stops: &stops, ExpectVersion: rec.TripVersion}, tripEditableFrom)
	if err != nil {
		t.Fatalf("UpdateTrip: %v", err)
	}

	wantStopStatuses(t, updated.Stops, store.RideStopCurrent)
	if updated.Stops[0].ID == "" {
		t.Error("the promoted stop must carry its server-minted id")
	}
	// The place must survive the promotion: the promote statement rewrites
	// status, and a read-back that lost the coordinate would send the car to
	// the wrong kerb while looking correct.
	if got := updated.Stops[0].Place; got.Latitude != 37.81 || got.Longitude != -122.41 {
		t.Errorf("promoted stop place = %v, want the submitted coordinate", got)
	}
}

// A SECOND stop joins the list behind the first: the promotion is idempotent,
// so the stop the car is already driving to keeps the leg. A promoter that
// re-ran unconditionally would silently re-aim a moving car.
func TestUpdateTrip_SecondStopLeavesTheFirstCurrent(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	rec := startedRide(t, repo)

	first := []store.RideStopEdit{{Place: place("Bakery", 37.81, -122.41)}}
	updated, err := repo.UpdateTrip(ctx, rec.ID,
		store.RideTripEdit{Stops: &first, ExpectVersion: rec.TripVersion}, tripEditableFrom)
	if err != nil {
		t.Fatalf("UpdateTrip (first stop): %v", err)
	}

	both := []store.RideStopEdit{
		{ID: updated.Stops[0].ID, Place: updated.Stops[0].Place},
		{Place: place("Pharmacy", 37.79, -122.40)},
	}
	updated, err = repo.UpdateTrip(ctx, rec.ID,
		store.RideTripEdit{Stops: &both, ExpectVersion: updated.TripVersion}, tripEditableFrom)
	if err != nil {
		t.Fatalf("UpdateTrip (second stop): %v", err)
	}

	wantStopStatuses(t, updated.Stops, store.RideStopCurrent, store.RideStopUpcoming)
	if updated.Stops[0].Place.Label != "Bakery" {
		t.Errorf("the current stop moved: %q, want Bakery still first", updated.Stops[0].Place.Label)
	}
}

// An enroute edit that moves ONLY the drop-off must not touch a stop status.
// The promotion lives in the stop-replacement half of the transaction, and an
// endpoint-only edit never enters it — a car mid-leg must not have its target
// re-decided because somebody retyped the destination.
func TestUpdateTrip_DropoffOnlyEditPromotesNothing(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	rec := startedRide(t, repo)

	// Seed two stops BEFORE the ride starts moving anything: added while
	// enroute, so the first is current and the second upcoming.
	stops := []store.RideStopEdit{
		{Place: place("Bakery", 37.81, -122.41)},
		{Place: place("Pharmacy", 37.79, -122.40)},
	}
	updated, err := repo.UpdateTrip(ctx, rec.ID,
		store.RideTripEdit{Stops: &stops, ExpectVersion: rec.TripVersion}, tripEditableFrom)
	if err != nil {
		t.Fatalf("UpdateTrip (seed stops): %v", err)
	}
	wantStopStatuses(t, updated.Stops, store.RideStopCurrent, store.RideStopUpcoming)

	newDropoff := place("Caltrain", 37.7766, -122.3946)
	updated, err = repo.UpdateTrip(ctx, rec.ID,
		store.RideTripEdit{Dropoff: &newDropoff, ExpectVersion: updated.TripVersion}, tripEditableFrom)
	if err != nil {
		t.Fatalf("UpdateTrip (dropoff): %v", err)
	}

	// The whole list comes back on an endpoint-only edit, and it comes back
	// unchanged.
	wantStopStatuses(t, updated.Stops, store.RideStopCurrent, store.RideStopUpcoming)
	if updated.Dropoff.Label != "Caltrain" {
		t.Errorf("dropoff = %q, want the edited one", updated.Dropoff.Label)
	}
}

// A stops edit on a ride that has NOT started leaves every stop upcoming.
// Promotion for that phase belongs to StartFirstStop and must stay there: a
// stop promoted while the car is still driving to the PICKUP would point the
// MYR-538 detector at the stop and let the ride skip its own pickup.
func TestUpdateTrip_StopsOnAnUnstartedRideStayUpcoming(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	for _, tt := range []struct {
		name string
		ride func(*testing.T, *store.RideRequestRepo) store.RideRequestRecord
	}{
		{"requested", func(t *testing.T, r *store.RideRequestRepo) store.RideRequestRecord {
			t.Helper()
			rec, err := r.Create(ctx, scheduledRideRequest())
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			return rec
		}},
		{"accepted", acceptedRide},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := tt.ride(t, repo)
			stops := []store.RideStopEdit{
				{Place: place("Bakery", 37.81, -122.41)},
				{Place: place("Pharmacy", 37.79, -122.40)},
			}
			updated, err := repo.UpdateTrip(ctx, rec.ID,
				store.RideTripEdit{Stops: &stops, ExpectVersion: rec.TripVersion}, tripEditableFrom)
			if err != nil {
				t.Fatalf("UpdateTrip: %v", err)
			}
			wantStopStatuses(t, updated.Stops, store.RideStopUpcoming, store.RideStopUpcoming)
		})
	}
}

// The Start still promotes for the phase the trip write deliberately leaves
// alone — the two promoters meet here and neither is redundant. This is the
// path the prod ride of MYR-550 would have taken had it ever started: its stop
// was added at `accepted` (correctly upcoming), and only the Start it never
// received would have made it current.
func TestStartFirstStop_PromotesAStopAddedBeforeTheRideStarted(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	rec := acceptedRide(t, repo)

	stops := []store.RideStopEdit{{Place: place("Bakery", 37.81, -122.41)}}
	updated, err := repo.UpdateTrip(ctx, rec.ID,
		store.RideTripEdit{Stops: &stops, ExpectVersion: rec.TripVersion}, tripEditableFrom)
	if err != nil {
		t.Fatalf("UpdateTrip: %v", err)
	}
	wantStopStatuses(t, updated.Stops, store.RideStopUpcoming)

	promoted, err := repo.StartFirstStop(ctx, rec.ID)
	if err != nil {
		t.Fatalf("StartFirstStop: %v", err)
	}
	if promoted == nil {
		t.Fatal("StartFirstStop promoted nothing, want the ride's only stop")
	}
	if promoted.ID != updated.Stops[0].ID {
		t.Errorf("promoted %q, want the ride's first stop %q", promoted.ID, updated.Stops[0].ID)
	}

	// Idempotent: the retried Start of an already-promoted trip must not move
	// the leg on to the next stop.
	again, err := repo.StartFirstStop(ctx, rec.ID)
	if err != nil {
		t.Fatalf("StartFirstStop (retry): %v", err)
	}
	if again != nil {
		t.Errorf("a retried Start promoted %q, want nothing", again.ID)
	}
}
