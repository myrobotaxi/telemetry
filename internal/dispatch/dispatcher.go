package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
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
// the resolved-outcome write. Satisfied by the ride-request repo via a cmd/
// adapter (status crosses as the dispatch.Outcome string).
type OutcomeStore interface {
	// ClaimDispatch returns true iff THIS call won the exactly-once claim.
	ClaimDispatch(ctx context.Context, rideID string) (bool, error)
	// RecordDispatchOutcome persists the resolved status; errCode is nil for
	// sent/skipped and the opaque failure code for failed.
	RecordDispatchOutcome(ctx context.Context, rideID string, status Outcome, errCode *string) error
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

	sem chan struct{}  // concurrency cap for in-flight dispatches
	wg  sync.WaitGroup // tracks in-flight dispatch goroutines
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
		return events.Subscription{}, fmt.Errorf("dispatch.Subscribe: %w", err)
	}
	return sub, nil
}

// Wait blocks until all in-flight dispatches finish. Call after unsubscribing
// (bus.Close) on shutdown to drain cleanly; also used by tests.
func (d *Dispatcher) Wait() { d.wg.Wait() }

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

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		// Acquire a worker slot (bounds concurrency to MaxConcurrent). This
		// runs off the bus loop, so delivery has already returned.
		d.sem <- struct{}{}
		defer func() { <-d.sem }()

		ctx, cancel := context.WithTimeout(context.Background(), d.cfg.OverallTimeout)
		defer cancel()
		d.process(ctx, ev)
	}()
}

// process runs claim → (kill-switch | resolve → command) → record for one
// accepted ride. It is safe to call directly in tests.
func (d *Dispatcher) process(ctx context.Context, ev events.RideAcceptedEvent) {
	claimed, err := d.store.ClaimDispatch(ctx, ev.RideRequestID)
	if err != nil {
		// Could not claim safely — do not push nav (we cannot guarantee
		// exactly-once). Log and drop; the ride stays un-dispatched.
		d.logger.Error("dispatch: claim failed",
			slog.String("ride_id", ev.RideRequestID),
			slog.String("vehicle_id", ev.VehicleID),
			slog.String("error", err.Error()),
		)
		return
	}
	if !claimed {
		// Already dispatched by a prior delivery — exactly-once guard.
		d.logger.Debug("dispatch: ride already dispatched, skipping",
			slog.String("ride_id", ev.RideRequestID),
		)
		return
	}

	if !d.cfg.Enabled {
		d.record(ctx, ev, "", OutcomeSkipped, nil)
		return
	}

	vin, code := d.resolveVIN(ctx, ev.VehicleID)
	if code != nil {
		d.record(ctx, ev, "", OutcomeFailed, code)
		return
	}

	token, code := d.resolveToken(ctx, ev.OwnerID)
	if code != nil {
		d.record(ctx, ev, vin, OutcomeFailed, code)
		return
	}

	outcome, ecode := d.executeWithRetry(ctx, vin, token, ev.Pickup)
	d.record(ctx, ev, vin, outcome, ecode)
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

// record persists the outcome and emits the single per-attempt audit line.
// The write runs on a context DETACHED from the per-event ctx (which bounds
// the whole dispatch and may already be canceled/timed-out — precisely when
// we most need to persist the outcome). Without WithoutCancel a timed-out
// ride would stay claimed (dispatched_at set) with a NULL dispatch_status
// forever; the startup reconciler would then have to clean it up. We keep the
// ctx values but drop its deadline, adding our own short bound.
func (d *Dispatcher) record(ctx context.Context, ev events.RideAcceptedEvent, vin string, outcome Outcome, code *string) {
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := d.store.RecordDispatchOutcome(recCtx, ev.RideRequestID, outcome, code); err != nil {
		d.logger.Error("dispatch: failed to record outcome",
			slog.String("ride_id", ev.RideRequestID),
			slog.String("outcome", string(outcome)),
			slog.String("error", err.Error()),
		)
	}

	attrs := []any{
		slog.String("ride_id", ev.RideRequestID),
		slog.String("vehicle_id", ev.VehicleID),
		slog.String("vin", redactVIN(vin)),
		slog.String("outcome", string(outcome)),
	}
	if code != nil {
		attrs = append(attrs, slog.String("error_code", *code))
	}
	d.logger.Info("dispatch attempt", attrs...)
}
