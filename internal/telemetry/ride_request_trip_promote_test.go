package telemetry

// MYR-550 — the handler's half of the promotion: the stop the store made
// CURRENT must be the target the dispatcher is told about.
//
// The existing LegTarget tests seed a ride that ALREADY has a current stop, so
// they pin the pass-through and say nothing about the case the issue is
// about — the first stop of a live leg, which does not exist until the write
// that promotes it returns.

import (
	"net/http"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// tripChangedEvent pulls the ride.trip_changed half of the published pair.
func tripChangedEvent(t *testing.T, pub *fakeRidePublisher) events.RideTripChangedEvent {
	t.Helper()
	if len(pub.events) != 2 {
		t.Fatalf("published %d events, want the pair", len(pub.events))
	}
	trip, ok := pub.events[1].Payload.(events.RideTripChangedEvent)
	if !ok {
		t.Fatalf("second event = %T, want RideTripChangedEvent", pub.events[1].Payload)
	}
	return trip
}

// A stop added to a ride ALREADY UNDER WAY: the write promotes it, the wire
// shows it current, and the event names it as the leg target. All three come
// from the one record the guarded write returned, which is what keeps the
// client, the dispatcher and the arrival detector pointed at the same place.
func TestRideTripPatch_FirstStopOnALiveLegBecomesTheLegTarget(t *testing.T) {
	store := &fakeRideStore{getRec: fixtureRideData(rideOtherUsr, rideStatusEnroute)}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	resp := tripPatch(t, h,
		`{"tripVersion":0,"stops":[{"place":{"lat":37.8109,"lng":-122.4103,"label":"Bakery"}}]}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("status %d: %s", resp.Code, resp.Body.String())
	}

	trip := tripChangedEvent(t, pub)
	if !trip.StopsChanged {
		t.Error("StopsChanged must be set")
	}
	if trip.LegTarget == nil {
		t.Fatal("LegTarget must be set on a stops edit")
	}
	if trip.LegTarget.Latitude != 37.8109 || trip.LegTarget.Longitude != -122.4103 {
		t.Errorf("LegTarget = %+v, want the newly promoted stop", trip.LegTarget)
	}
	// The drop-off must NOT be the target: that is the pre-MYR-550 answer, and
	// it is the one a car would actually drive to.
	if trip.LegTarget.Label != "Bakery" {
		t.Errorf("LegTarget label = %q, want the stop rather than the destination", trip.LegTarget.Label)
	}
}

// currentLegTarget is the Go twin of the arrival read's LATERAL: current stop,
// else EARLIEST UPCOMING, else the drop-off. The middle rung is the one that
// matters — it is what a lost Start promotion lands on, and the two resolvers
// disagreeing is precisely how a car ends up driving to a place nothing is
// watching.
func TestCurrentLegTarget_Ladder(t *testing.T) {
	tests := []struct {
		name         string
		stopStatuses []string
		wantLabel    string
	}{
		{
			name:         "the current stop wins",
			stopStatuses: []string{rideStopCompleted, rideStopCurrent, rideStopUpcoming},
			wantLabel:    "Stop",
		},
		{
			// No current stop: the invariant is broken, and the honest answer
			// is the place the car is on its way to.
			name:         "no current stop falls back to the earliest upcoming",
			stopStatuses: []string{rideStopUpcoming, rideStopUpcoming},
			wantLabel:    "Stop",
		},
		{
			name:         "every stop behind the car is the drop-off",
			stopStatuses: []string{rideStopCompleted, rideStopCompleted},
			wantLabel:    "Dropoff",
		},
		{
			name:         "a stopless trip is the drop-off",
			stopStatuses: nil,
			wantLabel:    "Dropoff",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := rideWithStops(rideStatusEnroute, tt.stopStatuses...)
			rec.Dropoff.Label = "Dropoff"

			got := currentLegTarget(rec)
			if got.Label != tt.wantLabel {
				t.Fatalf("target label = %q, want %q", got.Label, tt.wantLabel)
			}
			if tt.wantLabel != "Stop" {
				return
			}
			// Which stop, not merely "a stop": the first current one, or with
			// none the EARLIEST upcoming — never a later one.
			want := wantedStop(rec, tt.stopStatuses)
			if got.Latitude != want.Latitude {
				t.Errorf("target = %+v, want stop %+v", got, want)
			}
		})
	}
}

// wantedStop is the expected stop chosen independently of currentLegTarget, so
// the assertion is not the implementation restated.
func wantedStop(rec RideRequestData, statuses []string) RidePlaceData {
	for i, st := range statuses {
		if st == rideStopCurrent {
			return rec.Stops[i].Place
		}
	}
	for i, st := range statuses {
		if st == rideStopUpcoming {
			return rec.Stops[i].Place
		}
	}
	return rec.Dropoff
}
