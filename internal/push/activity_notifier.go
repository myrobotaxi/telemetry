package push

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// The Live Activity lifecycle consumer (MYR-172).
//
// It subscribes to the SAME ride.status.changed topic the alert notifier does,
// and that is deliberate rather than convenient: every lifecycle transition in
// the service already funnels through RideRequestHandler.mutateStatus and
// publishes there, so subscribing gets all of them — accept, decline, pickup,
// start, dropoff, cancel, and the account-deletion sweep — without a single
// call site being edited. A transition that grows a new handler tomorrow is
// covered on the day it publishes.
//
// The one lifecycle end this topic does NOT carry is a reservation that ran
// past its lateness ceiling: the sweeper resolves that into the dispatch
// columns without changing the ride's status. EndForReservationExpiry is the
// seam for it.

// Activity is one running Live Activity, the consumer-site view of a
// go_live_activities row.
type Activity struct {
	RideRequestID string
	UserID        string
	// Token is the ActivityKit update token. P1 — never log in full.
	Token   string
	Sandbox bool
}

// RideContext is everything a content-state needs that is not the token.
type RideContext struct {
	// Status is the ride's lifecycle status.
	Status string
	// VehicleName is the car's nickname, "" when it has none.
	VehicleName string
	// Destination is the dropoff's short label. P1.
	Destination string
	// ETAMinutes is the car's carried navigation ETA in whole minutes, nil when
	// the car has no active route.
	ETAMinutes *int
}

// ActivityStore is the send path's view of the registry.
type ActivityStore interface {
	// ActivitiesForRide lists the still-live Activities on a ride.
	ActivitiesForRide(ctx context.Context, rideRequestID string) ([]Activity, error)
	// EndActivitiesForRide tombstones every live Activity on a ride.
	EndActivitiesForRide(ctx context.Context, rideRequestID string) (int64, error)
	// DeleteActivityToken drops a token APNs has permanently rejected.
	DeleteActivityToken(ctx context.Context, token string) error
	// RideContextFor reads the content-state inputs for one ride.
	RideContextFor(ctx context.Context, rideRequestID string) (RideContext, error)
}

// terminalStatuses maps a terminal ride status onto how long its Activity
// lingers on the lock screen before iOS dismisses it (MYR-194 decision 5).
//
// `completed` lingers because the arrival state is the thing the rider most
// wants a moment with. The unhappy endings go promptly — but not instantly, so
// the news is legible before it disappears.
//
// `arrived` and `enroute` are NOT here: `arrived` is the car reaching the
// pickup (the ride is just beginning) and `enroute` is leg two. Both are the
// most active moments of the ride, and reading either as terminal would end the
// Activity exactly when the rider is looking at it.
var terminalStatuses = map[string]time.Duration{
	"completed": DismissAfter,
	"declined":  DismissPromptly,
	"cancelled": DismissPromptly,
}

// ActivityNotifier pushes ActivityKit updates on ride lifecycle transitions.
type ActivityNotifier struct {
	sender ActivitySender
	store  ActivityStore
	prefs  PrefStore
	cfg    Config
	logger *slog.Logger
	// now is the injectable clock. Timestamps and stale-dates are the whole
	// contract of this surface, so tests pin them rather than tolerate them.
	now func() time.Time

	sem  chan struct{}
	wg   sync.WaitGroup
	mu   sync.Mutex
	subs []events.Subscription
	bus  events.Bus
}

// NewActivityNotifier builds the Live Activity consumer.
//
// sender may be nil — the keyless mode the service runs in before the APNs
// secrets are set, where every send is logged as skipped. prefs may be nil,
// meaning every category is on; that is the same fail-open direction the alert
// notifier takes, and for the same reason.
func NewActivityNotifier(
	sender ActivitySender,
	store ActivityStore,
	prefs PrefStore,
	cfg Config,
	logger *slog.Logger,
) *ActivityNotifier {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	cfg = cfg.withDefaults()
	return &ActivityNotifier{
		sender: sender,
		store:  store,
		prefs:  prefs,
		cfg:    cfg,
		logger: logger,
		now:    time.Now,
		sem:    make(chan struct{}, cfg.MaxConcurrent),
	}
}

// active reports whether a send would actually reach Apple.
func (a *ActivityNotifier) active() bool {
	return a.cfg.Enabled && a.sender != nil && a.store != nil
}

// Subscribe registers the notifier on the ride-status topic.
func (a *ActivityNotifier) Subscribe(bus events.Bus) error {
	a.mu.Lock()
	a.bus = bus
	a.mu.Unlock()

	sub, err := bus.Subscribe(events.TopicRideStatusChanged, a.handleStatusChanged)
	if err != nil {
		return fmt.Errorf("push.ActivityNotifier.Subscribe(topic=%s): %w", events.TopicRideStatusChanged, err)
	}
	a.mu.Lock()
	a.subs = append(a.subs, sub)
	a.mu.Unlock()

	a.logger.Info("live activity notifier subscribed",
		slog.Bool("push_enabled", a.cfg.Enabled),
		slog.Bool("apns_configured", a.sender != nil),
	)
	return nil
}

// Unsubscribe removes every registration. Safe to call twice.
func (a *ActivityNotifier) Unsubscribe() {
	a.mu.Lock()
	subs, bus := a.subs, a.bus
	a.subs = nil
	a.mu.Unlock()

	if bus == nil {
		return
	}
	for _, sub := range subs {
		if err := bus.Unsubscribe(sub); err != nil {
			a.logger.Warn("live activity: unsubscribe failed",
				slog.String("subscription_id", sub.ID),
				slog.String("error", err.Error()),
			)
		}
	}
}

// Wait blocks until every in-flight update finishes.
func (a *ActivityNotifier) Wait() { a.wg.Wait() }

// handleStatusChanged pushes a new content-state for every transition.
//
// Unlike the alert notifier, NOTHING is filtered out here. An alert for a
// rider's own cancellation would be noise, but a Live Activity that keeps
// showing "on its way" after the rider cancelled is a wrong lock screen — the
// surface is a state display, so every state change is worth a push.
func (a *ActivityNotifier) handleStatusChanged(evt events.Event) {
	ev, ok := evt.Payload.(events.RideStatusChangedEvent)
	if !ok {
		a.logger.Error("live activity: unexpected payload",
			slog.String("topic", string(evt.Topic)),
		)
		return
	}

	a.async(func(ctx context.Context) {
		a.pushRide(ctx, ev.RideRequestID, string(evt.Topic))
	})
}

// EndForReservationExpiry ends a ride's Activities when the reservation
// sweeper gives up on a late reservation.
//
// This exists because that path is invisible on the event bus: the sweeper
// records dispatch_status='failed' / 'reservation_expired' and leaves the ride
// at `accepted`, so a rider whose scheduled car never came would otherwise
// watch an Activity promise a pickup forever. Called synchronously by the
// sweeper on a context it owns.
func (a *ActivityNotifier) EndForReservationExpiry(ctx context.Context, rideRequestID string) {
	a.endRide(ctx, rideRequestID, "reservation_expired", DismissPromptly)
}

// async runs fn on a bounded worker with its own timeout, detached from the
// bus so a slow APNs round-trip never backs up event delivery.
func (a *ActivityNotifier) async(fn func(context.Context)) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.sem <- struct{}{}
		defer func() { <-a.sem }()

		ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Timeout)
		defer cancel()
		fn(ctx)
	}()
}
