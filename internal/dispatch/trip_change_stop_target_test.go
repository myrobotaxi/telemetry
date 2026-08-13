package dispatch

// MYR-550 — the re-share must land on the stop the store just PROMOTED, not
// merely on "some place".
//
// The existing stops-edit test counts pushes, which is the right assertion for
// "one edit is one re-share" and no assertion at all about WHERE the car is
// sent. A dispatcher that re-shared the drop-off on a stops edit would satisfy
// it exactly, and the resulting ride is the MYR-550 failure with the symptom
// inverted: the statuses would be right and the car would drive past the stop.

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// The promoted stop's coordinate is what reaches Tesla. The event field the
// dispatcher reads (LegTarget) is computed by the handler from the record the
// guarded write returned, so this pins the last link of the chain that starts
// at the promotion: promoted stop → RETURNed row → LegTarget → nav value.
func TestTripChanged_ResharesThePromotedStopsCoordinate(t *testing.T) {
	promoted := events.RidePlace{Latitude: 37.8109, Longitude: -122.4103, Label: "Bakery"}
	dropoff := events.RidePlace{Latitude: 37.7766, Longitude: -122.3946, Label: "Caltrain"}

	exec := &fakeExecutor{}
	d := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000550"},
		&fakeTokenSource{token: "tok"},
		exec,
		&fakeStore{},
		Config{Enabled: true, Backoff: time.Millisecond},
		nil,
	)

	d.processTripChanged(context.Background(), events.RideTripChangedEvent{
		RideRequestID: "cride550", VehicleID: "cveh550", OwnerID: "cowner550",
		Status:       "enroute",
		StopsChanged: true,
		LegTarget:    &promoted,
		// The drop-off arm is deliberately armed too: the stops arm SUPERSEDES
		// it, so a regression that let both fire would show up here as the
		// wrong coordinate rather than as a second call nobody looked at.
		NewDropoff: &dropoff,
	})
	d.Wait()

	if len(exec.calls) != 1 {
		t.Fatalf("Tesla shares = %d, want exactly 1", len(exec.calls))
	}
	const want = "37.8109,-122.4103"
	if got := exec.calls[0].Params["value"]; got != want {
		t.Errorf("nav value = %v, want the promoted stop %q (the drop-off is %v)",
			got, want, "37.7766,-122.3946")
	}
}

// With every stop cleared the leg target IS the drop-off, and the same arm
// carries it — which is what makes "the stops arm supersedes the dropoff arm"
// safe rather than a way to lose an edited destination.
func TestTripChanged_ClearedStopsResharesTheDropoff(t *testing.T) {
	dropoff := events.RidePlace{Latitude: 37.7766, Longitude: -122.3946, Label: "Caltrain"}

	exec := &fakeExecutor{}
	d := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000551"},
		&fakeTokenSource{token: "tok"},
		exec,
		&fakeStore{},
		Config{Enabled: true, Backoff: time.Millisecond},
		nil,
	)

	d.processTripChanged(context.Background(), events.RideTripChangedEvent{
		RideRequestID: "cride551", VehicleID: "cveh551", OwnerID: "cowner551",
		Status: "enroute", StopsChanged: true,
		LegTarget: &dropoff, NewDropoff: &dropoff,
	})
	d.Wait()

	if len(exec.calls) != 1 {
		t.Fatalf("Tesla shares = %d, want exactly 1", len(exec.calls))
	}
	if got := exec.calls[0].Params["value"]; got != "37.7766,-122.3946" {
		t.Errorf("nav value = %v, want the drop-off", got)
	}
}
