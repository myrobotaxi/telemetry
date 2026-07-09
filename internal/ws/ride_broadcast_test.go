package ws

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// testClient registers a minimal client with a readable send buffer.
func testClient(hub *Hub, userID string) *Client {
	c := &Client{userID: userID, send: make(chan []byte, 8)}
	hub.Register(c)
	return c
}

// drainOne returns the next frame on the client's send channel, or fails if
// none arrives promptly.
func drainOne(t *testing.T, c *Client) wsMessage {
	t.Helper()
	select {
	case raw := <-c.send:
		var m wsMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		return m
	case <-time.After(time.Second):
		t.Fatal("expected a frame, got none")
		return wsMessage{}
	}
}

// assertNoFrame fails if the client received any frame.
func assertNoFrame(t *testing.T, c *Client) {
	t.Helper()
	select {
	case raw := <-c.send:
		t.Fatalf("expected no frame, got %s", raw)
	default:
	}
}

func newRideBroadcaster(hub *Hub) *Broadcaster {
	return NewBroadcaster(hub, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRideBroadcaster_Created_UnicastsToParties(t *testing.T) {
	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	rider := testClient(hub, "clrider")
	owner := testClient(hub, "clowner")
	stranger := testClient(hub, "clstranger") // shares the vehicle but is not a party

	b := newRideBroadcaster(hub)
	sched := time.Date(2026, 6, 18, 16, 0, 0, 0, time.UTC)
	b.handleRideRequestCreated(context.Background(), events.NewEvent(events.RideRequestCreatedEvent{
		RideRequestID: "crr1",
		VehicleID:     "clveh",
		RiderID:       "clrider",
		OwnerID:       "clowner",
		Status:        "requested",
		ScheduledFor:  &sched,
		CreatedAt:     time.Date(2026, 6, 15, 16, 12, 0, 0, time.UTC),
	}))

	for _, c := range []*Client{rider, owner} {
		m := drainOne(t, c)
		if m.Type != msgTypeRideRequestCreated {
			t.Errorf("type: got %q", m.Type)
		}
		var p rideRequestCreatedPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if p.RideRequestID != "crr1" || p.VehicleID != "clveh" || p.RiderID != "clrider" || p.Status != "requested" {
			t.Errorf("payload fields: %+v", p)
		}
		if p.ScheduledFor == nil || *p.ScheduledFor != "2026-06-18T16:00:00Z" {
			t.Errorf("scheduledFor: %v", p.ScheduledFor)
		}
		if p.Timestamp != "2026-06-15T16:12:00Z" {
			t.Errorf("timestamp: %q", p.Timestamp)
		}
	}
	assertNoFrame(t, stranger)
}

func TestRideBroadcaster_Created_OmitsScheduledForOnDemand(t *testing.T) {
	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)
	rider := testClient(hub, "clrider")

	b := newRideBroadcaster(hub)
	b.handleRideRequestCreated(context.Background(), events.NewEvent(events.RideRequestCreatedEvent{
		RideRequestID: "crr2", VehicleID: "clveh", RiderID: "clrider", OwnerID: "clowner",
		Status: "requested", ScheduledFor: nil, CreatedAt: time.Now().UTC(),
	}))
	m := drainOne(t, rider)
	if contains(m.Payload, "scheduledFor") {
		t.Errorf("on-demand frame must omit scheduledFor: %s", m.Payload)
	}
}

func TestRideBroadcaster_StatusChanged_UnicastsAndOmitsReschedule(t *testing.T) {
	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)
	rider := testClient(hub, "clrider")
	owner := testClient(hub, "clowner")

	b := newRideBroadcaster(hub)
	b.handleRideStatusChanged(context.Background(), events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: "crr3", VehicleID: "clveh", RiderID: "clrider", OwnerID: "clowner",
		Status: "cancelled", RescheduleStatus: nil, UpdatedAt: time.Date(2026, 6, 15, 16, 14, 30, 0, time.UTC),
	}))
	for _, c := range []*Client{rider, owner} {
		m := drainOne(t, c)
		if m.Type != msgTypeRideStatusChanged {
			t.Errorf("type: got %q", m.Type)
		}
		if contains(m.Payload, "rescheduleStatus") {
			t.Errorf("no-reschedule frame must omit rescheduleStatus: %s", m.Payload)
		}
		var p rideStatusChangedPayload
		_ = json.Unmarshal(m.Payload, &p)
		if p.Status != "cancelled" || p.Timestamp != "2026-06-15T16:14:30Z" {
			t.Errorf("payload: %+v", p)
		}
	}
}

func TestRideBroadcaster_StatusChanged_CarriesReschedule(t *testing.T) {
	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)
	rider := testClient(hub, "clrider")

	b := newRideBroadcaster(hub)
	rs := "requested"
	b.handleRideStatusChanged(context.Background(), events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: "crr4", VehicleID: "clveh", RiderID: "clrider", OwnerID: "clowner",
		Status: "accepted", RescheduleStatus: &rs, UpdatedAt: time.Now().UTC(),
	}))
	m := drainOne(t, rider)
	var p rideStatusChangedPayload
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.RescheduleStatus == nil || *p.RescheduleStatus != "requested" {
		t.Errorf("rescheduleStatus: %v", p.RescheduleStatus)
	}
}

func TestHub_SendToUsers_TargetsAndDedups(t *testing.T) {
	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)
	a := testClient(hub, "userA")
	b := testClient(hub, "userB")
	c := testClient(hub, "userC")

	// Duplicate + empty ids must not double-send or panic.
	hub.SendToUsers([]string{"userA", "userA", "", "userB"}, []byte("hello"))

	if got := <-a.send; string(got) != "hello" {
		t.Errorf("a: %q", got)
	}
	if len(a.send) != 0 {
		t.Errorf("a received duplicate frame")
	}
	if got := <-b.send; string(got) != "hello" {
		t.Errorf("b: %q", got)
	}
	assertNoFrame(t, c)

	// Empty / nil target lists are no-ops.
	hub.SendToUsers(nil, []byte("x"))
	hub.SendToUsers([]string{""}, []byte("x"))
	assertNoFrame(t, a)
}

// contains reports whether the JSON payload contains the given key literal.
func contains(payload json.RawMessage, key string) bool {
	return bytes.Contains(payload, []byte(`"`+key+`"`))
}
