package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-555: the reservation sweeper's dispatch frame reaches the broadcaster on
// the topic it already serves, and the WIRE IS UNCHANGED — that is the whole
// design claim. Had this frame needed a new key, the fix would have been a
// contracts release and a client update instead of a server deploy.

// TestRideBroadcaster_DispatchFrame_AddsNoWireField pins the no-schema-change
// promise. DispatchEdit is a publisher-to-consumer marker and must not appear on
// the wire; the client learns what happened by REFETCHING, which is what the
// frame is for.
func TestRideBroadcaster_DispatchFrame_AddsNoWireField(t *testing.T) {
	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)
	rider := testClient(hub, "clrider")

	b := newRideBroadcaster(hub)
	b.handleRideStatusChanged(context.Background(), events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: "crr555", VehicleID: "clveh", RiderID: "clrider", OwnerID: "clowner",
		Status: "accepted", PreviousStatus: "accepted", TripVersion: 5, DispatchEdit: true,
		UpdatedAt: time.Date(2026, 8, 13, 21, 40, 24, 0, time.UTC),
	}))

	m := drainOne(t, rider)
	if m.Type != msgTypeRideStatusChanged {
		t.Errorf("type = %q, want %q", m.Type, msgTypeRideStatusChanged)
	}

	var payload map[string]any
	if err := json.Unmarshal(m.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	for _, banned := range []string{"dispatchEdit", "dispatchedAt", "dispatchStatus", "previousStatus"} {
		if _, present := payload[banned]; present {
			t.Errorf("frame carries %q — the dispatch fix must add no wire field: %s", banned, m.Payload)
		}
	}
	// What it DOES carry: the unchanged status and the ride's CURRENT trip
	// version, which is the refetch signal a client gates on (MYR-548). A zero
	// here reads as a regression and the client never refetches — which would
	// leave exactly the bug this frame exists to fix.
	if payload["status"] != "accepted" {
		t.Errorf("status = %v, want accepted", payload["status"])
	}
	if v, ok := payload["tripVersion"].(float64); !ok || int(v) != 5 {
		t.Errorf("tripVersion = %v, want 5", payload["tripVersion"])
	}
}

// TestRideBroadcaster_DispatchFrame_ReachesBothParties: the owner is watching
// too. The MYR-535 lead means the car leaves BEFORE the pickup instant, and the
// owner's surface must move at the same moment the rider's does.
func TestRideBroadcaster_DispatchFrame_ReachesBothParties(t *testing.T) {
	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)
	rider := testClient(hub, "clrider")
	owner := testClient(hub, "clowner")

	b := newRideBroadcaster(hub)
	b.handleRideStatusChanged(context.Background(), events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: "crr555", VehicleID: "clveh", RiderID: "clrider", OwnerID: "clowner",
		Status: "accepted", DispatchEdit: true, UpdatedAt: time.Now().UTC(),
	}))

	for _, c := range []*Client{rider, owner} {
		if m := drainOne(t, c); m.Type != msgTypeRideStatusChanged {
			t.Errorf("type = %q, want %q", m.Type, msgTypeRideStatusChanged)
		}
	}
}
