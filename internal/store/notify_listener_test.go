package store

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// fakeBus is a minimal events.Bus stub that captures published events.
type fakeBus struct {
	mu     sync.Mutex
	events []events.Event
	err    error
	count  atomic.Int32
}

func (b *fakeBus) Publish(_ context.Context, evt events.Event) error {
	if b.err != nil {
		return b.err
	}
	b.mu.Lock()
	b.events = append(b.events, evt)
	b.mu.Unlock()
	b.count.Add(1)
	return nil
}

func (b *fakeBus) Subscribe(_ events.Topic, _ events.Handler) (events.Subscription, error) {
	return events.Subscription{}, nil
}
func (b *fakeBus) Unsubscribe(_ events.Subscription) error { return nil }
func (b *fakeBus) Close(_ context.Context) error          { return nil }

func newTestListener(bus events.Bus) *NotifyListener {
	return NewNotifyListener(NotifyListenerConfig{}, bus, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestNotifyListener_HandleNotification_HappyPath(t *testing.T) {
	bus := &fakeBus{}
	listener := newTestListener(bus)

	payload, _ := json.Marshal(vehicleDeletedPayload{
		VehicleID: "veh-1",
		UserID:    "usr-1",
		VIN:       "5YJ3E1EA1NF000002",
	})

	listener.handleNotification(context.Background(), VehicleDeletedChannel, string(payload))

	if got := bus.count.Load(); got != 1 {
		t.Fatalf("Publish call count = %d, want 1", got)
	}
	bus.mu.Lock()
	defer bus.mu.Unlock()
	got, ok := bus.events[0].Payload.(events.VehicleDeletedEvent)
	if !ok {
		t.Fatalf("Payload type = %T, want VehicleDeletedEvent", bus.events[0].Payload)
	}
	if got.VehicleID != "veh-1" || got.UserID != "usr-1" || got.VIN != "5YJ3E1EA1NF000002" {
		t.Errorf("event = %+v, want VehicleID=veh-1 UserID=usr-1 VIN=5YJ3...", got)
	}
}

func TestNotifyListener_HandleNotification_EmptyVIN(t *testing.T) {
	// Pre-pairing vehicles delete with a null vin. The listener must
	// still publish so the WS hub closes any subscribed clients.
	bus := &fakeBus{}
	listener := newTestListener(bus)

	payload, _ := json.Marshal(vehicleDeletedPayload{
		VehicleID: "veh-1",
		UserID:    "usr-1",
	})

	listener.handleNotification(context.Background(), VehicleDeletedChannel, string(payload))

	if got := bus.count.Load(); got != 1 {
		t.Fatalf("Publish call count = %d, want 1", got)
	}
	bus.mu.Lock()
	defer bus.mu.Unlock()
	got := bus.events[0].Payload.(events.VehicleDeletedEvent)
	if got.VIN != "" {
		t.Errorf("VIN = %q, want empty string", got.VIN)
	}
}

func TestNotifyListener_HandleNotification_MalformedJSONDropped(t *testing.T) {
	bus := &fakeBus{}
	listener := newTestListener(bus)

	listener.handleNotification(context.Background(), VehicleDeletedChannel, "{not valid json")

	if got := bus.count.Load(); got != 0 {
		t.Errorf("Publish call count = %d, want 0 (malformed payload should be dropped)", got)
	}
}

func TestNotifyListener_HandleNotification_MissingIdentifiersDropped(t *testing.T) {
	bus := &fakeBus{}
	listener := newTestListener(bus)

	tests := []struct {
		name    string
		payload vehicleDeletedPayload
	}{
		{"empty vehicle id", vehicleDeletedPayload{UserID: "usr-1"}},
		{"empty user id", vehicleDeletedPayload{VehicleID: "veh-1"}},
		{"both empty", vehicleDeletedPayload{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := bus.count.Load()
			payload, _ := json.Marshal(tt.payload)
			listener.handleNotification(context.Background(), VehicleDeletedChannel, string(payload))
			if got := bus.count.Load(); got != before {
				t.Errorf("Publish call count delta = %d, want 0 (event should be dropped)", got-before)
			}
		})
	}
}

func TestNotifyListener_HandleNotification_UnknownChannelIgnored(t *testing.T) {
	bus := &fakeBus{}
	listener := newTestListener(bus)

	payload, _ := json.Marshal(vehicleDeletedPayload{VehicleID: "v", UserID: "u"})
	listener.handleNotification(context.Background(), "some_other_channel", string(payload))

	if got := bus.count.Load(); got != 0 {
		t.Errorf("Publish call count = %d, want 0 (wrong channel should be ignored)", got)
	}
}

func TestNotifyListener_DefaultsApplied(t *testing.T) {
	listener := NewNotifyListener(NotifyListenerConfig{}, &fakeBus{}, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	if listener.cfg.ReconnectBackoff != time.Second {
		t.Errorf("ReconnectBackoff = %v, want 1s default", listener.cfg.ReconnectBackoff)
	}
	if listener.cfg.MaxReconnectBackoff != 30*time.Second {
		t.Errorf("MaxReconnectBackoff = %v, want 30s default", listener.cfg.MaxReconnectBackoff)
	}
}
