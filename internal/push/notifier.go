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

// RequesterNamer resolves a user id to a FIRST NAME for interpolation into
// owner-facing copy (MYR-535's "time to head out" push). Resolved at delivery
// time, like the vehicle nickname, so dispatch-side events stay summary-only
// — RideDueEvent deliberately carries ids and instants, no PII. An empty name
// (or an error) is not fatal — the copy falls back to an anonymous title.
type RequesterNamer interface {
	RequesterFirstName(ctx context.Context, userID string) (string, error)
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
	stores   notifierStores
	vehicles VehicleNamer
	// requesters resolves a rider's first name for the owner's due push
	// (MYR-535). Nil is the ordinary unwired state — the copy falls back to
	// its anonymous title — so it rides an optional wither rather than the
	// constructor.
	requesters RequesterNamer
	cfg        Config
	logger     *slog.Logger

	sem  chan struct{}
	mu   sync.Mutex
	subs []events.Subscription
	bus  events.Bus
	// workers counts the detached fan-outs async has started and not yet
	// retired. Not a sync.WaitGroup — see Wait, and internal/drain for the
	// argument (MYR-410).
	workers drain.Group
}

// NewNotifier builds a Notifier. sender may be nil — that is the KEYLESS mode
// the service runs in before the APNs secrets are set, where every send is
// logged as skipped and nothing is delivered. logger may be nil.
//
// prefs (MYR-349) may also be nil, and nil means EVERY CATEGORY IS ON — the
// pre-MYR-349 behaviour. That is the same direction the gate fails in when a
// lookup errors, and it is the only safe default: a notifier that silenced
// itself because its preference store was unwired would leave riders standing
// on sidewalks with no signal anywhere that anything was wrong.
//
// activities (MYR-413) may be nil too, and nil means NO BANNER IS EVER
// SUPPRESSED — the pre-MYR-413 behaviour, and the same fail-open direction for
// the same reason. See notifier_activity_gate.go.
func NewNotifier(
	sender Sender,
	devices DeviceStore,
	prefs PrefStore,
	activities ActivityPresenceStore,
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
		stores:   notifierStores{devices: devices, prefs: prefs, activities: activities},
		vehicles: vehicles,
		cfg:      cfg,
		logger:   logger,
		sem:      make(chan struct{}, cfg.MaxConcurrent),
	}
}

// WithRequesterNames wires the rider-first-name resolver for the owner's due
// push (MYR-535). Optional, following the sweeper's WithActivityEnder
// precedent: nil (never calling this) is the ordinary unwired state and the
// copy falls back to its anonymous title.
func (n *Notifier) WithRequesterNames(r RequesterNamer) *Notifier {
	n.requesters = r
	return n
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
		{events.TopicRideNavUnapplied, n.handleNavUnapplied},
		{events.TopicRideTripChanged, n.handleTripChanged},
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

// Wait blocks until every in-flight fan-out finishes. Shutdown calls it so a
// deploy landing mid-ride still delivers the push it already accepted; tests
// use it to make delivery deterministic.
//
// It is NOT backed by a sync.WaitGroup, and cannot be (MYR-410). Wait runs on
// the goroutine shutting the process down, while async's counting runs on the
// bus's delivery goroutine, and nothing orders the two — so an event Publish
// has already accepted can still be on its way into async when Wait looks. A
// WaitGroup read in that window is silently empty: Wait returns, the process
// exits, and the rider never gets the notification. internal/drain counts under
// the mutex the waiter blocks on, which has no such window. The identical bug
// was found and fixed on the Live Activity notifier first (MYR-398); this site
// and the nav dispatcher were the two copies it left behind.
//
// This Wait is HALF of the shutdown guarantee, not the whole of it. It covers
// fan-outs that have STARTED. An event still sitting in this subscriber's
// buffered channel has not reached handleCreated at all, so there is nothing to
// count and Wait returns over it — the dropped push, by a second route. The bus
// Close is what runs that backlog through the handler; only after it has can
// this Wait mean "every accepted event was delivered". cmd/telemetry-server
// orders the two, and its shutdown-order comment block is the authority.
func (n *Notifier) Wait() { n.workers.Wait() }

// WaitContext is Wait with a deadline, returning how many fan-outs were still
// in flight when it gave up (0 on a clean drain).
//
// Shutdown uses this rather than Wait. A fan-out runs on a fresh Background
// context bounded only by cfg.Timeout, which SIGTERM cannot shorten, and
// bus.Close hands this drain the whole buffered backlog at once — so an
// unbounded wait here can outlive the platform's kill timeout and be SIGKILLed
// mid-push. Abandoning the tail at a deadline loses the same pushes but says
// so.
func (n *Notifier) WaitContext(ctx context.Context) (int, error) {
	inFlight, err := n.workers.WaitContext(ctx)
	if err != nil {
		return inFlight, fmt.Errorf("push.Notifier.WaitContext (%d fan-out(s) abandoned): %w", inFlight, err)
	}
	return 0, nil
}

// handleCreated notifies the vehicle OWNER that somebody wants a ride.
func (n *Notifier) handleCreated(evt events.Event) {
	ev, ok := evt.Payload.(events.RideRequestCreatedEvent)
	if !ok {
		n.logUnexpectedPayload(evt)
		return
	}
	a := createdAlert(ev)
	n.async(func(ctx context.Context) {
		n.fanOut(ctx, delivery{
			userID:   ev.OwnerID,
			rideID:   ev.RideRequestID,
			topic:    string(evt.Topic),
			category: CategoryRideLifecycle,
		}, a)
	})
}

// handleStatusChanged notifies the two parties to a ride about the transitions
// each of them cares about. Transitions that are the recipient's own doing send
// them nothing.
//
// The two audiences are resolved independently, because they answer to
// different copy functions: the rider hears about `accepted`, `declined` and
// `arrived` (statusAlert), the owner hears about `enroute` (ownerStatusAlert,
// MYR-462). Those sets are DISJOINT, so at most one branch below runs on any
// given event and no transition wakes both phones — which is also why the two
// fan-outs can sit sequentially in one worker without sharing a timeout budget
// in practice. They are still written as two independent branches rather than
// an if/else, so that a future transition either party cares about is a copy
// change here and not a restructuring.
func (n *Notifier) handleStatusChanged(evt events.Event) {
	ev, ok := evt.Payload.(events.RideStatusChangedEvent)
	if !ok {
		n.logUnexpectedPayload(evt)
		return
	}
	// Cheap checks before spending a worker slot: most transitions are silent
	// for both parties. Which transitions speak does not depend on scheduling
	// or on either name, so the probes can pass empty values.
	// MYR-541: a TRIP-EDIT publish re-carries the ride's unchanged status for
	// the WS frame's sake; re-firing the status copy for it would tell a rider
	// "Your ride is confirmed" every time somebody moved the drop-off. The
	// trip-changed seam carries this event's own copy.
	if ev.TripEdit {
		return
	}
	scheduled := ev.ScheduledFor != nil
	byOwnerCancel := ownerCancelled(ev.Status, ev.CancelledBy)
	byRiderCancel := riderCancelled(ev.Status, ev.CancelledBy, ev.PreviousStatus)
	_, notifyRider := statusAlert(ev.Status, "", scheduled, byOwnerCancel)
	_, notifyOwner := ownerStatusAlert(ev.Status, nil, byRiderCancel)

	// An owner riding their own car is both parties, and this platform makes
	// that the COMMON case, not an edge one. "You started the ride" delivered
	// to the person whose thumb just left the button is pure noise, so the
	// owner-side push is suppressed whenever the two ids are the same person.
	// The rider-side sends are unaffected: they report the OWNER's actions
	// (accept, decline, arrive), which a self-rider still performs in the
	// other role and may well be looking away from.
	if ev.OwnerID == ev.RiderID {
		notifyOwner = false
	}
	if !notifyRider && !notifyOwner {
		return
	}

	n.async(func(ctx context.Context) {
		if notifyRider {
			a, _ := statusAlert(ev.Status, n.vehicleName(ctx, ev.VehicleID), scheduled, byOwnerCancel)
			n.fanOut(ctx, delivery{
				userID:   ev.RiderID,
				rideID:   ev.RideRequestID,
				topic:    string(evt.Topic),
				category: CategoryRideLifecycle,
				// MYR-413 — the ONLY site that sets this. The rider is the
				// party whose island expands, and a ride status is the only
				// input the ladder is driven by; see notifier_activity_gate.go
				// for why the owner's created push and the reservation's due
				// push are not gated even though one of their statuses is on
				// the ladder.
				islandAlerts: carriesIslandAlert(ev.Status),
			}, a)
		}
		if notifyOwner {
			a, _ := ownerStatusAlert(ev.Status, ev.RequesterName, byRiderCancel)
			n.fanOut(ctx, delivery{
				userID:   ev.OwnerID,
				rideID:   ev.RideRequestID,
				topic:    string(evt.Topic),
				category: CategoryRideLifecycle,
				// islandAlerts stays FALSE, and not by oversight: the MYR-413
				// gate suppresses a banner only when the recipient's own Live
				// Activity is about to announce the same news, and the
				// registration is keyed (ride, user) with only the rider
				// registered today. An owner has no card to defer to, so
				// marking this suppressible could only ever delete the
				// notification rather than defer to a better one.
			}, a)
		}
	})
}

// handleDue notifies BOTH parties that a reserved ride just dispatched: the
// RIDER that their car is moving, and — MYR-535 — the OWNER that the route
// landed on their dash and it is time to head out. The dispatch itself fires
// EARLY by the computed lead, so both land when the owner still has time to
// make the pickup instant.
//
// A SELF-RIDE (owner riding their own car, MYR-325) gets the rider push
// alone, exactly as before this issue: both alerts would land on one phone
// announcing one fact, and the rider one is the surface their Live Activity
// and tracking sheet already continue from.
func (n *Notifier) handleDue(evt events.Event) {
	ev, ok := evt.Payload.(events.RideDueEvent)
	if !ok {
		n.logUnexpectedPayload(evt)
		return
	}
	n.async(func(ctx context.Context) {
		n.fanOut(ctx, delivery{
			userID:   ev.RiderID,
			rideID:   ev.RideRequestID,
			topic:    string(evt.Topic),
			category: CategoryRideLifecycle,
		}, dueAlert(n.vehicleName(ctx, ev.VehicleID)))
		if ev.OwnerID != ev.RiderID {
			n.fanOut(ctx, delivery{
				userID:   ev.OwnerID,
				rideID:   ev.RideRequestID,
				topic:    string(evt.Topic),
				category: CategoryRideLifecycle,
				// islandAlerts stays false for the owner, the standing
				// MYR-413 reasoning: only the rider holds a Live Activity,
				// so an owner banner has no card to defer to.
			}, ownerDueAlert(n.requesterFirstName(ctx, ev.RiderID)))
		}
	})
}

// handleNavUnapplied tells the OWNER their car may not have the route
// (MYR-527). The rider deliberately hears nothing: the alert's action —
// touch the dash — is only the owner's to take, and the rider's surface is
// already showing the honest wrong numbers the fix would correct.
func (n *Notifier) handleNavUnapplied(evt events.Event) {
	ev, ok := evt.Payload.(events.RideNavUnappliedEvent)
	if !ok {
		n.logUnexpectedPayload(evt)
		return
	}
	n.async(func(ctx context.Context) {
		n.fanOut(ctx, delivery{
			userID:   ev.OwnerID,
			rideID:   ev.RideRequestID,
			topic:    string(evt.Topic),
			category: CategoryRideLifecycle,
		}, navUnappliedAlert(n.vehicleName(ctx, ev.VehicleID)))
	})
}

// handleTripChanged tells the OTHER participants a trip edit landed
// (MYR-541). The editor hears nothing (their own thumb); everyone else on the
// ride does — today the other principal, and every group member when MYR-540's
// membership lands. Copy forks on WHO edited, and per the payload policy it
// names the edited PART, never the place.
func (n *Notifier) handleTripChanged(evt events.Event) {
	ev, ok := evt.Payload.(events.RideTripChangedEvent)
	if !ok {
		n.logUnexpectedPayload(evt)
		return
	}
	n.async(func(ctx context.Context) {
		part := tripEditedPart(ev.NewPickup != nil, ev.NewDropoff != nil, ev.StopsChanged)
		if ev.EditorUserID != ev.RiderID && ev.RiderID != ev.OwnerID {
			// The owner edited: tell the rider.
			n.fanOut(ctx, delivery{
				userID:   ev.RiderID,
				rideID:   ev.RideRequestID,
				topic:    string(evt.Topic),
				category: CategoryRideLifecycle,
			}, riderTripChangedAlert(part))
		}
		if ev.EditorUserID != ev.OwnerID && ev.RiderID != ev.OwnerID {
			// The rider edited: tell the owner.
			n.fanOut(ctx, delivery{
				userID:   ev.OwnerID,
				rideID:   ev.RideRequestID,
				topic:    string(evt.Topic),
				category: CategoryRideLifecycle,
			}, ownerTripChangedAlert(ev.RequesterName, part))
		}
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
//
// The fan-out is counted HERE, on the bus's delivery goroutine, and not inside
// the worker: counting after the go statement would reopen the window Wait
// exists to close.
func (n *Notifier) async(fn func(context.Context)) {
	done := n.workers.Track()
	go func() {
		defer done()
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
