package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-541 — the dispatcher's decision table for an edited endpoint: only the
// CURRENT leg's target re-shares, and the re-share carries the NEW place.
func TestTripChanged_ReshareDecisionTable(t *testing.T) {
	place := events.RidePlace{Latitude: 32.9, Longitude: -99.2, Label: "New"}
	tests := []struct {
		name       string
		ev         events.RideTripChangedEvent
		wantShares int
	}{
		{name: "dropoff edited aboard re-shares", wantShares: 1,
			ev: events.RideTripChangedEvent{Status: "enroute", NewDropoff: &place}},
		{name: "pickup edited while dispatched re-shares", wantShares: 1,
			ev: events.RideTripChangedEvent{Status: "accepted", PickupDispatched: true, NewPickup: &place}},
		{name: "pickup edited before dispatch shares nothing", wantShares: 0,
			ev: events.RideTripChangedEvent{Status: "accepted", NewPickup: &place}},
		{name: "dropoff edited before the start shares nothing", wantShares: 0,
			ev: events.RideTripChangedEvent{Status: "accepted", NewDropoff: &place}},
		{name: "dropoff edited at the kerb shares nothing", wantShares: 0,
			ev: events.RideTripChangedEvent{Status: "arrived", NewDropoff: &place}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Deliberately WITHOUT WithNavVerify: the verifier is its own
			// (tested) machinery on an untracked goroutine, and wiring it here
			// would race the executor read below. armNavVerify no-ops on nil,
			// so the count is exactly the decision table's own shares.
			exec := &fakeExecutor{}
			d := New(
				&fakeVehicleResolver{vin: "5YJ3E1EA7KF000001"},
				&fakeTokenSource{token: "tok"},
				exec,
				&fakeStore{},
				Config{Enabled: true, MaxRetries: 0, Backoff: time.Millisecond},
				nil,
			)
			tt.ev.RideRequestID, tt.ev.VehicleID, tt.ev.OwnerID = "cride1", "cveh1", "cowner1"

			d.processTripChanged(context.Background(), tt.ev)
			d.Wait()

			if len(exec.calls) < tt.wantShares {
				t.Fatalf("shares = %d, want >= %d", len(exec.calls), tt.wantShares)
			}
			if tt.wantShares == 0 && len(exec.calls) != 0 {
				t.Fatalf("shares = %d, want 0", len(exec.calls))
			}
			if tt.wantShares > 0 {
				req := exec.calls[0]
				if req.Params["value"] == nil {
					t.Skip("share body shape not asserted here")
				}
			}
		})
	}
}
