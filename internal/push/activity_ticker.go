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

// ActivityKey identifies one registration — the (ride, party) pair the
// registry is unique on.
type ActivityKey struct {
	RideRequestID string
	UserID        string
}

// ActivityLegStore is the ticker's view of the registry.
type ActivityLegStore interface {
	// ActiveLegs lists up to limit live Activities whose ride is mid-leg,
	// least recently updated first.
	ActiveLegs(ctx context.Context, limit int) ([]ActivityLeg, error)
	// MarkPushed records that these registrations were just delivered to, so
	// the next pass orders them last.
	MarkPushed(ctx context.Context, keys []ActivityKey) (int64, error)
	// RidesAwaitingEnd lists up to limit rides that completed between heldFor
	// and giveUpAfter ago and still have a live Activity — the held `end`
	// pushes that are due and still worth attempting (MYR-421). Newest
	// completion first.
	RidesAwaitingEnd(ctx context.Context, heldFor, giveUpAfter time.Duration, limit int) ([]string, error)
	// AbandonHeldEnds tombstones completed rides whose held `end` was never
	// delivered within giveUpAfter of completion, sending nothing, and reports
	// how many Activities it retired. The retry loop's terminator.
	AbandonHeldEnds(ctx context.Context, giveUpAfter time.Duration) (int64, error)
	// SweepStale deletes rows untouched for longer than olderThan.
	SweepStale(ctx context.Context, olderThan time.Duration) (int64, error)
}

// TickerConfig tunes the ETA ticker. Zero values get defaults via withDefaults.
type TickerConfig struct {
	// Enabled is the kill-switch (LIVE_ACTIVITY_TICKER_ENABLED). False stops
	// the periodic ETA REFRESH and nothing else: lifecycle transitions still
	// update Activities, and the Activity's own stale-date then does its job,
	// which is exactly the degraded mode MYR-194 designed for — a safe place to
	// stand while an operator investigates APNs throttling.
	//
	// IT NO LONGER STOPS THE LOOP ITSELF (MYR-421), and that is what keeps its
	// documented meaning true rather than changing it. Since the completion
	// pair's `end` is held for five minutes, that end runs on this loop — and
	// it is the second half of a LIFECYCLE TRANSITION, not a refresh. Left
	// inside the switch, turning off the ETA ticker would silently strand every
	// completed card on every rider's lock screen until ActivityKit's own
	// multi-hour ceiling reaped it, which is a far worse degraded mode than the
	// one the switch was built to offer.
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
	if t.legs == nil || t.notifier == nil {
		t.logger.Info("live activity ticker not wired",
			slog.Bool("legs", t.legs != nil),
			slog.Bool("notifier", t.notifier != nil))
		return
	}

	t.logger.Info("live activity ticker started",
		// eta_refresh rather than "enabled": the loop runs either way now, and
		// what the switch actually governs is the periodic refresh. See
		// TickerConfig.Enabled.
		slog.Bool("eta_refresh", t.cfg.Enabled),
		slog.Duration("interval", t.cfg.Interval),
		slog.Duration("startup_delay", t.cfg.StartupDelay),
		slog.Float64("jitter_fraction", t.cfg.JitterFraction),
		slog.Int("max_per_pass", t.cfg.MaxPerPass),
		slog.Duration("held_end", DismissAfter),
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

// RunPass performs ONE pass: refresh every active leg's Activity, send the
// completion `end`s whose hold has expired, then sweep if the sweep is due.
//
// THE THREE STEPS ARE INDEPENDENT and run in that order because it reads as
// what it is — refresh the living, end the finished, reap the dead. Only the
// first is governed by the ETA kill-switch; see TickerConfig.Enabled for why the
// held end must not be.
//
// Exported so tests drive a pass deterministically instead of racing a timer.
func (t *ActivityTicker) RunPass(ctx context.Context) {
	if !t.notifier.active() {
		return
	}

	if t.cfg.Enabled {
		t.refreshActiveLegs(ctx)
	}
	t.endHeldCompletions(ctx)
	t.sweepIfDue(ctx, t.notifier.now())
}

// refreshActiveLegs re-pushes the current content-state to every live Activity
// whose ride is mid-leg.
func (t *ActivityTicker) refreshActiveLegs(ctx context.Context) {
	listCtx, cancel := context.WithTimeout(ctx, t.cfg.ListTimeout)
	legs, err := t.legs.ActiveLegs(listCtx, t.cfg.MaxPerPass)
	cancel()
	if err != nil {
		t.logger.Error("live activity ticker: list failed",
			slog.String("error", err.Error()))
		return
	}

	now := t.notifier.now()
	pushed := make([]ActivityKey, 0, len(legs))
	// Indexed rather than ranged by value: ActivityLeg grew a progress anchor
	// with MYR-398 and gocritic now flags the per-iteration copy, which runs
	// once per live Activity on every 60-90s pass.
	for i := range legs {
		leg := &legs[i]
		if !t.notifier.allowed(ctx, leg.UserID, leg.RideRequestID) {
			continue
		}
		state, anchor := contentState(leg.RideContext, leg.Progress, now)
		// THE ONE ALERT THAT ORIGINATES HERE. Arriving is a threshold on the
		// ETA rather than a ride status, so no transition announces it and the
		// ticker is the only thing that evaluates it — which also means it is
		// true on every remaining pass of the pickup leg once it is true at
		// all. alertFor's high-water comparison against the row is what turns
		// "true forty times" into "sent once"; without it this loop is the
		// strobing island the design forbids by name.
		//
		// THE HONEST BOUND, because "sent once" is a promise this loop cannot
		// quite make. The mark is read from a snapshot (ActiveLegs, at the top
		// of the pass) and raised only AFTER Apple accepts — see
		// saveAlertedPhase for why that ordering is the right one — so the
		// window between the read and the write is real and two things fit
		// through it. Two replicas can list the same leg at the same mark and
		// both attach the same alert. And one replica's snapshot AGES across a
		// pass of up to MaxPerPass sends, so a leg listed at Arriving can be
		// reached after another process has already alerted Arrived, putting
		// "almost here" on the rider's screen seconds after "your ride is
		// here".
		//
		// Both are bounded rather than eliminated: at most ONE duplicate
		// expansion per replica per phase, because the guarded UPDATE refuses
		// to lower the mark and the next pass reads the raised one — the
		// out-of-order case self-heals on that refusal, having already shown
		// its banner. The alternative is a claim-then-send latch (the repo has
		// the primitive — ClaimReservationDispatch), and it is the wrong trade
		// here: it burns a phase on every push APNs never accepts, and a
		// process that dies mid-send makes that unrecoverable. Between "the
		// island opened once more than it had to" and "a state change went
		// unseen", the first is the one to choose. Contract: §7.21.4.
		phase := alertPhaseFor(leg.RideContext, now)
		alert := alertFor(phase, leg.AlertedPhase)
		// LowPriority: an ETA tick rides apns-priority 5 so it never competes
		// with a lifecycle transition for Apple's per-Activity budget
		// (MYR-194 decision 3). An alerting tick is promoted back to 10 by
		// ActivityNotification.priority — the flag is a statement about a
		// routine refresh, and this push is not one.
		// A tick has no retry semantics — the next pass is the retry — so the
		// two failure outcomes are treated alike here and only `delivered`
		// stamps the row.
		if t.notifier.send(ctx, leg.Activity, ActivityNotification{
			ActivityToken: leg.Token,
			Sandbox:       leg.Sandbox,
			Event:         ActivityEventUpdate,
			ContentState:  state,
			Timestamp:     now,
			LowPriority:   true,
			Alert:         alert,
		}) == sendDelivered {
			pushed = append(pushed, ActivityKey{RideRequestID: leg.RideRequestID, UserID: leg.UserID})
			t.notifier.saveProgress(ctx, leg.Activity, anchor)
			if alert != nil {
				t.notifier.saveAlertedPhase(ctx, leg.Activity, phase)
			}
		}
	}

	t.markPushed(ctx, pushed)

	if len(legs) > 0 {
		t.logger.Info("live activity ticker pass",
			slog.Int("legs", len(legs)),
			slog.Int("delivered", len(pushed)),
			slog.Bool("capped", len(legs) == t.cfg.MaxPerPass))
	}
}

// markPushed stamps the delivered rows so the next pass sorts them last.
//
// This is what turns the LIST's `ORDER BY updated_at ASC LIMIT n` into an
// actual rotation. Without it the order is a fixed permutation and MaxPerPass
// truncates the same tail on every tick forever — the Activities that most need
// a refresh would be exactly the ones that never get one, and the anti-
// starvation claim in the query's own comment would be false.
//
// Non-fatal and detached from the pass deadline, which the sends may have
// consumed: a failed stamp costs one pass of fairness, never a pass of updates.
func (t *ActivityTicker) markPushed(ctx context.Context, keys []ActivityKey) {
	if len(keys) == 0 {
		return
	}

	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.cfg.ListTimeout)
	defer cancel()

	if _, err := t.legs.MarkPushed(markCtx, keys); err != nil {
		t.logger.Error("live activity ticker: mark pushed failed",
			slog.Int("activities", len(keys)),
			slog.String("error", err.Error()))
	}
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
	//
	// #nosec G404 -- not a security decision. The only requirement on this
	// number is that two replicas disagree about when to wake up; a
	// cryptographic source would buy nothing and cost a syscall per tick.
	return time.Duration(float64(d) + (rand.Float64()*2-1)*spread) //nolint:gosec // G404 — see the #nosec rationale above
}
