// Package fleetsuspend stops paying for telemetry nobody is reading.
//
// ── WHAT THIS COSTS AND WHY THE PACKAGE EXISTS ──────────────────────────────
//
// Tesla bills Fleet Telemetry streaming PER VEHICLE. A car whose owner stopped
// opening the app streams frames to a receiver nobody reads from, every day,
// for as long as the config lives (its `exp` is 350 days). The client's
// cost-control ruling, implemented here and documented at rest-api.md §7.27:
//
//   - FOUR days without any authenticated activity from the vehicle's OWNER →
//     one warning push, once per episode.
//   - FIVE days → remove the vehicle's fleet-telemetry config at Tesla.
//
// ── STRICTLY THE OWNER'S ACTIVITY (EXPLICIT CLIENT RULING) ──────────────────
//
// A RIDER'S OR VIEWER'S USE OF A SHARED CAR DOES NOT DEFER SUSPENSION. This is
// the client's decision, not an implementation convenience, and it is stated
// here at the top because it is the one rule a later reader is most likely to
// "fix". A shared car whose owner has been away five days is suspended even
// while a rider is watching it. The platform is paying for the stream on the
// OWNER's behalf and only the owner can decide to keep paying; the rider's
// honest rendering is the ordinary no-live-telemetry state, which the contract
// spells out (vehicle-summary.schema.json, telemetrySuspendedAt).
//
// The enforcement is the candidate query's INNER JOIN on `"Vehicle"."userId"`
// and nothing else (internal/store/telemetry_suspension.go). There is no rider
// probe here to disable.
//
// ── WHAT SUSPENSION TOUCHES: ONLY THE CONFIG ────────────────────────────────
//
// One Tesla call, `DELETE /api/1/vehicles/{vin}/fleet_telemetry_config` — the
// same call the §7.12 full teardown makes, reused rather than re-implemented.
// The OAuth grant, the virtual key, the `Vehicle` row, every share and every
// ride survive untouched. That is what makes the owner's re-connect (§7.28) a
// single call with no OAuth and no lock/unlock dance, and it is why this
// package holds no teardown, no token clear and no revoke.
//
// ── A RIDE IN PROGRESS IS NOT A SPECIAL CASE (THE RULING) ───────────────────
//
// An owner five days silent whose car is mid-ride is close to impossible and
// not quite: a shared rider can start one. The client's ruling is NOT to
// special-case it, and this package obeys — there is no ride probe. What was
// verified is that the ride does not BREAK. Suspension deletes a config at
// Tesla and writes one Go-owned row; it publishes no ride event, cancels
// nothing, ends no Live Activity and touches no ride table. The car simply
// stops sending frames, which every ride surface already tolerates as the
// ordinary offline case (the drive detector idles, the Live Activity keeps its
// last known state, the arrival flash does not fire). The ride loses live
// frames and nothing else. If that ever needs revisiting, revisit it as a
// product decision here rather than as a defensive probe.
//
// ── SHAPE ───────────────────────────────────────────────────────────────────
//
// The loop follows internal/dispatch's ReservationSweeper: a kill-switched
// Run(ctx) over a ticker, one SweepOnce per tick that returns a counter struct,
// per-item work re-bounded onto a detached context so a SIGTERM cannot
// guillotine a Tesla call mid-flight, and hold-on-error semantics everywhere —
// an unreadable state does nothing and lets the next tick re-decide. SweepOnce
// is EXPORTED, following internal/retention, so tests drive a pass
// deterministically instead of racing a timer.
package fleetsuspend

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// Candidate is one vehicle the sweeper may act on: configured, not yet
// suspended, and belonging to an owner who has been silent since the warning
// threshold. Declared here (consumer site) so this package never imports
// internal/store; cmd/ adapts the store row onto this shape.
type Candidate struct {
	VehicleID string
	// VIN addresses the Tesla config delete. Redact in logs.
	VIN string
	// OwnerID is the warning push's only recipient.
	OwnerID string
	// VehicleName is the owner's nickname for the car; may be empty.
	VehicleName string
	// OwnerLastSeenAt is the stamped instant, up to the stamper's throttle
	// window stale and always in the conservative direction.
	OwnerLastSeenAt time.Time
	// WarnedAt is nil until this episode's warning has been handed off.
	WarnedAt *time.Time
}

// Store is the persistence surface the sweeper needs.
type Store interface {
	// ClearReturnedOwnerEpisodes deletes OPEN episodes whose owner has come
	// back. Run first each pass, so a returned owner is never re-warned.
	ClearReturnedOwnerEpisodes(ctx context.Context, warnBefore time.Time) (int64, error)
	// ListInactiveOwnerVehicles returns candidates, oldest silence first.
	ListInactiveOwnerVehicles(ctx context.Context, warnBefore time.Time, limit int) ([]Candidate, error)
	// MarkWarned records that this episode's warning went out.
	MarkWarned(ctx context.Context, vehicleID string, at time.Time) error
	// MarkSuspended stamps the instant the config was removed. Called only
	// AFTER the Tesla delete has returned without error.
	MarkSuspended(ctx context.Context, vehicleID string, at time.Time) error
}

// ConfigRemover removes a vehicle's fleet-telemetry config at Tesla. Satisfied
// by *telemetry.FleetAPIClient.DeleteTelemetryConfig — the SAME method the
// §7.12 teardown uses, deliberately, so there is one delete in the codebase and
// not two that drift.
type ConfigRemover interface {
	DeleteTelemetryConfig(ctx context.Context, token, vin string) error
}

// TokenSource resolves an owner's Tesla access token, refreshing if needed.
// Satisfied by an adapter over *telemetry.TeslaTokenResolver.
type TokenSource interface {
	AccessToken(ctx context.Context, userID string) (string, error)
}

// Publisher is the fire-and-forget half of the event bus, and the only half
// this package needs. Narrowed at the consumer site rather than taking
// events.Bus outright: the sweeper publishes and never subscribes, so accepting
// Subscribe/Close would advertise a capability it does not use and would make
// every test double implement two methods it never calls. *events.ChannelBus
// satisfies it structurally.
type Publisher interface {
	Publish(ctx context.Context, e events.Event) error
}

// Config tunes the sweeper. Zero values get defaults via withDefaults.
type Config struct {
	// Enabled is the kill-switch. False makes Run return immediately — the
	// state to deploy in if suspension ever has to be stopped in a hurry,
	// because it stops NEW suspensions without un-suspending anything.
	Enabled bool
	// Interval between passes.
	Interval time.Duration
	// WarnAfter is the owner silence that earns a warning push.
	WarnAfter time.Duration
	// SuspendAfter is the owner silence that removes the config. MUST exceed
	// WarnAfter; withDefaults enforces it rather than trusting the caller,
	// because an inverted pair would suspend without ever warning.
	SuspendAfter time.Duration
	// MaxPerSweep caps the candidates examined in one pass.
	MaxPerSweep int
	// PassTimeout bounds the candidate read.
	PassTimeout time.Duration
	// VehicleTimeout bounds one vehicle's Tesla call plus its writes.
	VehicleTimeout time.Duration
	// Keepalive tunes the MYR-594 arm that stops a suspended owner's Tesla
	// refresh token lapsing. Nested so the policy above stays one group.
	Keepalive KeepaliveConfig
}

const (
	defaultInterval       = time.Hour
	defaultWarnAfter      = 4 * 24 * time.Hour
	defaultSuspendAfter   = 5 * 24 * time.Hour
	defaultMaxPerSweep    = 200
	defaultPassTimeout    = 30 * time.Second
	defaultVehicleTimeout = 30 * time.Second
)

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.WarnAfter <= 0 {
		c.WarnAfter = defaultWarnAfter
	}
	if c.SuspendAfter <= 0 {
		c.SuspendAfter = defaultSuspendAfter
	}
	// An inverted or equal pair would mean a car is suspended on the same pass
	// it is first warned, or before — the owner would lose telemetry with no
	// notice at all, which is the one outcome the warning exists to prevent.
	// Correct it rather than trusting a caller's arithmetic.
	if c.SuspendAfter <= c.WarnAfter {
		c.SuspendAfter = c.WarnAfter + 24*time.Hour
	}
	if c.MaxPerSweep <= 0 {
		c.MaxPerSweep = defaultMaxPerSweep
	}
	if c.PassTimeout <= 0 {
		c.PassTimeout = defaultPassTimeout
	}
	if c.VehicleTimeout <= 0 {
		c.VehicleTimeout = defaultVehicleTimeout
	}
	c.Keepalive = c.Keepalive.withDefaults()
	return c
}

// Sweeper warns and then suspends the vehicles of inactive owners, and keeps
// the suspended ones' Tesla grants from lapsing while they are away.
type Sweeper struct {
	store   Store
	configs ConfigRemover
	tokens  TokenSource
	bus     Publisher
	keep    *keepaliveArm // nil disables the MYR-594 keepalive arm entirely
	cfg     Config
	logger  *slog.Logger
	now     func() time.Time
}

// SweeperOption configures optional collaborators. An option rather than a
// constructor parameter because every existing caller predates the keepalive
// arm and none of them should have to say "no" to it.
type SweeperOption func(*Sweeper)

// NewSweeper builds a sweeper. A nil logger discards.
//
// EVERY COLLABORATOR MAY BE NIL, and each nil has a different, deliberate
// meaning rather than being an error:
//
//   - store nil → nothing can be read, so Run refuses to start.
//   - configs or tokens nil → the tesla-http-proxy is not configured (the
//     ordinary state in tests and CI). The sweeper still WARNS but never
//     suspends: there is no way to remove a config, and stamping a suspension
//     we did not perform would tell every consumer a streaming car is
//     disconnected.
//   - bus nil → warnings are decided and recorded but nothing is delivered.
//   - no WithKeepalive option → the MYR-594 arm does not exist and no pass
//     touches a Tesla grant, so suspended owners' refresh tokens age exactly as
//     they did before that issue.
func NewSweeper(
	st Store,
	configs ConfigRemover,
	tokens TokenSource,
	bus Publisher,
	cfg Config,
	logger *slog.Logger,
	opts ...SweeperOption,
) *Sweeper {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	s := &Sweeper{
		store:   st,
		configs: configs,
		tokens:  tokens,
		bus:     bus,
		cfg:     cfg.withDefaults(),
		logger:  logger,
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// withClock injects a clock for deterministic threshold tests.
func (s *Sweeper) withClock(now func() time.Time) *Sweeper {
	if now != nil {
		s.now = now
	}
	return s
}

// Run drives a pass on every tick until ctx is cancelled.
//
// NO INITIAL PASS BEFORE THE FIRST TICK, unlike the cert monitors. This worker's
// action is irreversible without an owner's explicit reconnect, and a restart
// loop that swept on boot would run it as fast as the process crashed. One
// interval of delay after a deploy costs nothing against a four-day threshold.
func (s *Sweeper) Run(ctx context.Context) {
	if s.store == nil {
		s.logger.Info("telemetry inactivity sweeper not wired (no store); nothing will be warned or suspended")
		return
	}
	if !s.cfg.Enabled {
		s.logger.Warn("telemetry inactivity sweeper disabled; per-vehicle streaming will not be reclaimed")
		return
	}
	s.logger.Info("telemetry inactivity sweeper started",
		slog.Duration("interval", s.cfg.Interval),
		slog.Duration("warn_after", s.cfg.WarnAfter),
		slog.Duration("suspend_after", s.cfg.SuspendAfter),
		slog.Int("max_per_sweep", s.cfg.MaxPerSweep),
		slog.Bool("can_suspend", s.configs != nil && s.tokens != nil),
		slog.Bool("keepalive", s.keep != nil),
	)

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("telemetry inactivity sweeper stopped")
			return
		case <-ticker.C:
			s.SweepOnce(ctx)
		}
	}
}

// discardWriter drops log output for the nil-logger default.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
