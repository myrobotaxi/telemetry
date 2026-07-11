package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// commandNavigationGPS is the Tesla command name for a lat/long navigation
// push (MYR-180 registry). UNSIGNED — forwarded straight to the Fleet API.
const commandNavigationGPS = "navigation_gps_request"

// pickupNavOrder is the Tesla remote-nav "order" integer for the pickup
// destination. Tesla's convention is 1-based: order=1 REPLACES the current
// trip (making the pickup THE active destination), 2 prepends a stop, 3
// appends a stop. We want the pickup to be the immediate destination, so we
// send 1 — matching internal/commands buildNavigationGPS's default. (Source:
// Tesla Fleet API navigation_gps_request remote-nav order semantics, as
// documented by the Teslemetry python-tesla-fleet-api client:
// https://github.com/Teslemetry/python-tesla-fleet-api — "1 replaces the
// trip, 2 prepends a stop, 3 appends a stop".)
const pickupNavOrder = float64(1)

// Outcome is the resolved dispatch result persisted on the ride row. The
// string values match the go_ride_requests.dispatch_status CHECK enum.
type Outcome string

const (
	OutcomeSent    Outcome = "sent"
	OutcomeFailed  Outcome = "failed"
	OutcomeSkipped Outcome = "skipped"
)

// Non-command failure codes recorded in dispatch_error when the pipeline
// fails BEFORE (or instead of) a Tesla command. Command failures record the
// typed wserrors code (key_not_paired, vehicle_asleep, …) verbatim.
//
// The standalone `// #nosec G101` lines below suppress gosec's hardcoded-
// credential false positive: these are OPAQUE outcome codes surfaced in
// logs + the P0 dispatch_error column, never credentials. gosec matches the
// nosec directive on the line directly above the flagged const spec, so the
// comment MUST stay standalone (a trailing `//nolint:gosec // #nosec` on the
// same line is NOT honored by the standalone gosec scanner).
const (
	codeVehicleUnresolved = "vehicle_unresolved"
	// #nosec G101 -- opaque outcome code, not a credential
	codeTokenUnavailable = "token_unavailable"
	// #nosec G101 -- opaque outcome code, not a credential
	codeTokenExpired          = "token_expired"
	codeTransportUnconfigured = "transport_unconfigured"
	codeCanceled              = "dispatch_canceled"
)

// Permanent (non-retryable) resolution-failure sentinels. The cmd/ adapters
// map the concrete telemetry/store sentinels (ErrTeslaTokenExpired,
// ErrTeslaTokenUnavailable, store.ErrVehicleNotFound) onto these so the
// dispatcher can classify a resolution failure as permanent WITHOUT importing
// those packages (keeping the consumer-site interface boundary clean). Any
// OTHER resolution error is treated as transient and retried under the
// bounded policy.
var (
	// ErrTokenExpired — a token exists but is expired and cannot be
	// refreshed; the owner must re-link. Recorded as token_expired.
	ErrTokenExpired = errors.New("dispatch: tesla token expired")
	// ErrTokenUnavailable — no token on file (account never linked).
	// Recorded as token_unavailable.
	ErrTokenUnavailable = errors.New("dispatch: tesla token unavailable")
	// ErrVehicleNotFound — the vehicle row is gone; no VIN to resolve.
	// Recorded as vehicle_unresolved.
	ErrVehicleNotFound = errors.New("dispatch: vehicle not found")
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

// resolveWithRetry runs fn under the bounded retry policy (MaxRetries extra
// attempts with exponential backoff). It retries only transient errors:
// a permanentResolution error, or a dead ctx, stops the loop immediately.
func (d *Dispatcher) resolveWithRetry(ctx context.Context, fn func(context.Context) (string, error)) (string, error) {
	attempts := d.cfg.MaxRetries + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoffFor(d.cfg.Backoff, attempt)); err != nil {
				return "", err
			}
		}
		v, err := fn(ctx)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if permanentResolution(err) || ctx.Err() != nil {
			return "", err
		}
	}
	return "", lastErr
}

// permanentResolution reports whether a resolution error is a well-identified
// permanent condition that no retry can fix.
func permanentResolution(err error) bool {
	return errors.Is(err, ErrTokenExpired) ||
		errors.Is(err, ErrTokenUnavailable) ||
		errors.Is(err, ErrVehicleNotFound)
}

// tokenErrorCode maps a failed token resolution to its opaque outcome code.
func tokenErrorCode(err error) string {
	switch {
	case isContextErr(err):
		return codeCanceled
	case errors.Is(err, ErrTokenExpired):
		return codeTokenExpired
	case errors.Is(err, ErrTokenUnavailable):
		return codeTokenUnavailable
	default:
		// Transient failure that exhausted the retry budget: the token could
		// not be obtained, so it is effectively unavailable.
		return codeTokenUnavailable
	}
}

// isContextErr reports whether err is (or wraps) a context cancellation or
// deadline — used to record dispatch_canceled rather than a resolution code.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// executeWithRetry runs the navigation_gps_request under the bounded retry
// policy. Retryable codes (transport / asleep-after-wake) back off and retry
// up to MaxRetries; every other code is terminal.
func (d *Dispatcher) executeWithRetry(ctx context.Context, vin, token string, pickup events.RidePlace) (outcome Outcome, errCode *string) {
	req := commands.Request{
		VIN:     vin,
		Command: commandNavigationGPS,
		Params: map[string]any{
			"lat":   pickup.Latitude,
			"lon":   pickup.Longitude,
			"order": pickupNavOrder,
		},
		AccessToken: token,
		Scopes:      commands.ParseScopes(token),
	}

	attempts := d.cfg.MaxRetries + 1
	lastCode := string(wserrors.ErrCodeCommandFailed)
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoffFor(d.cfg.Backoff, attempt)); err != nil {
				c := codeCanceled
				return OutcomeFailed, &c
			}
		}

		_, err := d.executor.Execute(ctx, req)
		if err == nil {
			return OutcomeSent, nil
		}
		var retry bool
		retry, lastCode = classifyCommandErr(err)
		if !retry {
			return OutcomeFailed, &lastCode
		}
	}
	// Retries exhausted on a retryable error.
	return OutcomeFailed, &lastCode
}

// classifyCommandErr decides whether a command error is worth another bounded
// attempt and returns the opaque code to record. Retryability is the
// executor's own signal (CommandError.Retryable) — a transient transport /
// vehicle failure or asleep-after-wake. The unconfigured-transport sentinel is
// a permanent misconfiguration: never retry it, and record a distinct code so
// ops can tell it apart from a live command_failed 502. A non-typed error is
// treated as a transient transport failure.
func classifyCommandErr(err error) (retryable bool, code string) {
	if errors.Is(err, commands.ErrTransportNotConfigured) {
		return false, codeTransportUnconfigured
	}
	var cmdErr *commands.CommandError
	if errors.As(err, &cmdErr) {
		return cmdErr.Retryable, string(cmdErr.Code)
	}
	return true, string(wserrors.ErrCodeCommandFailed)
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

// backoffFor grows the base backoff exponentially per retry attempt
// (attempt is 1-based for the first retry): base, 2·base, 4·base, …
func backoffFor(base time.Duration, attempt int) time.Duration {
	if attempt <= 1 {
		return base
	}
	return base << (attempt - 1)
}

// sleepCtx sleeps for d or returns early if ctx is canceled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("dispatch backoff: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// redactVIN shows only the last 4 chars of a VIN for logs (empty stays empty).
func redactVIN(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return "***" + vin[len(vin)-4:]
}

// discardWriter drops log output for the nil-logger default.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
