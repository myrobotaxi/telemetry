package push

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// The ETA ticker (MYR-172, cadence from MYR-194 decision 3).
//
// A Live Activity's ETA is the one thing on it that goes wrong by SITTING
// STILL: the status only changes when something happens, but "arrives at 4:12"
// stops being true the moment traffic does. So while a leg is active the server
// re-pushes the current content-state every 60–90s whether or not anything
// happened.
//
// Shape follows the MYR-320 in-service re-poll — startup pass, then a jittered
// interval, a bounded LIST, a per-pass cap and a kill-switch — because that is
// this service's established answer to "run something periodically", and a
// second shape would be a second set of operational surprises.

// ActivityLeg is one live Activity together with its ride's current state.
type ActivityLeg struct {
	Activity
	RideContext
}

// ActivityLegStore is the ticker's view of the registry.
type ActivityLegStore interface {
	// ActiveLegs lists up to limit live Activities whose ride is mid-leg,
	// least recently updated first.
	ActiveLegs(ctx context.Context, limit int) ([]ActivityLeg, error)
	// SweepStale deletes rows untouched for longer than olderThan.
	SweepStale(ctx context.Context, olderThan time.Duration) (int64, error)
}

// TickerConfig tunes the ETA ticker. Zero values get defaults via withDefaults.
type TickerConfig struct {
	// Enabled is the kill-switch (LIVE_ACTIVITY_TICKER_ENABLED). False means
	// Run returns immediately: lifecycle transitions still update Activities,
	// and only the periodic ETA refresh stops. The Activity's own stale-date
	// then does its job, which is exactly the degraded mode MYR-194 designed
	// for — a safe place to stand while an operator investigates APNs
	// throttling.
	Enabled bool
	// Interval is the base cadence between passes.
	Interval time.Duration
	// JitterFraction spreads each wait by up to ±this fraction of Interval.
	JitterFraction float64
	// StartupDelay is how long after Run starts the first pass fires.
	StartupDelay time.Duration
	// ListTimeout bounds the LIST query so a database stall cannot wedge the
	// loop. It does not bound the sends, which run under the pass context.
	ListTimeout time.Duration
	// MaxPerPass caps one pass. An Activity missed by the cap is re-listed next
	// tick — and because the LIST is ordered by least-recently-updated, the cap
	// sheds the freshest rows, never the most neglected.
	MaxPerPass int
	// SweepAge is how long an untouched row survives before the sweep reaps it.
	SweepAge time.Duration
}

const (
	// defaultTickInterval sits at the MIDPOINT of MYR-194's 60–90s window so
	// that the default jitter lands the actual cadence inside the window at
	// both extremes: 75s ± 20% is 60s to 90s exactly.
	defaultTickInterval = 75 * time.Second
	// defaultTickJitter is 20% — chosen with the interval above to make the
	// jittered range coincide with the specified window, and incidentally to
	// de-synchronise replicas so they do not push Apple in a burst.
	defaultTickJitter = 0.20
	// defaultTickStartupDelay lets pools warm and subscriptions settle before
	// the first pass, without making a deploy drop a whole tick for every ride
	// already in flight.
	defaultTickStartupDelay = 20 * time.Second
	// defaultTickListTimeout bounds the LIST query only.
	defaultTickListTimeout = 10 * time.Second
	// defaultTickMaxPerPass bounds one pass. Generous against any plausible
	// count of simultaneously active rides, so the cap is a guard rail rather
	// than a routine truncation.
	defaultTickMaxPerPass = 200
	// defaultSweepAge is the 24-hour cleanup horizon. Comfortably longer than
	// any ride, so a row this old is one whose Activity died on the phone
	// without ever telling us.
	defaultSweepAge = 24 * time.Hour
	// sweepEvery bounds how often the sweep actually runs, independent of the
	// tick cadence. The reaping is not urgent — nothing reads these rows — and
	// an hourly DELETE over an indexed predicate is free, whereas one every 75
	// seconds is 1,150 pointless statements a day.
	sweepEvery = time.Hour
)

func (c TickerConfig) withDefaults() TickerConfig {
	if c.Interval <= 0 {
		c.Interval = defaultTickInterval
	}
	if c.JitterFraction <= 0 {
		c.JitterFraction = defaultTickJitter
	}
	if c.StartupDelay <= 0 {
		c.StartupDelay = defaultTickStartupDelay
	}
	if c.ListTimeout <= 0 {
		c.ListTimeout = defaultTickListTimeout
	}
	if c.MaxPerPass <= 0 {
		c.MaxPerPass = defaultTickMaxPerPass
	}
	if c.SweepAge <= 0 {
		c.SweepAge = defaultSweepAge
	}
	return c
}

// ActivityTicker refreshes the ETA on every live Activity whose leg is active.
type ActivityTicker struct {
	notifier *ActivityNotifier
	legs     ActivityLegStore
	cfg      TickerConfig
	logger   *slog.Logger

	// lastSweep is only ever touched from the single Run goroutine.
	lastSweep time.Time
}

// NewActivityTicker builds the ETA ticker over an existing notifier, reusing
// its sender, preference gate and rejected-token handling rather than growing a
// second copy of any of them.
func NewActivityTicker(
	notifier *ActivityNotifier,
	legs ActivityLegStore,
	cfg TickerConfig,
	logger *slog.Logger,
) *ActivityTicker {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &ActivityTicker{
		notifier: notifier,
		legs:     legs,
		cfg:      cfg.withDefaults(),
		logger:   logger,
	}
}

// Run drives the ticker until ctx is cancelled. Started as its own goroutine by
// cmd/ wiring, alongside the notifier's subscription, because it is a timer and
// not a subscription.
func (t *ActivityTicker) Run(ctx context.Context) {
	if t.legs == nil || t.notifier == nil || !t.cfg.Enabled {
		t.logger.Info("live activity ticker disabled",
			slog.Bool("wired", t.legs != nil && t.notifier != nil),
			slog.Bool("enabled", t.cfg.Enabled))
		return
	}

	t.logger.Info("live activity ticker started",
		slog.Duration("interval", t.cfg.Interval),
		slog.Duration("startup_delay", t.cfg.StartupDelay),
		slog.Float64("jitter_fraction", t.cfg.JitterFraction),
		slog.Int("max_per_pass", t.cfg.MaxPerPass),
		slog.Duration("sweep_age", t.cfg.SweepAge))

	wait := t.cfg.StartupDelay
	for {
		timer := time.NewTimer(jitterDuration(wait, t.cfg.JitterFraction))
		select {
		case <-ctx.Done():
			timer.Stop()
			t.logger.Info("live activity ticker stopped")
			return
		case <-timer.C:
		}
		t.RunPass(ctx)
		wait = t.cfg.Interval
	}
}

// RunPass performs ONE pass: refresh every active leg's Activity, then sweep if
// the sweep is due.
//
// Exported so tests drive a pass deterministically instead of racing a timer.
func (t *ActivityTicker) RunPass(ctx context.Context) {
	if !t.notifier.active() {
		return
	}

	listCtx, cancel := context.WithTimeout(ctx, t.cfg.ListTimeout)
	legs, err := t.legs.ActiveLegs(listCtx, t.cfg.MaxPerPass)
	cancel()
	if err != nil {
		t.logger.Error("live activity ticker: list failed",
			slog.String("error", err.Error()))
		return
	}

	now := t.notifier.now()
	var delivered int
	for _, leg := range legs {
		if !t.notifier.allowed(ctx, leg.UserID, leg.RideRequestID) {
			continue
		}
		// lowPriority: an ETA tick rides apns-priority 5 so it never competes
		// with a lifecycle transition for Apple's per-Activity budget
		// (MYR-194 decision 3).
		if t.notifier.send(ctx, leg.Activity, contentState(leg.RideContext, now), ActivityEventUpdate, nil, true, now) {
			delivered++
		}
	}

	if len(legs) > 0 {
		t.logger.Info("live activity ticker pass",
			slog.Int("legs", len(legs)),
			slog.Int("delivered", delivered),
			slog.Bool("capped", len(legs) == t.cfg.MaxPerPass))
	}

	t.sweepIfDue(ctx, now)
}

// sweepIfDue runs the stale-row cleanup at most once per sweepEvery.
func (t *ActivityTicker) sweepIfDue(ctx context.Context, now time.Time) {
	if !t.lastSweep.IsZero() && now.Sub(t.lastSweep) < sweepEvery {
		return
	}
	t.lastSweep = now

	sweepCtx, cancel := context.WithTimeout(ctx, t.cfg.ListTimeout)
	defer cancel()

	removed, err := t.legs.SweepStale(sweepCtx, t.cfg.SweepAge)
	if err != nil {
		t.logger.Error("live activity ticker: sweep failed",
			slog.String("error", err.Error()))
		return
	}
	if removed > 0 {
		t.logger.Info("live activity ticker: swept stale registrations",
			slog.Int64("removed", removed),
			slog.Duration("older_than", t.cfg.SweepAge))
	}
}

// jitterDuration spreads d by up to ±fraction of itself.
//
// A local copy of the helper the in-service re-poll uses rather than an import:
// internal/push must not depend on internal/telemetry, and the function is four
// lines. math/rand/v2 is not cryptographic and does not need to be — the only
// requirement is that two replicas disagree about when to wake up.
func jitterDuration(d time.Duration, fraction float64) time.Duration {
	if d <= 0 || fraction <= 0 {
		return d
	}
	spread := float64(d) * fraction
	// rand.Float64() is [0,1); map onto [-spread, +spread).
	return time.Duration(float64(d) + (rand.Float64()*2-1)*spread) //nolint:gosec // G404: cadence de-synchronisation, not a security decision
}
