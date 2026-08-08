package push

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/myrobotaxi/telemetry/internal/drain"
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

// VehicleRide is one ride caught by an owner teardown: which ride, and the
// status that decides how its card must be ended.
//
// The status is carried rather than looked up per ride because the read that
// produces this list already joins the ride, and because getting it wrong is
// the specific defect it exists to prevent — see EndForVehicleTeardown.
type VehicleRide struct {
	RideRequestID string
	Status        string
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
	// still have a live Activity registered against them, with each ride's
	// status — which decides how its card is ended (MYR-421).
	RideIDsWithActivitiesForVehicle(ctx context.Context, vehicleID string) ([]VehicleRide, error)
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
	// workers counts the detached sends async has started and not yet retired,
	// plus the caller-goroutine work track covers. See activity_drain.go, and
	// internal/drain for why a sync.WaitGroup cannot hold this job (MYR-398,
	// MYR-410).
	workers drain.Group
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
// A RIDE IN FLIGHT is sent as `cancelled`, because that is what it is from the
// rider's side — the ride is not happening, and no one is coming — with
// DismissPromptly, the same linger as any other unhappy ending.
//
// A RIDE THAT ALREADY COMPLETED IS ENDED AS COMPLETED, and MYR-421 is why that
// distinction had to be drawn. The teardown read's predicate is
// `ended_at IS NULL`, which before the held `end` could never match a completed
// ride: the tombstone went out with the transition. Now the row stays live for
// five minutes, so a rider who finished their trip and whose owner then
// unlinked the car had their completion check overwritten with a CANCELLATION —
// news that is both wrong and worse — and dismissed in thirty seconds.
//
// Skipping completed rides here is NOT the fix, tempting as it looks: the
// cascade is about to delete the rows, so the held-end pass would never fire
// for them and the card would be stranded on its arrival state until
// ActivityKit's ceiling. This is the last chance to end it, so it is ended
// honestly — final `completed` content-state, no alert (the announcement
// already spent rung six), and the same five-minute linger it would have got.
//
// Called synchronously on the caller's context and best-effort throughout: a
// teardown must never fail because a push did.
func (a *ActivityNotifier) EndForVehicleTeardown(ctx context.Context, vehicleID string) {
	if !a.active() {
		return
	}

	rides, err := a.store.RideIDsWithActivitiesForVehicle(ctx, vehicleID)
	if err != nil {
		a.logger.Error("live activity: teardown ride lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		return
	}
	if len(rides) == 0 {
		return
	}

	var completed int
	for _, ride := range rides {
		if ride.Status == statusCompleted {
			completed++
			a.endRide(ctx, ride.RideRequestID, statusCompleted, teardownCompletedEnding)
			continue
		}
		a.endRide(ctx, ride.RideRequestID, "cancelled", promptEnding)
	}

	a.logger.Info("live activities ended for vehicle teardown",
		slog.String("vehicle_id", vehicleID),
		slog.Int("rides", len(rides)),
		// Separated because the two endings say opposite things to the rider,
		// and "the teardown cancelled a ride that had already happened" is the
		// shape of the bug this count exists to make visible.
		slog.Int("completed", completed),
	)
}
