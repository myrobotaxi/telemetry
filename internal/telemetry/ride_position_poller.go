package telemetry

// MYR-394 ride-scoped position polling.
//
// THE DEFECT. A vehicle only reports where it is while it is STREAMING. A car
// that is in service, asleep, or simply offline streams nothing, and nothing
// else in the system asks — so the rider-tracking map keeps rendering the last
// fix from before the car went quiet. Observed by the client on a live ride:
// the marker sat 1.5 miles from where the car was actually parked, and the ETA
// was computed from that fiction.
//
// THE FIX, AND ITS BOUNDARIES. While — and only while — a vehicle is carrying
// out a ride, ask Tesla's REST vehicle_data for its position every ~25s and
// feed the answer through the EXISTING MYR-260 backfill pipeline. Four
// boundaries, each deliberate:
//
//   - BOUNDED TO THE RIDE. A poller starts when a ride becomes live and stops
//     on any terminal transition. It is also rebuilt from the database at
//     startup and re-checked periodically, so a poller can never outlive the
//     ride that justified it — not across a restart, and not when the bus drops
//     a terminal event under backpressure (the bus is drop-OLDEST, so this is a
//     real failure mode, not a hypothetical one).
//   - NEVER WAKES A SLEEPING CAR. Structurally, not by policy: this type holds
//     no waker and reaches no code that has one. An asleep car answers 408/503,
//     the cycle is skipped quietly, and the schedule continues — the driver
//     getting in will wake it soon enough.
//   - YIELDS TO THE STREAM. Two independent layers. This loop skips the REST
//     call entirely while the car is streaming (saving the request), and if it
//     ever did fire, the MYR-300 gate inside RefreshFromVehicleData drops every
//     stream-sourceable field anyway. A poll can never out-rank live data, and
//     because noteStreamFrame ignores non-streamed frames it can never stamp
//     the freshness clock and latch itself into looking live.
//   - ONE POLLER PER VEHICLE. The registry is keyed by VEHICLE, so two rides on
//     one car cannot double the request rate; the handle holds the SET of rides
//     justifying it and the poller stops when that set empties. MaxActive
//     bounds the fleet-wide worst case.
//   - BOUNDED IN TIME AS WELL AS IN SCOPE. Nothing in this system automatically
//     terminates a ride, so "the ride is still open" is not on its own a
//     promise that anything will ever stop the loop. MaxLifetime and the
//     matching age bound in pollableRidePredicate are what turn it into one.
//
// It introduces NO new read path, NO new WS message and NO contract change:
// RefreshPositionFromVehicleData publishes the same SourceRESTBackfill frame
// the MYR-260 backfill always has, and latitude / longitude / speed / heading
// are already on the wire, already on the owner mask, and already persisted.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// ridePositionRefresher performs ONE vehicle_data read per call and
// republishes it through the MYR-260 backfill mapping. Satisfied by
// *ServiceStatusMonitor.RefreshPositionFromVehicleData.
//
// It is deliberately NOT vehicleDataPublisher (the connectivity-edge
// interface): that method chases a second /release_notes GET and rewrites the
// Prisma colour column on every call, which is correct on an edge and pure
// waste on a 25-second loop. Naming the narrower method here is what makes
// "one active ride costs one Tesla GET per interval" a property of the type
// rather than of a comment.
type ridePositionRefresher interface {
	RefreshPositionFromVehicleData(ctx context.Context, vin, token string) error
}

// RideVehicleResolver maps a ride's vehicleID onto the VIN the Fleet API is
// keyed by. Declared at the consumer site; satisfied in cmd/ by an adapter over
// store.VehicleRepo.GetByID — the same shape dispatch.VehicleResolver uses.
type RideVehicleResolver interface {
	ResolveVIN(ctx context.Context, vehicleID string) (string, error)
}

// ActiveRideTarget is one vehicle that should currently be polled, together
// with the ride that justifies it.
type ActiveRideTarget struct {
	RideRequestID string
	VehicleID     string
	VIN           string
}

// ActiveRideLister lists the vehicles currently mid-ride. Satisfied in cmd/ by
// an adapter over store.RideRequestRepo.ListActiveRidePollTargets. This is the
// authority the reconcile trusts OVER its own in-memory registry.
type ActiveRideLister interface {
	ListActiveRideTargets(ctx context.Context, limit int) ([]ActiveRideTarget, error)
}

// RidePositionPoller owns the set of per-vehicle position pollers and the
// ride-lifecycle subscriptions that open and close them.
//
// Its two Tesla-facing dependencies are the interfaces the MYR-266
// VehicleRefresher already established for exactly this job — a backfill
// publisher and a stream-recency reader, both satisfied by
// *ServiceStatusMonitor — so this adds a TRIGGER to the existing read path and
// nothing else.
type RidePositionPoller struct {
	bus  events.Bus
	deps RidePollDeps

	cfg    RidePositionPollConfig
	logger *slog.Logger

	// mu guards active, subs and stopped. The registry is keyed by VEHICLE id,
	// which is what makes "one poll stream per car" an invariant rather than a
	// hope: two rides on one vehicle collapse onto one entry.
	mu      sync.Mutex
	active  map[string]*activePoll
	subs    []events.Subscription
	stopped bool

	// ctx is the lifetime context every spawned poller derives from, captured
	// in Start. Storing a context on a struct is normally a smell; here it is
	// the point — pollers are spawned from BUS HANDLERS, which have no context
	// of their own and must not block, so the component's lifetime has to be
	// reachable from inside a handler. cancel is its lever and wg makes Stop a
	// real join rather than a hope.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// now is the clock the MaxLifetime ceiling is measured against. Injectable
	// so the ceiling can be tested in milliseconds rather than hours, matching
	// drives.Detector.setNow.
	now func() time.Time
}

// RidePollDeps bundles the six collaborators so the poller struct stays under
// the "no God struct" bar (CLAUDE.md: more than seven fields wants
// decomposition) and so cmd/ wiring names each one at the call site.
type RidePollDeps struct {
	// Backfill is the MYR-260 read+publish path — the ONLY way this component
	// talks to Tesla, and the reason it cannot wake a car. ONE GET per call.
	Backfill ridePositionRefresher
	// Streams is the MYR-300 per-VIN stream-recency reader the poll yields to.
	Streams streamRecencyReader
	// Vehicles resolves a ride's vehicleID to a VIN.
	Vehicles RideVehicleResolver
	// Owners resolves a VIN to its owner, whose Tesla token authorises the read.
	Owners VehicleOwnerLookup
	// Tokens resolves that owner's current Tesla access token.
	Tokens teslaTokenResolver
	// Rides is the reconcile's view of what SHOULD be polled.
	Rides ActiveRideLister
}

// NewRidePositionPoller builds a poller. Every dependency is required; cmd/
// guards construction so a deployment missing any of them keeps the
// pre-MYR-394 behaviour rather than running a half-wired loop.
func NewRidePositionPoller(
	bus events.Bus,
	deps RidePollDeps,
	cfg RidePositionPollConfig,
	logger *slog.Logger,
) *RidePositionPoller {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &RidePositionPoller{
		bus:    bus,
		deps:   deps,
		cfg:    cfg.withDefaults(),
		logger: logger,
		active: make(map[string]*activePoll),
		now:    time.Now,
	}
}

// NewRidePositionPollerDeps assembles the dependency bundle. Offered as a
// constructor as well as a struct so cmd/ names every collaborator positionally
// and cannot silently leave one nil when a new one is added.
func NewRidePositionPollerDeps(
	backfill ridePositionRefresher,
	streams streamRecencyReader,
	vehicles RideVehicleResolver,
	owners VehicleOwnerLookup,
	tokens teslaTokenResolver,
	rides ActiveRideLister,
) RidePollDeps {
	return RidePollDeps{
		Backfill: backfill,
		Streams:  streams,
		Vehicles: vehicles,
		Owners:   owners,
		Tokens:   tokens,
		Rides:    rides,
	}
}

// Start captures the lifetime context and subscribes to the three ride seams
// that open and close pollers.
//
// It does NOT run the reconcile — cmd/ does that explicitly afterwards under
// its own bounded context, matching how setupNavDispatcher sequences Subscribe,
// then Reconcile, then the sweeper. Subscribing FIRST is deliberate: a ride
// accepted DURING the reconcile pass must not be missed, and starting a poller
// twice is a no-op.
func (p *RidePositionPoller) Start(ctx context.Context) error {
	if !p.cfg.Enabled {
		p.logger.Info("ride position poller disabled (RIDE_POSITION_POLL_ENABLED=false)")
		return nil
	}

	p.mu.Lock()
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.mu.Unlock()

	seams := []struct {
		topic   events.Topic
		handler events.Handler
	}{
		{events.TopicRideAccepted, p.handleAccepted},
		{events.TopicRideDue, p.handleDue},
		{events.TopicRideStatusChanged, p.handleStatusChanged},
	}
	for _, s := range seams {
		sub, err := p.bus.Subscribe(s.topic, s.handler)
		if err != nil {
			p.unsubscribeAll()
			return fmt.Errorf("ride position poller: subscribe %s: %w", s.topic, err)
		}
		p.mu.Lock()
		p.subs = append(p.subs, sub)
		p.mu.Unlock()
	}

	p.logger.Info("ride position poller started",
		slog.Duration("interval", p.cfg.Interval),
		slog.Duration("call_timeout", p.cfg.CallTimeout),
		slog.Duration("reconcile_interval", p.cfg.ReconcileInterval),
		slog.Int("max_active", p.cfg.MaxActive),
	)
	return nil
}

// Stop unsubscribes, cancels every running poller, and WAITS for them.
//
// The order matters, and it is the MYR-176 lesson about dying-context writes:
// the subscriptions come off FIRST so no handler can start a poller into a
// context that is about to be cancelled; then `stopped` is latched so a handler
// already mid-flight refuses; then the pollers are cancelled and joined. Stop
// is idempotent, and safe to call when Start was never run (kill-switch off).
func (p *RidePositionPoller) Stop() {
	p.unsubscribeAll()

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	cancel := p.cancel
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	p.wg.Wait()
	p.logger.Info("ride position poller stopped")
}

func (p *RidePositionPoller) unsubscribeAll() {
	p.mu.Lock()
	subs := p.subs
	p.subs = nil
	p.mu.Unlock()

	for _, sub := range subs {
		if err := p.bus.Unsubscribe(sub); err != nil {
			p.logger.Warn("ride position poller: unsubscribe failed",
				slog.String("error", err.Error()))
		}
	}
}
