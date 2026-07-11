package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
const (
	codeVehicleUnresolved = "vehicle_unresolved"
	codeTokenUnavailable  = "token_unavailable" //nolint:gosec // #nosec G101 -- opaque outcome code, not a credential
	codeCanceled          = "dispatch_canceled"
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
}

const (
	defaultMaxRetries     = 2
	defaultBackoff        = 2 * time.Second
	defaultOverallTimeout = 2 * time.Minute
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
	return &Dispatcher{
		vehicles: vehicles,
		tokens:   tokens,
		executor: executor,
		store:    store,
		cfg:      cfg.withDefaults(),
		logger:   logger,
	}
}

// Subscribe registers the dispatcher on the ride.accepted topic. The bus runs
// the handler in a dedicated per-subscriber goroutine with serial delivery,
// so process needs no internal locking.
func (d *Dispatcher) Subscribe(bus events.Bus) (events.Subscription, error) {
	sub, err := bus.Subscribe(events.TopicRideAccepted, d.handle)
	if err != nil {
		return events.Subscription{}, fmt.Errorf("dispatch.Subscribe: %w", err)
	}
	return sub, nil
}

// handle is the events.Handler: type-asserts, bounds the work with a fresh
// context (the bus handler carries none), and runs the pipeline.
func (d *Dispatcher) handle(evt events.Event) {
	ev, ok := evt.Payload.(events.RideAcceptedEvent)
	if !ok {
		d.logger.Error("dispatch: unexpected payload type on ride.accepted",
			slog.String("event_id", evt.ID),
		)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.OverallTimeout)
	defer cancel()
	d.process(ctx, ev)
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

	vin, err := d.vehicles.ResolveVIN(ctx, ev.VehicleID)
	if err != nil {
		code := codeVehicleUnresolved
		d.record(ctx, ev, "", OutcomeFailed, &code)
		return
	}

	token, err := d.tokens.ResolveToken(ctx, ev.OwnerID)
	if err != nil {
		code := codeTokenUnavailable
		d.record(ctx, ev, vin, OutcomeFailed, &code)
		return
	}

	outcome, code := d.executeWithRetry(ctx, vin, token, ev.Pickup)
	d.record(ctx, ev, vin, outcome, code)
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
		lastCode = codeOf(err)
		if !retryable(lastCode) {
			return OutcomeFailed, &lastCode
		}
	}
	// Retries exhausted on a retryable error.
	return OutcomeFailed, &lastCode
}

// record persists the outcome and emits the single per-attempt audit line.
func (d *Dispatcher) record(ctx context.Context, ev events.RideAcceptedEvent, vin string, outcome Outcome, code *string) {
	if err := d.store.RecordDispatchOutcome(ctx, ev.RideRequestID, outcome, code); err != nil {
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

// codeOf extracts the typed command error code, defaulting to command_failed
// for a non-typed error.
func codeOf(err error) string {
	var cmdErr *commands.CommandError
	if errors.As(err, &cmdErr) {
		return string(cmdErr.Code)
	}
	return string(wserrors.ErrCodeCommandFailed)
}

// retryable reports whether a command error code is worth another bounded
// attempt: transient transport failures and asleep-after-wake exhaustion.
// Everything else (key_not_paired, permission_denied, invalid_request,
// internal_error) is terminal.
func retryable(code string) bool {
	switch code {
	case string(wserrors.ErrCodeVehicleAsleep), string(wserrors.ErrCodeCommandFailed):
		return true
	default:
		return false
	}
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
