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
	// Progress is this Activity's leg-progress anchor as last DELIVERED to the
	// phone (MYR-398). Per-Activity rather than per-ride because the
	// monotonicity rule it enforces is a promise made to one client about the
	// sequence of values that client has seen.
	Progress ProgressAnchor
	// AlertedPhase is the highest phase this Activity's island has already been
	// expanded for (MYR-398, migration 0029). Per-Activity for the same reason
	// Progress is: it records what one client has been shown, and the "at most
	// once" promise is made to that client. See activity_alert.go.
	AlertedPhase AlertPhase
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
	// TripMilesRemaining is the car's carried remaining distance on that route,
	// in miles (Tesla's tripDistanceRemaining), nil when it reports none. The
	// preferred progress input — see activity_progress.go.
	TripMilesRemaining *float64
	// NavUpdatedAt is when the car's ROW was last written — an upper bound on
	// the age of the two readings above, not a stamp on them. Nil for a car we
	// have never heard from. Used ONLY to gate progress, and only as the cheap
	// half of that gate (see navFresh / readingStalled); `eta` keeps its
	// existing ungated behaviour.
	NavUpdatedAt *time.Time
	// DispatchUnderway is MYR-376's reservation-dormancy predicate as evaluated
	// by the store: false while a reservation sleeps between accept and the
	// earlier of its dispatch and its due instant. It gates the PICKUP leg's
	// track — see legUnderway for what happens without it.
	DispatchUnderway bool
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
	// RideIDsWithActivitiesForVehicle lists the rides of one vehicle that
	// still have a live Activity registered against them.
	RideIDsWithActivitiesForVehicle(ctx context.Context, vehicleID string) ([]string, error)
	// SaveProgress records the leg-progress anchor one Activity was just shown.
	// Called ONLY after APNs accepted the push (MYR-398).
	SaveProgress(ctx context.Context, key ActivityKey, anchor ProgressAnchor) error
	// SaveAlertedPhase raises one Activity's island-expand high-water mark.
	// Called ONLY after APNs accepted an alerting push (MYR-398).
	SaveAlertedPhase(ctx context.Context, key ActivityKey, phase AlertPhase) error
}

// terminalStatuses maps a terminal ride status onto how its Activity ends —
// how long the card lingers before iOS dismisses it (MYR-194 decision 5), and
// since MYR-421 whether the `end` push waits.
//
// `completed` lingers because the arrival state is the thing the rider most
// wants a moment with, and it is the one ending whose `end` is HELD for that
// linger: the push does not merely end the Activity, it takes the Dynamic
// Island presence with it. The unhappy endings go promptly and immediately —
// but not instantly, so the news is legible before it disappears.
//
// `arrived` and `enroute` are NOT here: `arrived` is the car reaching the
// pickup (the ride is just beginning) and `enroute` is leg two. Both are the
// most active moments of the ride, and reading either as terminal would end the
// Activity exactly when the rider is looking at it.
var terminalStatuses = map[string]activityEnding{
	"completed": completedEnding,
	"declined":  promptEnding,
	"cancelled": promptEnding,
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
	mu   sync.Mutex
	subs []events.Subscription
	bus  events.Bus
	// inflight counts the detached workers async has started and not yet
	// retired; idle broadcasts when the last one leaves. Both are guarded by
	// mu — see activity_drain.go for why a sync.WaitGroup cannot hold this
	// job (MYR-398).
	inflight int
	idle     *sync.Cond
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
	a := &ActivityNotifier{
		sender: sender,
		store:  store,
		prefs:  prefs,
		cfg:    cfg,
		logger: logger,
		now:    time.Now,
		sem:    make(chan struct{}, cfg.MaxConcurrent),
	}
	a.idle = sync.NewCond(&a.mu)
	return a
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
	a.endRide(ctx, rideRequestID, "reservation_expired", promptEnding)
}

// EndForVehicleTeardown ends every live Activity on a vehicle's rides, and is
// called BEFORE the owner teardown deletes them.
//
// The order is the entire point. Teardown issues a bare
// `DELETE FROM go_ride_requests WHERE vehicle_id = $1` and migration 0025's FK
// cascades go_live_activities away with it. That delete publishes nothing, so
// the lifecycle subscription never hears about it, and afterwards there is no
// row left to tell anyone from: the token is gone, the ride is gone, and the
// rider's lock screen is left frozen on "your car is on its way" until
// ActivityKit's own multi-hour ceiling reaps it. A cascade cannot strand a
// DATABASE ROW, which is what the migration header claimed; it strands the
// thing the row was pointing at.
//
// Sent as `cancelled` because that is what it is from the rider's side — the
// ride is not happening, and no one is coming — and with DismissPromptly, the
// same linger as any other unhappy ending.
//
// Called synchronously on the caller's context and best-effort throughout: a
// teardown must never fail because a push did.
func (a *ActivityNotifier) EndForVehicleTeardown(ctx context.Context, vehicleID string) {
	if !a.active() {
		return
	}

	rideIDs, err := a.store.RideIDsWithActivitiesForVehicle(ctx, vehicleID)
	if err != nil {
		a.logger.Error("live activity: teardown ride lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		return
	}
	if len(rideIDs) == 0 {
		return
	}

	for _, rideID := range rideIDs {
		a.endRide(ctx, rideID, "cancelled", promptEnding)
	}

	a.logger.Info("live activities ended for vehicle teardown",
		slog.String("vehicle_id", vehicleID),
		slog.Int("rides", len(rideIDs)),
	)
}
