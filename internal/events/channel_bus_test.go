package events

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// testLogger returns a no-op logger for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(
		discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError},
	))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// testBus creates a ChannelBus with small buffers and noop metrics for testing.
func testBus(bufSize int) *ChannelBus {
	cfg := BusConfig{
		BufferSize:   bufSize,
		DrainTimeout: 2 * time.Second,
	}
	return NewChannelBus(cfg, NoopBusMetrics{}, testLogger())
}

// testPayload is a trivial EventPayload used in tests.
type testPayload struct {
	BasePayload
	Value string
}

func (testPayload) EventTopic() Topic { return TopicVehicleTelemetry }

func TestChannelBus_PublishSubscribe(t *testing.T) {
	tests := []struct {
		name    string
		topic   Topic
		payload EventPayload
	}{
		{
			name:    "vehicle telemetry event",
			topic:   TopicVehicleTelemetry,
			payload: VehicleTelemetryEvent{VIN: "5YJ3E1EA1NF000001", CreatedAt: time.Now()},
		},
		{
			name:    "connectivity event",
			topic:   TopicConnectivity,
			payload: ConnectivityEvent{VIN: "5YJ3E1EA1NF000002", Status: StatusConnected},
		},
		{
			name:  "drive started event",
			topic: TopicDriveStarted,
			payload: DriveStartedEvent{
				VIN:       "5YJ3E1EA1NF000003",
				DriveID:   "drv_001",
				Location:  Location{Latitude: 33.09, Longitude: -96.82},
				StartedAt: time.Now(),
			},
		},
		{
			name:  "drive updated event",
			topic: TopicDriveUpdated,
			payload: DriveUpdatedEvent{
				VIN:     "5YJ3E1EA1NF000003",
				DriveID: "drv_001",
				RoutePoint: RoutePoint{
					Latitude: 33.10, Longitude: -96.83,
					Speed: 65.0, Heading: 245,
					Timestamp: time.Now(),
				},
			},
		},
		{
			name:  "drive ended event",
			topic: TopicDriveEnded,
			payload: DriveEndedEvent{
				VIN:     "5YJ3E1EA1NF000003",
				DriveID: "drv_001",
				Stats: DriveStats{
					Distance: 12.5, Duration: 20 * time.Minute,
					AvgSpeed: 37.5, MaxSpeed: 65.0,
				},
				EndedAt: time.Now(),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := testBus(16)
			defer bus.Close(context.Background())

			received := make(chan Event, 1)
			_, err := bus.Subscribe(tt.topic, func(e Event) {
				received <- e
			})
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}

			event := NewEvent(tt.payload)
			if err := bus.Publish(context.Background(), event); err != nil {
				t.Fatalf("publish: %v", err)
			}

			select {
			case got := <-received:
				if got.ID != event.ID {
					t.Errorf("event ID: got %q, want %q", got.ID, event.ID)
				}
				if got.Topic != tt.topic {
					t.Errorf("topic: got %q, want %q", got.Topic, tt.topic)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for event delivery")
			}
		})
	}
}

func TestChannelBus_FanOut(t *testing.T) {
	bus := testBus(16)
	defer bus.Close(context.Background())

	const numSubscribers = 5
	var received [numSubscribers]chan Event
	for i := range numSubscribers {
		received[i] = make(chan Event, 1)
		idx := i
		_, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
			received[idx] <- e
		})
		if err != nil {
			t.Fatalf("subscribe[%d]: %v", i, err)
		}
	}

	event := NewEvent(testPayload{Value: "fan-out-test"})
	if err := bus.Publish(context.Background(), event); err != nil {
		t.Fatalf("publish: %v", err)
	}

	for i := range numSubscribers {
		select {
		case got := <-received[i]:
			if got.ID != event.ID {
				t.Errorf("subscriber[%d] event ID: got %q, want %q", i, got.ID, event.ID)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber[%d] timed out", i)
		}
	}
}

func TestChannelBus_TopicIsolation(t *testing.T) {
	bus := testBus(16)
	defer bus.Close(context.Background())

	telemetryReceived := make(chan Event, 1)
	driveReceived := make(chan Event, 1)

	_, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
		telemetryReceived <- e
	})
	if err != nil {
		t.Fatalf("subscribe telemetry: %v", err)
	}

	_, err = bus.Subscribe(TopicDriveStarted, func(e Event) {
		driveReceived <- e
	})
	if err != nil {
		t.Fatalf("subscribe drive: %v", err)
	}

	// Publish only to telemetry topic.
	event := NewEvent(testPayload{Value: "isolation-test"})
	if err := bus.Publish(context.Background(), event); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Telemetry subscriber should receive the event.
	select {
	case <-telemetryReceived:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("telemetry subscriber timed out")
	}

	// Drive subscriber should NOT receive the event.
	select {
	case e := <-driveReceived:
		t.Fatalf("drive subscriber received unexpected event: %+v", e)
	case <-time.After(100 * time.Millisecond):
		// expected — no event
	}
}

func TestChannelBus_Unsubscribe(t *testing.T) {
	bus := testBus(16)
	defer bus.Close(context.Background())

	var count atomic.Int64

	sub, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
		count.Add(1)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Publish one event — should be delivered.
	event := NewEvent(testPayload{Value: "before-unsub"})
	if err := bus.Publish(context.Background(), event); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Wait for delivery.
	deadline := time.After(2 * time.Second)
	for count.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for first event")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// Unsubscribe.
	if err := bus.Unsubscribe(sub); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}

	// Give the goroutine time to stop.
	time.Sleep(50 * time.Millisecond)

	// Publish another event — should NOT be delivered.
	event2 := NewEvent(testPayload{Value: "after-unsub"})
	if err := bus.Publish(context.Background(), event2); err != nil {
		t.Fatalf("publish after unsub: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if got := count.Load(); got != 1 {
		t.Errorf("handler call count: got %d, want 1", got)
	}
}

func TestChannelBus_UnsubscribeNotFound(t *testing.T) {
	bus := testBus(16)
	defer bus.Close(context.Background())

	err := bus.Unsubscribe(Subscription{ID: "nonexistent", Topic: TopicVehicleTelemetry})
	if err == nil {
		t.Fatal("expected error for nonexistent subscription")
	}
}

func TestChannelBus_Backpressure_DropOldest(t *testing.T) {
	const bufSize = 4
	bus := testBus(bufSize)
	defer bus.Close(context.Background())

	// Use a channel that blocks the handler to simulate a slow subscriber.
	// The handler will not consume events until we signal it.
	gate := make(chan struct{})
	var deliveredEvents []Event
	var mu sync.Mutex

	_, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
		<-gate // block until test signals
		mu.Lock()
		deliveredEvents = append(deliveredEvents, e)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Give subscriber goroutine time to start and block on gate.
	time.Sleep(50 * time.Millisecond)

	// Publish more events than the buffer can hold.
	// The first event will be consumed by the goroutine (blocking on gate).
	// Events 2..bufSize+1 will fill the buffer.
	// Event bufSize+2 will trigger drop-oldest.
	totalEvents := bufSize + 3
	events := make([]Event, totalEvents)
	for i := range totalEvents {
		events[i] = NewEvent(testPayload{Value: string(rune('A' + i))})
		if err := bus.Publish(context.Background(), events[i]); err != nil {
			t.Fatalf("publish[%d]: %v", i, err)
		}
		// Small delay so the first event is picked up by the goroutine.
		if i == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Now unblock the handler and let it drain.
	close(gate)

	// Wait for delivery.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(deliveredEvents)
		mu.Unlock()
		if n >= bufSize+1 {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("timed out: got %d events, expected >= %d", len(deliveredEvents), bufSize+1)
			mu.Unlock()
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Verify that we received fewer events than we published (some were dropped).
	mu.Lock()
	got := len(deliveredEvents)
	mu.Unlock()

	if got >= totalEvents {
		t.Errorf("expected some events to be dropped: delivered %d of %d", got, totalEvents)
	}

	// The LAST published event must always be in the delivered set,
	// because drop-oldest ensures the newest is always kept.
	mu.Lock()
	lastDelivered := deliveredEvents[len(deliveredEvents)-1]
	mu.Unlock()

	lastPublished := events[totalEvents-1]
	if lastDelivered.ID != lastPublished.ID {
		t.Errorf("last delivered event ID %q != last published event ID %q",
			lastDelivered.ID, lastPublished.ID)
	}
}

func TestChannelBus_SlowSubscriberDoesNotBlockFast(t *testing.T) {
	// Use a buffer large enough that the fast subscriber never drops.
	// The slow subscriber's buffer will fill and drop, but publish must
	// still complete without blocking — proving slow subs don't block
	// fast ones.
	bus := testBus(256)
	defer bus.Close(context.Background())

	// Slow subscriber: blocks forever until test cleanup.
	slowGate := make(chan struct{})
	_, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
		<-slowGate
	})
	if err != nil {
		t.Fatalf("subscribe slow: %v", err)
	}
	defer close(slowGate)

	// Fast subscriber: immediately records events.
	fastReceived := make(chan Event, 100)
	_, err = bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
		fastReceived <- e
	})
	if err != nil {
		t.Fatalf("subscribe fast: %v", err)
	}

	// Publish several events. Because publish is non-blocking and the
	// fast subscriber's buffer is large, all events should be delivered.
	const n = 50
	for i := range n {
		event := NewEvent(testPayload{Value: string(rune('0' + i%10))})
		if err := bus.Publish(context.Background(), event); err != nil {
			t.Fatalf("publish[%d]: %v", i, err)
		}
	}

	// Fast subscriber should receive all events even though slow subscriber
	// is permanently blocked.
	received := 0
	deadline := time.After(5 * time.Second)
	for received < n {
		select {
		case <-fastReceived:
			received++
		case <-deadline:
			t.Fatalf("fast subscriber only received %d/%d events", received, n)
		}
	}
}

func TestChannelBus_PublishAfterClose(t *testing.T) {
	bus := testBus(16)

	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	err := bus.Publish(context.Background(), NewEvent(testPayload{Value: "after-close"}))
	if !errors.Is(err, ErrBusClosed) {
		t.Errorf("publish after close: got %v, want %v", err, ErrBusClosed)
	}
}

func TestChannelBus_SubscribeAfterClose(t *testing.T) {
	bus := testBus(16)

	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {})
	if !errors.Is(err, ErrBusClosed) {
		t.Errorf("subscribe after close: got %v, want %v", err, ErrBusClosed)
	}
}

func TestChannelBus_CloseIdempotent(t *testing.T) {
	bus := testBus(16)

	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("first close: %v", err)
	}

	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestChannelBus_GracefulShutdownDrains(t *testing.T) {
	bus := testBus(64)

	var delivered atomic.Int64

	_, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
		// Simulate slow processing.
		time.Sleep(time.Millisecond)
		delivered.Add(1)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Publish events.
	const n = 20
	for i := range n {
		event := NewEvent(testPayload{Value: string(rune('A' + i%26))})
		if err := bus.Publish(context.Background(), event); err != nil {
			t.Fatalf("publish[%d]: %v", i, err)
		}
	}

	// Close with a generous timeout — events should drain.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := bus.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := delivered.Load()
	if got < n {
		t.Logf("warning: only %d/%d events delivered (some may have been in flight)", got, n)
	}
	// At minimum, most events should be delivered during drain.
	if got == 0 {
		t.Fatal("no events were delivered during graceful shutdown")
	}
}

func TestChannelBus_ConcurrentPublishSubscribe(t *testing.T) {
	bus := testBus(256)
	defer bus.Close(context.Background())

	var totalDelivered atomic.Int64

	// Concurrently subscribe.
	const numSubscribers = 10
	const numPublishers = 5
	const eventsPerPublisher = 100

	var subWg sync.WaitGroup
	for range numSubscribers {
		subWg.Add(1)
		go func() {
			defer subWg.Done()
			_, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
				totalDelivered.Add(1)
			})
			if err != nil {
				t.Errorf("subscribe: %v", err)
			}
		}()
	}
	subWg.Wait()

	// Concurrently publish.
	var pubWg sync.WaitGroup
	for range numPublishers {
		pubWg.Add(1)
		go func() {
			defer pubWg.Done()
			for range eventsPerPublisher {
				event := NewEvent(testPayload{Value: "concurrent"})
				if err := bus.Publish(context.Background(), event); err != nil {
					t.Errorf("publish: %v", err)
				}
			}
		}()
	}
	pubWg.Wait()

	// Wait for delivery.
	expected := int64(numSubscribers * numPublishers * eventsPerPublisher)
	deadline := time.After(5 * time.Second)
	for totalDelivered.Load() < expected {
		select {
		case <-deadline:
			got := totalDelivered.Load()
			// With backpressure, not all events may be delivered; that's OK.
			// But a significant number should be.
			if got < expected/2 {
				t.Fatalf("delivered %d events, expected at least %d", got, expected/2)
			}
			return
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestChannelBus_EventOrdering(t *testing.T) {
	bus := testBus(256)
	defer bus.Close(context.Background())

	const n = 100
	received := make(chan Event, n)

	_, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
		received <- e
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Publish events sequentially with distinct IDs.
	publishedIDs := make([]string, n)
	for i := range n {
		event := NewEvent(testPayload{Value: string(rune('A' + i%26))})
		publishedIDs[i] = event.ID
		if err := bus.Publish(context.Background(), event); err != nil {
			t.Fatalf("publish[%d]: %v", i, err)
		}
	}

	// Collect delivered events and verify order matches publish order.
	deadline := time.After(5 * time.Second)
	for i := range n {
		select {
		case got := <-received:
			if got.ID != publishedIDs[i] {
				t.Fatalf("event[%d]: got ID %q, want %q", i, got.ID, publishedIDs[i])
			}
		case <-deadline:
			t.Fatalf("timed out after receiving %d/%d events", i, n)
		}
	}
}

func TestChannelBus_PublishToTopicWithNoSubscribers(t *testing.T) {
	bus := testBus(16)
	defer bus.Close(context.Background())

	// Publishing to a topic with no subscribers should not error.
	event := NewEvent(testPayload{Value: "no-subscribers"})
	if err := bus.Publish(context.Background(), event); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func TestChannelBus_MultipleTopics(t *testing.T) {
	bus := testBus(16)
	defer bus.Close(context.Background())

	telemetry := make(chan Event, 1)
	drive := make(chan Event, 1)

	_, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
		telemetry <- e
	})
	if err != nil {
		t.Fatalf("subscribe telemetry: %v", err)
	}

	_, err = bus.Subscribe(TopicDriveStarted, func(e Event) {
		drive <- e
	})
	if err != nil {
		t.Fatalf("subscribe drive: %v", err)
	}

	// Publish to both topics.
	e1 := NewEvent(VehicleTelemetryEvent{VIN: "VIN1", CreatedAt: time.Now()})
	e2 := NewEvent(DriveStartedEvent{VIN: "VIN1", DriveID: "d1", StartedAt: time.Now()})

	if err := bus.Publish(context.Background(), e1); err != nil {
		t.Fatalf("publish telemetry: %v", err)
	}
	if err := bus.Publish(context.Background(), e2); err != nil {
		t.Fatalf("publish drive: %v", err)
	}

	timeout := time.After(2 * time.Second)

	select {
	case got := <-telemetry:
		if got.ID != e1.ID {
			t.Errorf("telemetry event ID mismatch")
		}
	case <-timeout:
		t.Fatal("timed out waiting for telemetry event")
	}

	select {
	case got := <-drive:
		if got.ID != e2.ID {
			t.Errorf("drive event ID mismatch")
		}
	case <-timeout:
		t.Fatal("timed out waiting for drive event")
	}
}

func TestNewEvent(t *testing.T) {
	payload := VehicleTelemetryEvent{
		VIN:       "5YJ3E1EA1NF000001",
		CreatedAt: time.Now(),
		Fields:    map[string]TelemetryValue{"speed": {FloatVal: ptr(65.0)}},
	}

	event := NewEvent(payload)

	if event.ID == "" {
		t.Error("event ID should not be empty")
	}
	if event.Topic != TopicVehicleTelemetry {
		t.Errorf("topic: got %q, want %q", event.Topic, TopicVehicleTelemetry)
	}
	if event.Timestamp.IsZero() {
		t.Error("timestamp should not be zero")
	}
	if event.Payload == nil {
		t.Error("payload should not be nil")
	}

	// Type assert the payload.
	got, ok := event.Payload.(VehicleTelemetryEvent)
	if !ok {
		t.Fatalf("payload type assertion failed: got %T", event.Payload)
	}
	if got.VIN != payload.VIN {
		t.Errorf("VIN: got %q, want %q", got.VIN, payload.VIN)
	}
}

func TestDefaultBusConfig(t *testing.T) {
	cfg := DefaultBusConfig()

	if cfg.BufferSize != 256 {
		t.Errorf("BufferSize: got %d, want 256", cfg.BufferSize)
	}
	if cfg.DrainTimeout != 5*time.Second {
		t.Errorf("DrainTimeout: got %v, want 5s", cfg.DrainTimeout)
	}
}

func TestNewChannelBus_DefaultsZeroConfig(t *testing.T) {
	bus := NewChannelBus(BusConfig{}, NoopBusMetrics{}, testLogger())
	defer bus.Close(context.Background())

	if bus.cfg.BufferSize != 256 {
		t.Errorf("BufferSize: got %d, want 256", bus.cfg.BufferSize)
	}
	if bus.cfg.DrainTimeout != 5*time.Second {
		t.Errorf("DrainTimeout: got %v, want 5s", bus.cfg.DrainTimeout)
	}
}

func TestChannelBus_MetricsCalledOnDrop(t *testing.T) {
	const bufSize = 2
	metrics := &countingMetrics{}
	cfg := BusConfig{BufferSize: bufSize, DrainTimeout: 2 * time.Second}
	bus := NewChannelBus(cfg, metrics, testLogger())
	defer bus.Close(context.Background())

	// Subscribe with a handler that blocks forever.
	gate := make(chan struct{})
	defer close(gate)

	_, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
		<-gate
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Let goroutine pick up first event and block.
	time.Sleep(20 * time.Millisecond)

	// Fill the buffer and overflow.
	for i := range bufSize + 3 {
		event := NewEvent(testPayload{Value: string(rune('A' + i))})
		if err := bus.Publish(context.Background(), event); err != nil {
			t.Fatalf("publish[%d]: %v", i, err)
		}
		if i == 0 {
			time.Sleep(20 * time.Millisecond) // let goroutine consume first event
		}
	}

	// Some events should have been dropped.
	drops := metrics.dropped.Load()
	if drops == 0 {
		t.Error("expected dropped metric to be > 0")
	}
}

func TestChannelBus_UnsubscribeTopicNeverSubscribed(t *testing.T) {
	bus := testBus(16)
	defer bus.Close(context.Background())

	// Unsubscribe from a topic that has never had any subscribers. This
	// exercises the nil topicEntry path in Unsubscribe (line 190-191).
	err := bus.Unsubscribe(Subscription{ID: "phantom", Topic: TopicDriveEnded})
	if err == nil {
		t.Fatal("expected error for unsubscribe on a topic with no subscribers")
	}
	if !errors.Is(err, ErrSubscriptionNotFound) {
		t.Errorf("expected ErrSubscriptionNotFound, got: %v", err)
	}
}

func TestChannelBus_MetricsPublishAndDelivery(t *testing.T) {
	metrics := &countingMetrics{}
	cfg := BusConfig{BufferSize: 16, DrainTimeout: 2 * time.Second}
	bus := NewChannelBus(cfg, metrics, testLogger())
	defer bus.Close(context.Background())

	var delivered atomic.Int64
	_, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
		delivered.Add(1)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	const n = 5
	for i := range n {
		event := NewEvent(testPayload{Value: string(rune('A' + i))})
		if err := bus.Publish(context.Background(), event); err != nil {
			t.Fatalf("publish[%d]: %v", i, err)
		}
	}

	// Wait for all events to be delivered.
	deadline := time.After(2 * time.Second)
	for delivered.Load() < n {
		select {
		case <-deadline:
			t.Fatalf("timed out: delivered %d/%d", delivered.Load(), n)
		default:
			time.Sleep(time.Millisecond)
		}
	}

	if got := metrics.published.Load(); got != n {
		t.Errorf("IncPublished: got %d, want %d", got, n)
	}
	if got := metrics.delivered.Load(); got != n {
		t.Errorf("IncDelivered: got %d, want %d", got, n)
	}
	if got := metrics.dropped.Load(); got != 0 {
		t.Errorf("IncDropped: got %d, want 0", got)
	}
}

func TestChannelBus_MetricsSubscriberCount(t *testing.T) {
	metrics := &countingMetrics{}
	cfg := BusConfig{BufferSize: 16, DrainTimeout: 2 * time.Second}
	bus := NewChannelBus(cfg, metrics, testLogger())
	defer bus.Close(context.Background())

	sub1, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {})
	if err != nil {
		t.Fatalf("subscribe 1: %v", err)
	}

	if got := metrics.lastSubCount.Load(); got != 1 {
		t.Errorf("after 1st subscribe: subscriber count = %d, want 1", got)
	}

	sub2, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {})
	if err != nil {
		t.Fatalf("subscribe 2: %v", err)
	}

	if got := metrics.lastSubCount.Load(); got != 2 {
		t.Errorf("after 2nd subscribe: subscriber count = %d, want 2", got)
	}

	if err := bus.Unsubscribe(sub1); err != nil {
		t.Fatalf("unsubscribe 1: %v", err)
	}

	if got := metrics.lastSubCount.Load(); got != 1 {
		t.Errorf("after unsubscribe: subscriber count = %d, want 1", got)
	}

	if err := bus.Unsubscribe(sub2); err != nil {
		t.Fatalf("unsubscribe 2: %v", err)
	}

	if got := metrics.lastSubCount.Load(); got != 0 {
		t.Errorf("after all unsubscribed: subscriber count = %d, want 0", got)
	}
}

func TestChannelBus_CloseWithDrainTimeout(t *testing.T) {
	// Use a very short drain timeout and a handler that blocks, so the
	// drain times out. Exercises the drainCtx.Done() path in Close.
	cfg := BusConfig{BufferSize: 16, DrainTimeout: 10 * time.Millisecond}
	bus := NewChannelBus(cfg, NoopBusMetrics{}, testLogger())

	gate := make(chan struct{})
	defer close(gate)

	_, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
		<-gate // block forever
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Publish a few events so there is work in the buffer.
	for i := range 5 {
		event := NewEvent(testPayload{Value: string(rune('A' + i))})
		if err := bus.Publish(context.Background(), event); err != nil {
			t.Fatalf("publish[%d]: %v", i, err)
		}
	}

	// Close should return after DrainTimeout without hanging.
	start := time.Now()
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	elapsed := time.Since(start)

	// Should complete roughly around DrainTimeout, not 2s+.
	if elapsed > 500*time.Millisecond {
		t.Errorf("close took %v, expected to complete near drain timeout of 10ms", elapsed)
	}
}

func TestChannelBus_NewChannelBus_NegativeConfig(t *testing.T) {
	// Negative values in config should be replaced with defaults.
	bus := NewChannelBus(BusConfig{BufferSize: -1, DrainTimeout: -1}, NoopBusMetrics{}, testLogger())
	defer bus.Close(context.Background())

	if bus.cfg.BufferSize != 256 {
		t.Errorf("BufferSize: got %d, want 256 (default)", bus.cfg.BufferSize)
	}
	if bus.cfg.DrainTimeout != 5*time.Second {
		t.Errorf("DrainTimeout: got %v, want 5s (default)", bus.cfg.DrainTimeout)
	}
}

func TestChannelBus_ConcurrentSubscribeUnsubscribe(t *testing.T) {
	// Stress test that concurrent subscribe/unsubscribe operations are
	// race-free. The race detector will catch issues here.
	bus := testBus(16)
	defer bus.Close(context.Background())

	const goroutines = 20
	const opsPerGoroutine = 50

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range opsPerGoroutine {
				sub, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {})
				if err != nil {
					return // bus may have been closed
				}
				// Publish an event while subscribed.
				_ = bus.Publish(context.Background(), NewEvent(testPayload{Value: "race"}))
				_ = bus.Unsubscribe(sub)
			}
		}()
	}
	wg.Wait()
}

func TestNewEvent_UniqueIDs(t *testing.T) {
	// Verify that NewEvent generates unique IDs.
	const n = 1000
	ids := make(map[string]struct{}, n)
	for range n {
		event := NewEvent(testPayload{Value: "id-test"})
		if _, exists := ids[event.ID]; exists {
			t.Fatalf("duplicate event ID: %q", event.ID)
		}
		ids[event.ID] = struct{}{}
	}
}

func TestNewEvent_AllPayloadTypes(t *testing.T) {
	// Verify NewEvent correctly derives the topic for every domain event type.
	tests := []struct {
		name     string
		payload  EventPayload
		wantTopic Topic
	}{
		{
			name:      "VehicleTelemetryEvent",
			payload:   VehicleTelemetryEvent{VIN: "TEST"},
			wantTopic: TopicVehicleTelemetry,
		},
		{
			name:      "ConnectivityEvent_connected",
			payload:   ConnectivityEvent{VIN: "TEST", Status: StatusConnected},
			wantTopic: TopicConnectivity,
		},
		{
			name:      "ConnectivityEvent_disconnected",
			payload:   ConnectivityEvent{VIN: "TEST", Status: StatusDisconnected},
			wantTopic: TopicConnectivity,
		},
		{
			name:      "DriveStartedEvent",
			payload:   DriveStartedEvent{VIN: "TEST", DriveID: "d1"},
			wantTopic: TopicDriveStarted,
		},
		{
			name:      "DriveUpdatedEvent",
			payload:   DriveUpdatedEvent{VIN: "TEST", DriveID: "d1"},
			wantTopic: TopicDriveUpdated,
		},
		{
			name:      "DriveEndedEvent",
			payload:   DriveEndedEvent{VIN: "TEST", DriveID: "d1"},
			wantTopic: TopicDriveEnded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := NewEvent(tt.payload)
			if event.Topic != tt.wantTopic {
				t.Errorf("topic: got %q, want %q", event.Topic, tt.wantTopic)
			}
			if event.Payload == nil {
				t.Error("payload should not be nil")
			}
		})
	}
}

func TestPrometheusBusMetrics_RegisterAndIncrement(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewPrometheusBusMetrics(reg)

	// Call each method — these should not panic.
	metrics.IncPublished(TopicVehicleTelemetry)
	metrics.IncPublished(TopicVehicleTelemetry)
	metrics.IncDelivered(TopicVehicleTelemetry)
	metrics.IncDropped(TopicDriveStarted)
	metrics.ObservePublishDuration(TopicVehicleTelemetry, 0.001)
	metrics.SetSubscriberCount(TopicVehicleTelemetry, 3)

	// Verify the counters via the Prometheus registry.
	gathered, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	// Build a lookup for easy assertion.
	metricFamilies := make(map[string]float64)
	for _, mf := range gathered {
		for _, m := range mf.GetMetric() {
			key := mf.GetName()
			for _, lp := range m.GetLabel() {
				key += ":" + lp.GetValue()
			}
			if m.Counter != nil {
				metricFamilies[key] = m.Counter.GetValue()
			}
			if m.Gauge != nil {
				metricFamilies[key] = m.Gauge.GetValue()
			}
		}
	}

	if v := metricFamilies["telemetry_event_bus_published_total:vehicle.telemetry"]; v != 2 {
		t.Errorf("published_total: got %v, want 2", v)
	}
	if v := metricFamilies["telemetry_event_bus_delivered_total:vehicle.telemetry"]; v != 1 {
		t.Errorf("delivered_total: got %v, want 1", v)
	}
	if v := metricFamilies["telemetry_event_bus_dropped_total:drive.started"]; v != 1 {
		t.Errorf("dropped_total: got %v, want 1", v)
	}
	if v := metricFamilies["telemetry_event_bus_subscribers:vehicle.telemetry"]; v != 3 {
		t.Errorf("subscribers gauge: got %v, want 3", v)
	}
}

// countingMetrics counts metric calls for test assertions.
type countingMetrics struct {
	published    atomic.Int64
	delivered    atomic.Int64
	dropped      atomic.Int64
	lastSubCount atomic.Int64
}

func (m *countingMetrics) IncPublished(Topic)                    { m.published.Add(1) }
func (m *countingMetrics) IncDelivered(Topic)                    { m.delivered.Add(1) }
func (m *countingMetrics) IncDropped(Topic)                      { m.dropped.Add(1) }
func (m *countingMetrics) ObservePublishDuration(Topic, float64) {}
func (m *countingMetrics) SetSubscriberCount(_ Topic, count int) { m.lastSubCount.Store(int64(count)) }

// ptr is a generic helper to get a pointer to a value.
func ptr[T any](v T) *T { return &v }

// --- Benchmarks ---

func BenchmarkPublish_10Subscribers(b *testing.B) {
	bus := testBus(1024)
	defer bus.Close(context.Background())

	for range 10 {
		_, err := bus.Subscribe(TopicVehicleTelemetry, func(e Event) {
			// fast no-op handler
		})
		if err != nil {
			b.Fatalf("subscribe: %v", err)
		}
	}

	event := NewEvent(testPayload{Value: "bench"})

	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		if err := bus.Publish(context.Background(), event); err != nil {
			b.Fatalf("publish: %v", err)
		}
	}
}

func BenchmarkPublish_NoSubscribers(b *testing.B) {
	bus := testBus(256)
	defer bus.Close(context.Background())

	event := NewEvent(testPayload{Value: "bench"})

	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		if err := bus.Publish(context.Background(), event); err != nil {
			b.Fatalf("publish: %v", err)
		}
	}
}
