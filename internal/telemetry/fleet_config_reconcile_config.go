// Tunables and the background loop for the MYR-448 fleet-config reconciler.

package telemetry

import (
	"context"
	"log/slog"
	"time"
)

// Defaults for FleetConfigReconcileConfig.
//
// The cadence is deliberately slow. The condition being healed is "the owner
// has not yet tapped through virtual-key pairing in the Tesla app", which
// resolves on human timescales, and each pass costs one Fleet API read per
// quiet car. Fifteen minutes bounds the worst-case wait between an owner
// finishing pairing and their car being told to stream, which is well inside
// the patience of someone who has just completed setup.
const (
	defaultFleetConfigReconcileInterval = 15 * time.Minute
	// defaultFleetConfigStaleness is how long a Vehicle row must have been
	// quiet to be considered a candidate. A streaming car writes roughly every
	// 25s, so 30 minutes is far outside normal jitter while still leaving a
	// generous grace window for a link-time push that is still in flight.
	defaultFleetConfigStaleness = 30 * time.Minute
	// defaultFleetConfigMaxPerPass caps Fleet API calls per pass. Unreached
	// candidates simply come back next tick (the ORDER BY is oldest-first, so
	// the worst-off car is never starved).
	defaultFleetConfigMaxPerPass = 20
	// defaultFleetConfigCallTimeout bounds the token+read+push sequence for one
	// vehicle so a hung Tesla call cannot consume the whole pass. Chosen against
	// MaxPerPass so the worst case (20 × 20s = 400s) stays well inside the
	// interval — at 45s the worst case was exactly one interval, leaving the
	// loop running back to back with no headroom under Tesla degradation.
	defaultFleetConfigCallTimeout = 20 * time.Second
	// defaultFleetConfigMaxBackoff caps the exponential retry gap. An owner who
	// never pairs would otherwise cost a signed POST every interval forever;
	// twelve hours still reaches them the same day they finally pair.
	defaultFleetConfigMaxBackoff = 12 * time.Hour
	// defaultFleetConfigAwaitingKeyMaxBackoff is a TIGHTER cap that applies
	// only while Tesla is answering `missing_key` (MYR-489).
	//
	// The general 12h cap is right for "this car cannot be fixed": read
	// failures, dead tokens, unknown skips. Awaiting-key is the opposite — it
	// is the EXPECTED state of an owner who is mid-setup, the one state we most
	// want to exit promptly, and the one where MYR-448's backoff did active
	// harm: attempts made before the key existed pushed Nabil to an 8-hour gap
	// that began exactly when he paired. The applied-command signal now resets
	// that instantly, but only for an owner who sends a command; this bounds
	// the silent owner's worst case to two hours instead of twelve, at a cost
	// of at most 12 signed POSTs per unpaired car per day.
	defaultFleetConfigAwaitingKeyMaxBackoff = 2 * time.Hour
	// defaultFleetConfigSyncedQuietGrace is how long a car must remain silent
	// AFTER a synced config read before the forced re-push is allowed. It sits
	// at one Interval so, at default settings, the gate is "we saw synced, we
	// waited a full cycle, it is still silent".
	defaultFleetConfigSyncedQuietGrace = 15 * time.Minute
	// defaultFleetConfigAwakeWindow is how recently a signed command must have
	// applied for that alone to count as proof the car is alive. Beyond it the
	// escalation pays for a GetVehicle read instead of trusting a stale stamp.
	// A day is generous because the claim being made is only "this is a live
	// car in active onboarding", not "it is awake this second".
	defaultFleetConfigAwakeWindow = 24 * time.Hour
)

// FleetConfigReconcileConfig tunes the reconciler.
type FleetConfigReconcileConfig struct {
	// Interval is the gap between background passes.
	Interval time.Duration
	// Staleness is how quiet a Vehicle row must be to become a candidate.
	Staleness time.Duration
	// MaxPerPass caps candidates examined per pass.
	MaxPerPass int
	// CallTimeout bounds the Tesla calls for a single vehicle.
	CallTimeout time.Duration
	// MaxBackoff caps the exponential gap between attempts on one vehicle.
	MaxBackoff time.Duration
	// AwaitingKeyMaxBackoff caps it more tightly while Tesla reports the
	// virtual key missing — the state an owner is actively working to leave.
	AwaitingKeyMaxBackoff time.Duration
	// SyncedQuietGrace is how long a synced-but-silent car must stay silent
	// before the forced re-push escalation is permitted.
	SyncedQuietGrace time.Duration
	// AwakeWindow is how recently a signed command must have applied for that
	// stamp alone to prove the car is alive.
	AwakeWindow time.Duration
	// TickInterval is how often the loop LOOKS for due rows (MYR-529). It is
	// not the backoff base — see defaultFleetConfigTickInterval.
	TickInterval time.Duration
	// HotEpochWindow is how long a fresh pairing epoch earns the hot ladder and
	// the candidate query's staleness exemption (MYR-529).
	HotEpochWindow time.Duration
}

// backoffForOutcome is backoffFor with the MYR-489 awaiting-key override.
//
// Every other outcome keeps the MYR-448 curve exactly. Awaiting-key gets its
// own, lower ceiling because it is a state the owner is actively trying to
// leave, and being asleep in it is the failure this issue exists to fix.
func (c FleetConfigReconcileConfig) backoffForOutcome(attemptCount int, outcome string) time.Duration {
	d := c.backoffFor(attemptCount)
	if outcome != outcomeAwaitingKey {
		return d
	}
	if capped := c.withDefaults().AwaitingKeyMaxBackoff; d > capped {
		return capped
	}
	return d
}

// backoffFor returns how long to wait before the next attempt on a vehicle
// that has already had attemptCount consecutive unsuccessful ones.
//
// Interval doubles per attempt (15m, 30m, 1h, 2h, …) up to MaxBackoff. The
// shift is guarded rather than clever: attemptCount comes from the database
// and a large value would overflow the shift into nonsense (or zero, which
// would mean "retry immediately, forever" — the exact hot loop the backoff
// exists to prevent).
func (c FleetConfigReconcileConfig) backoffFor(attemptCount int) time.Duration {
	c = c.withDefaults()
	if attemptCount <= 0 {
		return c.Interval
	}
	const maxShift = 20 // 15m << 20 is ~1.4 years; far past MaxBackoff already
	if attemptCount > maxShift {
		return c.MaxBackoff
	}
	d := c.Interval << uint(attemptCount)
	if d <= 0 || d > c.MaxBackoff {
		return c.MaxBackoff
	}
	return d
}

// withDefaults fills zero fields. Negative values are treated as zero so a
// misconfiguration degrades to the default rather than to a hot loop.
func (c FleetConfigReconcileConfig) withDefaults() FleetConfigReconcileConfig {
	if c.Interval <= 0 {
		c.Interval = defaultFleetConfigReconcileInterval
	}
	if c.Staleness <= 0 {
		c.Staleness = defaultFleetConfigStaleness
	}
	if c.MaxPerPass <= 0 {
		c.MaxPerPass = defaultFleetConfigMaxPerPass
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = defaultFleetConfigCallTimeout
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = defaultFleetConfigMaxBackoff
	}
	if c.MaxBackoff < c.Interval {
		// A cap below the base would make the backoff shrink the retry gap.
		c.MaxBackoff = c.Interval
	}
	if c.AwaitingKeyMaxBackoff <= 0 {
		c.AwaitingKeyMaxBackoff = defaultFleetConfigAwaitingKeyMaxBackoff
	}
	if c.AwaitingKeyMaxBackoff < c.Interval {
		c.AwaitingKeyMaxBackoff = c.Interval
	}
	if c.AwaitingKeyMaxBackoff > c.MaxBackoff {
		// The awaiting-key ceiling only ever tightens the general one; a
		// configuration that inverts them would silently retry an unpairable
		// car MORE often than a dead one.
		c.AwaitingKeyMaxBackoff = c.MaxBackoff
	}
	if c.SyncedQuietGrace <= 0 {
		c.SyncedQuietGrace = defaultFleetConfigSyncedQuietGrace
	}
	if c.AwakeWindow <= 0 {
		c.AwakeWindow = defaultFleetConfigAwakeWindow
	}
	if c.TickInterval <= 0 {
		c.TickInterval = defaultFleetConfigTickInterval
	}
	if c.TickInterval > c.Interval {
		// A tick slower than the backoff base would put every car's real gap at
		// the tick rather than at its schedule, which is the MYR-529 bug with a
		// different number in it.
		c.TickInterval = c.Interval
	}
	if c.HotEpochWindow <= 0 {
		c.HotEpochWindow = defaultFleetConfigHotEpochWindow
	}
	return c
}

// startupDelay is how long RunReconcileLoop waits before its FIRST pass.
//
// The first pass deliberately does NOT run on the boot path. A pass is up to
// MaxPerPass sequential third-party HTTP round-trips, and running it before
// the listeners bind would leave /healthz and — far worse — the Tesla mTLS
// port refusing connections from real cars for the whole window, on every
// deploy. Waiting lets the server come up first while still getting a pass in
// promptly enough to be useful to whoever is watching the deploy.
const startupDelay = 30 * time.Second

// RunReconcileLoop runs a pass shortly after start and then every
// TickInterval, until ctx is cancelled. It ALSO drains the MYR-489 pairing
// inbox.
//
// THE TICK IS NOT THE CADENCE (MYR-529). Every vehicle's real gap is its own
// `next_attempt_at`, enforced in SQL; the tick only decides how promptly a row
// that has come due is noticed. Ticking at Interval conflated the two and made
// "scheduled for 01:38" mean "examined somewhere in 01:38–01:53", which is what
// made the hot pairing ladder impossible and what left Joey Verrone's car
// unexamined while he drove it. A tick with nothing due costs one indexed query.
//
// A pass error is logged and the loop continues: the next tick is the retry.
//
// ONE GOROUTINE, ON PURPOSE. Ticks and pairing signals share this single
// select, so a signed command applied WHILE a pass is running cannot race it:
// the VIN waits in the buffered inbox and is handled the moment the pass
// returns, reading the schedule row that pass has already written. Handling
// signals on their own goroutine would have two writers on one vehicle's
// schedule and could interleave a backoff reset with the very attempt that was
// about to supersede it.
//
// The startup window is covered too — signals that arrive during startupDelay
// or the first pass are buffered, not dropped.
func (r *FleetConfigReconciler) RunReconcileLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(startupDelay):
	}
	r.runPass(ctx)

	ticker := time.NewTicker(r.cfg.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runPass(ctx)
		case vin := <-r.pairing:
			r.handlePairingSignal(ctx, vin)
		}
	}
}

func (r *FleetConfigReconciler) runPass(ctx context.Context) {
	if _, err := r.Reconcile(ctx); err != nil {
		r.logger.Warn("fleet-config reconcile: pass failed",
			slog.String("error", err.Error()))
	}
}
