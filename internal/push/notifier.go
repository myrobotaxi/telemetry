package push

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// Device is one registered installation, as the notifier needs it. Declared
// here (consumer site) so internal/push never imports internal/store; the
// cmd/ wiring adapts the store row onto this shape.
type Device struct {
	// Token is the APNs device token. P1 — never log in full.
	Token string
	// Sandbox selects the APNs sandbox gateway.
	Sandbox bool
}

// DeviceStore is the registry surface the notifier needs: resolve a ride
// party to their phones, and drop a token APNs has permanently rejected.
type DeviceStore interface {
	DevicesForUser(ctx context.Context, userID string) ([]Device, error)
	DeleteDeviceToken(ctx context.Context, deviceToken string) error
}

// VehicleNamer resolves a vehicle cuid to its owner-chosen nickname. An empty
// name (or an error) is not fatal — the copy falls back to a generic label.
type VehicleNamer interface {
	VehicleName(ctx context.Context, vehicleID string) (string, error)
}

// Config tunes the notifier. Zero values get defaults via withDefaults.
type Config struct {
	// Enabled is the kill-switch (PUSH_ENABLED). False sends nothing and logs
	// each would-be notification as skipped.
	Enabled bool
	// MaxConcurrent caps in-flight fan-outs. The bus delivers serially per
	// subscriber, so the handler hands each event to a worker and returns
	// immediately; without that, one slow APNs round-trip would stall the
	// ride's own WS broadcast behind it.
	MaxConcurrent int
	// Timeout bounds one event's entire fan-out (lookup + every send).
	Timeout time.Duration
}

const (
	defaultMaxConcurrent = 4
	defaultTimeout       = 30 * time.Second
	// deleteTimeout bounds the detached registry delete that follows an APNs
	// 410. Short and independent of the fan-out context, which may already
	// have expired by the time Apple's verdict arrives.
	deleteTimeout = 10 * time.Second
)

func (c Config) withDefaults() Config {
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = defaultMaxConcurrent
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	return c
}

// Notifier turns ride-lifecycle events into APNs notifications.
//
// It is deliberately FIRE-AND-FORGET in both directions: nothing it does can
// fail a ride, and nothing about a ride waits on it. Every handler hands the
// event to a bounded worker and returns to the bus immediately; every send
// failure is logged and dropped.
//
// AT-MOST-ONCE IS NOT GUARANTEED (v1, documented). The bus makes no
// exactly-once promise, and `ride.status.changed` is published on every
// lifecycle mutation — including a reschedule sub-state change, which
// re-publishes the ride's UNCHANGED main status. So an accepted ride that is
// later rescheduled can produce a second "Your ride is confirmed". This is
// accepted for v1: a duplicate notification is a minor annoyance to a human,
// whereas a missed one is a rider standing on a sidewalk. `ride.due` has no
// such exposure — its publisher holds a one-winner latch for the ride's whole
// lifetime.
type Notifier struct {
	sender   Sender
	devices  DeviceStore
	vehicles VehicleNamer
	cfg      Config
	logger   *slog.Logger

	sem  chan struct{}
	wg   sync.WaitGroup
	mu   sync.Mutex
	subs []events.Subscription
	bus  events.Bus
}

// NewNotifier builds a Notifier. sender may be nil — that is the KEYLESS mode
// the service runs in before the APNs secrets are set, where every send is
// logged as skipped and nothing is delivered. logger may be nil.
func NewNotifier(
	sender Sender,
	devices DeviceStore,
	vehicles VehicleNamer,
	cfg Config,
	logger *slog.Logger,
) *Notifier {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	cfg = cfg.withDefaults()
	return &Notifier{
		sender:   sender,
		devices:  devices,
		vehicles: vehicles,
		cfg:      cfg,
		logger:   logger,
		sem:      make(chan struct{}, cfg.MaxConcurrent),
	}
}

// active reports whether a send would actually reach Apple.
func (n *Notifier) active() bool { return n.cfg.Enabled && n.sender != nil }

// Subscribe registers the notifier on the three ride-lifecycle topics. On a
// partial failure it unsubscribes whatever it already registered, so a failed
// Subscribe leaves no half-wired consumer behind.
func (n *Notifier) Subscribe(bus events.Bus) error {
	n.mu.Lock()
	n.bus = bus
	n.mu.Unlock()

	registrations := []struct {
		topic   events.Topic
		handler events.Handler
	}{
		{events.TopicRideRequestCreated, n.handleCreated},
		{events.TopicRideStatusChanged, n.handleStatusChanged},
		{events.TopicRideDue, n.handleDue},
	}

	for _, reg := range registrations {
		sub, err := bus.Subscribe(reg.topic, reg.handler)
		if err != nil {
			n.Unsubscribe()
			return fmt.Errorf("push.Subscribe(topic=%s): %w", reg.topic, err)
		}
		n.mu.Lock()
		n.subs = append(n.subs, sub)
		n.mu.Unlock()
	}

	n.logger.Info("push notifier subscribed",
		slog.Bool("push_enabled", n.cfg.Enabled),
		slog.Bool("apns_configured", n.sender != nil),
		slog.Int("topics", len(registrations)),
	)
	return nil
}

// Unsubscribe removes every registration. Safe to call twice.
func (n *Notifier) Unsubscribe() {
	n.mu.Lock()
	subs, bus := n.subs, n.bus
	n.subs = nil
	n.mu.Unlock()

	if bus == nil {
		return
	}
	for _, sub := range subs {
		if err := bus.Unsubscribe(sub); err != nil {
			n.logger.Warn("push: unsubscribe failed",
				slog.String("subscription_id", sub.ID),
				slog.String("error", err.Error()),
			)
		}
	}
}

// Wait blocks until every in-flight fan-out finishes. Call after the bus is
// closed on shutdown; tests use it to make delivery deterministic.
func (n *Notifier) Wait() { n.wg.Wait() }

// handleCreated notifies the vehicle OWNER that somebody wants a ride.
func (n *Notifier) handleCreated(evt events.Event) {
	ev, ok := evt.Payload.(events.RideRequestCreatedEvent)
	if !ok {
		n.logUnexpectedPayload(evt)
		return
	}
	a := createdAlert(ev)
	n.async(func(ctx context.Context) {
		n.fanOut(ctx, ev.OwnerID, ev.RideRequestID, string(evt.Topic), a)
	})
}

// handleStatusChanged notifies the RIDER about the transitions they care
// about. Transitions that are the rider's own doing send nothing.
func (n *Notifier) handleStatusChanged(evt events.Event) {
	ev, ok := evt.Payload.(events.RideStatusChangedEvent)
	if !ok {
		n.logUnexpectedPayload(evt)
		return
	}
	// Cheap check before spending a worker slot: most transitions are silent.
	if _, notify := statusAlert(ev.Status, ""); !notify {
		return
	}
	n.async(func(ctx context.Context) {
		a, _ := statusAlert(ev.Status, n.vehicleName(ctx, ev.VehicleID))
		n.fanOut(ctx, ev.RiderID, ev.RideRequestID, string(evt.Topic), a)
	})
}

// handleDue notifies the RIDER that their reserved car is moving.
func (n *Notifier) handleDue(evt events.Event) {
	ev, ok := evt.Payload.(events.RideDueEvent)
	if !ok {
		n.logUnexpectedPayload(evt)
		return
	}
	n.async(func(ctx context.Context) {
		n.fanOut(ctx, ev.RiderID, ev.RideRequestID, string(evt.Topic), dueAlert(n.vehicleName(ctx, ev.VehicleID)))
	})
}

func (n *Notifier) logUnexpectedPayload(evt events.Event) {
	n.logger.Error("push: unexpected payload type",
		slog.String("topic", string(evt.Topic)),
		slog.String("event_id", evt.ID),
	)
}

// async runs fn on a bounded worker under a fresh timeout, returning
// immediately so the bus's serial per-subscriber loop is never blocked.
//
// The goroutine is spawned before the semaphore is acquired, which looks
// unbounded but is not: the bus delivers SERIALLY per subscriber, so at most
// one handler per topic can be in flight here at a time, and each spawned
// goroutine either runs or parks on sem — it never fans out further. The cap
// therefore bounds concurrent APNs traffic, not goroutine count, which is the
// resource actually worth limiting.
func (n *Notifier) async(fn func(context.Context)) {
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.sem <- struct{}{}
		defer func() { <-n.sem }()

		ctx, cancel := context.WithTimeout(context.Background(), n.cfg.Timeout)
		defer cancel()
		fn(ctx)
	}()
}

// discardWriter drops log output for the nil-logger default.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
