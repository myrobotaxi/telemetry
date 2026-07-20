package commands

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Default retry policy. Tesla guidance: a command against an asleep vehicle
// returns "vehicle unavailable"; the client wakes the vehicle and retries
// while it boots (typically a few seconds, occasionally longer). We wake +
// retry a bounded number of times with a fixed backoff, then surface the
// transient vehicle_asleep code so the SDK backs off. A signing session
// counter/anti-replay error means the cached session is stale; the proxy
// re-handshakes on the next call, so one retry suffices.
const (
	defaultWakeMaxAttempts = 3
	defaultWakeBackoff     = 2 * time.Second
	defaultCounterRetryMax = 1
)

// Config tunes the Executor retry policy.
type Config struct {
	WakeMaxAttempts int
	WakeBackoff     time.Duration
	CounterRetryMax int
}

func (c Config) withDefaults() Config {
	if c.WakeMaxAttempts <= 0 {
		c.WakeMaxAttempts = defaultWakeMaxAttempts
	}
	if c.WakeBackoff <= 0 {
		c.WakeBackoff = defaultWakeBackoff
	}
	if c.CounterRetryMax < 0 {
		c.CounterRetryMax = defaultCounterRetryMax
	}
	return c
}

// Executor validates, scope-gates, and dispatches vehicle commands through
// a Transport, applying the wake+retry and session-refresh policies.
type Executor struct {
	registry  *Registry
	transport Transport
	cfg       Config
	logger    *slog.Logger
}

// Option configures an Executor.
type Option func(*Executor)

// WithConfig overrides the default retry policy.
func WithConfig(cfg Config) Option { return func(e *Executor) { e.cfg = cfg.withDefaults() } }

// WithRegistry overrides the default command registry (tests).
func WithRegistry(r *Registry) Option { return func(e *Executor) { e.registry = r } }

// NewExecutor builds an Executor over the given transport. The logger may
// be nil (a discard logger is substituted).
func NewExecutor(transport Transport, logger *slog.Logger, opts ...Option) *Executor {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	e := &Executor{
		registry:  NewRegistry(),
		transport: transport,
		cfg:       Config{}.withDefaults(),
		logger:    logger,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Request is a single command invocation. AccessToken is the vehicle
// owner's Tesla OAuth access token; Scopes is its parsed scope set (may be
// not-Known, in which case scope enforcement is left to Tesla).
type Request struct {
	VIN         string
	Command     string
	Params      map[string]any
	AccessToken string
	Scopes      ScopeSet
}

// Result is the outcome of a successful command.
type Result struct {
	Command string
	Applied bool
}

// Registry exposes the executor's command registry (for the handler to
// validate names and for docs generation).
func (e *Executor) Registry() *Registry { return e.registry }

// Execute runs one command end to end. On failure it returns a
// *CommandError carrying the typed code + HTTP status.
func (e *Executor) Execute(ctx context.Context, req Request) (Result, error) {
	cmd, ok := e.registry.Lookup(req.Command)
	if !ok {
		return Result{}, errUnknownCommand(req.Command)
	}

	// Scope gating: if we know the granted scopes and the required one is
	// absent, deny WITHOUT dialing Tesla (rest-api.md §7.9). When scopes are
	// unknown we defer to Tesla's own enforcement.
	if req.Scopes.Known() && !req.Scopes.Has(cmd.Scope) {
		return Result{}, errPermissionDenied("Tesla token lacks the " + string(cmd.Scope) + " scope for this command")
	}

	// Degrade gracefully when the signing transport is not configured.
	if !e.transport.Enabled() {
		if cmd.SignerRequired {
			return Result{}, errKeyNotPaired("vehicle command signing is not configured (no proxy)")
		}
		return Result{}, errTransportNotConfigured()
	}

	var body []byte
	if cmd.BuildBody != nil {
		b, err := cmd.BuildBody(req.Params)
		if err != nil {
			return Result{}, err // already a *CommandError (invalid_request)
		}
		body = b
	}

	return e.invoke(ctx, cmd, req, body)
}

// invoke runs the transport call under the wake+retry and counter-refresh
// policies. The loop always makes progress: OK and every terminal outcome
// return, and the transient outcomes (asleep, counter) each bound their own
// retry counter, so it cannot spin.
func (e *Executor) invoke(ctx context.Context, cmd Command, req Request, body []byte) (Result, error) {
	tReq := TransportRequest{VIN: req.VIN, Command: cmd.Name, Token: req.AccessToken, Body: body}

	wakeAttempts := 0
	counterRetries := 0
	for {
		res, err := e.transport.Command(ctx, tReq)
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, errInternal("command canceled")
			}
			return Result{}, errCommandFailed("vehicle command transport error")
		}

		switch res.Outcome {
		case OutcomeOK:
			return Result{Command: cmd.Name, Applied: true}, nil

		case OutcomeAsleep:
			if wakeAttempts >= e.cfg.WakeMaxAttempts {
				return Result{}, withDetail(errVehicleAsleep(), res.Reason)
			}
			wakeAttempts++
			if wErr := e.transport.Wake(ctx, req.VIN, req.AccessToken); wErr != nil {
				e.logger.Warn("wake request failed; will retry command anyway",
					slog.Int("attempt", wakeAttempts),
					slog.String("command", cmd.Name),
				)
			}
			if sErr := sleepCtx(ctx, e.cfg.WakeBackoff); sErr != nil {
				return Result{}, errInternal("command canceled during wake backoff")
			}

		case OutcomeCounterError:
			if counterRetries >= e.cfg.CounterRetryMax {
				return Result{}, withDetail(errCommandFailed("signing session error after re-handshake"), res.Reason)
			}
			counterRetries++
			e.logger.Info("retrying command after session counter error",
				slog.String("command", cmd.Name),
			)

		case OutcomeNotPaired:
			return Result{}, withDetail(errKeyNotPaired("virtual key not paired with vehicle"), res.Reason)

		case OutcomePermissionDenied:
			return Result{}, withDetail(errPermissionDenied("Tesla rejected command: insufficient access"), res.Reason)

		case OutcomeInvalidRequest:
			return Result{}, withDetail(errInvalidParams("Tesla rejected command parameters"), res.Reason)

		default: // OutcomeFailed
			return Result{}, withDetail(errCommandFailed("vehicle command failed"), res.Reason)
		}
	}
}

// withDetail attaches the opaque transport-side reason to a *CommandError so
// it can be surfaced in the dispatch outcome log without altering the wire
// code. A nil error or empty detail is a no-op.
func withDetail(e *CommandError, detail string) *CommandError {
	if e != nil && detail != "" {
		e.Detail = detail
	}
	return e
}

// sleepCtx sleeps for d or returns early if ctx is canceled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wake backoff: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// discard is an io.Writer that drops everything, for the nil-logger default.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
