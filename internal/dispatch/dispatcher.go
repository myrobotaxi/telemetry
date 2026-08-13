package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/drain"
	"github.com/myrobotaxi/telemetry/internal/events"
)

// Outcome is the resolved dispatch result persisted on the ride row. The
// string values match the go_ride_requests.dispatch_status CHECK enum.
type Outcome string

const (
	OutcomeSent    Outcome = "sent"
	OutcomeFailed  Outcome = "failed"
	OutcomeSkipped Outcome = "skipped"
)

// VehicleResolver resolves a vehicle cuid to its Tesla VIN. Implemented in
// cmd/ over the vehicle repo.
type VehicleResolver interface {
	ResolveVIN(ctx context.Context, vehicleID string) (string, error)
}

// TokenSource resolves a vehicle owner's Tesla OAuth access token (refreshing
// an expired one). Implemented in cmd/ over the account repo + refresher.
type TokenSource interface {
	ResolveToken(ctx context.Context, userID string) (string, error)
}

// CommandExecutor executes one Tesla vehicle command. Satisfied by
// *commands.Executor; a fake substitutes it in tests.
type CommandExecutor interface {
	Execute(ctx context.Context, req commands.Request) (commands.Result, error)
}

// OutcomeStore is the persistence seam: the exactly-once dispatch claim plus
// the resolved-outcome write, for BOTH nav legs. Leg 1 (pickup, on accept)
// uses ClaimDispatch/RecordDispatchOutcome; leg 2 (dropoff, on rider start —
// MYR-270) uses the Dropoff* pair, which write independent columns so neither leg
// clobbers the other's history. Satisfied by the ride-request repo via a cmd/
// adapter (status crosses as the dispatch.Outcome string).
type OutcomeStore interface {
	// ClaimDispatch returns true iff THIS call won the exactly-once leg-1 claim.
	ClaimDispatch(ctx context.Context, rideID string) (bool, error)
	// RecordDispatchOutcome persists the resolved leg-1 status; errCode is nil
	// for sent and for a kill-switch skip, and carries the opaque code for
	// every failure — plus for the one skip that has a reason to name,
	// nav_superseded (MYR-526).
	RecordDispatchOutcome(ctx context.Context, rideID string, status Outcome, errCode *string) error
	// ClaimDropoffDispatch is the leg-2 (dropoff) exactly-once claim.
	ClaimDropoffDispatch(ctx context.Context, rideID string) (bool, error)
	// RecordDropoffDispatchOutcome persists the resolved leg-2 status.
	RecordDropoffDispatchOutcome(ctx context.Context, rideID string, status Outcome, errCode *string) error
}

// Config tunes the dispatcher. Zero values get sane defaults via withDefaults.
type Config struct {
	// Enabled is the kill-switch. False records every accept as `skipped`
	// with no Tesla call (DISPATCH_ENABLED=false).
	Enabled bool
	// MaxRetries is the number of ADDITIONAL command attempts after the first
	// on a retryable error (transport / asleep-after-wake). 0 disables retry.
	MaxRetries int
	// Backoff is the base delay between retries (grows exponentially).
	Backoff time.Duration
	// OverallTimeout bounds the whole per-event dispatch (claim → command →
	// record), independent of the bus handler.
	OverallTimeout time.Duration
	// MaxConcurrent caps how many dispatches run at once. The bus delivers
	// serially per subscriber; the handler hands each event off to a worker
	// and returns immediately, so a slow dispatch (up to OverallTimeout)
	// never blocks delivery of the next event. 0 gets the default.
	MaxConcurrent int
}

const (
	defaultMaxRetries     = 2
	defaultBackoff        = 2 * time.Second
	defaultOverallTimeout = 2 * time.Minute
	defaultMaxConcurrent  = 4
	// recordTimeout bounds the detached outcome-write context. It is short
	// and independent of the per-event ctx so a timeout outcome still
	// persists even though the per-event ctx has already expired.
	recordTimeout = 10 * time.Second
)

func (c Config) withDefaults() Config {
	if c.MaxRetries < 0 {
		c.MaxRetries = defaultMaxRetries
	}
	if c.Backoff <= 0 {
		c.Backoff = defaultBackoff
	}
	if c.OverallTimeout <= 0 {
		c.OverallTimeout = defaultOverallTimeout
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = defaultMaxConcurrent
	}
	return c
}

// Dispatcher subscribes to ride.accepted and pushes the pickup to the
// vehicle's Tesla navigation. Construct with New, then Subscribe on the bus.
type Dispatcher struct {
	vehicles VehicleResolver
	tokens   TokenSource
	executor CommandExecutor
	store    OutcomeStore
	cfg      Config
	logger   *slog.Logger

	sem chan struct{} // concurrency cap for in-flight dispatches
	// nav orders this dispatcher's pushes per vehicle so the two legs of one
	// ride can never land on the car out of order (MYR-526).
	nav *navSequencer
	// verify is the optional nav-apply close-loop (MYR-527). Nil = off.
	verify *navVerify
	// workers counts the in-flight dispatch goroutines. Not a sync.WaitGroup —
	// see Wait, and internal/drain for the argument (MYR-410).
	workers drain.Group
}

// New builds a Dispatcher. logger may be nil (a discard logger is used).
func New(
	vehicles VehicleResolver,
	tokens TokenSource,
	executor CommandExecutor,
	store OutcomeStore,
	cfg Config,
	logger *slog.Logger,
) *Dispatcher {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	cfg = cfg.withDefaults()
	return &Dispatcher{
		vehicles: vehicles,
		tokens:   tokens,
		executor: executor,
		store:    store,
		cfg:      cfg,
		logger:   logger,
		sem:      make(chan struct{}, cfg.MaxConcurrent),
		nav:      newNavSequencer(),
	}
}

// Subscribe registers the dispatcher on the ride.accepted topic. The bus runs
// the handler in a dedicated per-subscriber goroutine with SERIAL delivery.
// handle therefore hands each event off to a bounded worker pool and returns
// immediately, so a slow dispatch (up to OverallTimeout) never blocks the bus
// loop or the delivery of the next event. Each dispatch runs independently
// (no shared mutable state; the store is the concurrency-safe seam), so the
// pool needs no per-event locking.
func (d *Dispatcher) Subscribe(bus events.Bus) (events.Subscription, error) {
	sub, err := bus.Subscribe(events.TopicRideAccepted, d.handle)
	if err != nil {
		return events.Subscription{}, fmt.Errorf("dispatch.Subscribe(accepted): %w", err)
	}
	// Leg 2 (MYR-270, was ride.boarded in MYR-265): the same dispatcher also
	// pushes the DROPOFF when the RIDER starts the ride. Both handlers feed one
	// bounded worker pool and are drained by Wait; the caller relies on bus.Close
	// for unsubscribe on shutdown, so only the leg-1 subscription is returned
	// (both are torn down together).
	if _, err := bus.Subscribe(events.TopicRideStarted, d.handleStarted); err != nil {
		return events.Subscription{}, fmt.Errorf("dispatch.Subscribe(started): %w", err)
	}
	// MYR-541: an edited endpoint that is the current leg's target re-shares
	// (and re-verifies, MYR-527) through the same pool.
	if _, err := bus.Subscribe(events.TopicRideTripChanged, d.handleTripChanged); err != nil {
		return events.Subscription{}, fmt.Errorf("dispatch.Subscribe(trip_changed): %w", err)
	}
	return sub, nil
}

// Wait blocks until all in-flight dispatches finish. Call AFTER bus.Close on
// shutdown, never before; also used by tests.
//
// It is NOT backed by a sync.WaitGroup, and cannot be (MYR-410). Wait runs on
// whichever goroutine is shutting down, while dispatchAsync's counting runs on
// the bus's delivery goroutine, and nothing orders the two — so a ride.accepted
// the bus has already accepted can still be on its way into dispatchAsync when
// Wait looks. A WaitGroup read in that window is silently empty: the drain ends
// early and a pickup that was mid-flight never reaches the car. internal/drain
// counts under the mutex the waiter blocks on, which has no such window. Same
// bug, same fix as the Live Activity notifier in MYR-398.
//
// The ordering above is load-bearing, not advisory. This Wait covers dispatches
// that have STARTED; an event still in this subscriber's buffered channel has
// not reached handle, so nothing is counted and Wait returns over it. bus.Close
// is what makes that backlog started work. The pair is the guarantee.
//
// NOTE: cmd/telemetry-server does not call this yet — setupNavDispatcher never
// returns the dispatcher. Wiring it needs a BOUNDED wait, because one dispatch
// may run for OverallTimeout (2 minutes) against a 10s deploy grace period.
// That is MYR-431.
func (d *Dispatcher) Wait() { d.workers.Wait() }

// handle is the events.Handler: it type-asserts and hands the event to a
// worker goroutine, returning immediately so the bus's serial per-subscriber
// loop is never blocked by a slow dispatch. Concurrency is capped by the sem
// semaphore (acquired inside the worker, so handle does not block); the
// per-event OverallTimeout still bounds each dispatch.
func (d *Dispatcher) handle(evt events.Event) {
	ev, ok := evt.Payload.(events.RideAcceptedEvent)
	if !ok {
		d.logger.Error("dispatch: unexpected payload type on ride.accepted",
			slog.String("event_id", evt.ID),
		)
		return
	}
	d.dispatchAsync(func(ctx context.Context) { d.process(ctx, ev) })
}

// handleStarted is the events.Handler for ride.started (leg 2, MYR-270): it
// type-asserts the RideStartedEvent and hands the dropoff push to the same
// bounded worker pool as leg 1, returning immediately so the bus loop is never
// blocked.
func (d *Dispatcher) handleStarted(evt events.Event) {
	ev, ok := evt.Payload.(events.RideStartedEvent)
	if !ok {
		d.logger.Error("dispatch: unexpected payload type on ride.started",
			slog.String("event_id", evt.ID),
		)
		return
	}
	d.dispatchAsync(func(ctx context.Context) { d.processDropoff(ctx, ev) })
}

// dispatchAsync runs fn on a bounded worker goroutine under a fresh
// OverallTimeout-bounded context, shared by both legs. Returns immediately so
// the bus's serial per-subscriber loop is never blocked by a slow dispatch.
//
// The dispatch is counted HERE, on the bus's delivery goroutine, and not inside
// the worker: counting after the go statement would reopen the window Wait
// exists to close.
func (d *Dispatcher) dispatchAsync(fn func(context.Context)) {
	done := d.workers.Track()
	go func() {
		defer done()
		// Acquire a worker slot (bounds concurrency to MaxConcurrent). This
		// runs off the bus loop, so delivery has already returned.
		d.sem <- struct{}{}
		defer func() { <-d.sem }()

		ctx, cancel := context.WithTimeout(context.Background(), d.cfg.OverallTimeout)
		defer cancel()
		fn(ctx)
	}()
}

// resolveVIN resolves the vehicle's VIN under the bounded retry policy,
// retrying transient failures. A permanent condition (ErrVehicleNotFound)
// short-circuits. On failure it returns a non-nil error code; on success a
// nil code and the VIN.
func (d *Dispatcher) resolveVIN(ctx context.Context, vehicleID string) (vin string, errCode *string) {
	vin, err := d.resolveWithRetry(ctx, func(c context.Context) (string, error) {
		return d.vehicles.ResolveVIN(c, vehicleID)
	})
	if err != nil {
		code := codeVehicleUnresolved
		if isContextErr(err) {
			code = codeCanceled
		}
		return "", &code
	}
	return vin, nil
}

// resolveToken resolves the owner's Tesla access token under the bounded
// retry policy. Permanent conditions short-circuit with a DISTINCT code:
// token_expired (must re-link) vs token_unavailable (never linked). A
// transient failure that exhausts the budget records token_unavailable (the
// token could not be obtained); ctx cancellation records dispatch_canceled.
func (d *Dispatcher) resolveToken(ctx context.Context, ownerID string) (token string, errCode *string) {
	token, err := d.resolveWithRetry(ctx, func(c context.Context) (string, error) {
		return d.tokens.ResolveToken(c, ownerID)
	})
	if err != nil {
		code := tokenErrorCode(err)
		return "", &code
	}
	return token, nil
}
