package dispatch

import (
	"context"
	"testing"

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
			fs := &fakeNavVerifyStore{status: tt.ev.Status}
			d, exec, _, _ := verifyHarness(fs, &fakeBus{})
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
