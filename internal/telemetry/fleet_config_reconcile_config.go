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

// RunReconcileLoop runs a pass shortly after start and then every Interval,
// until ctx is cancelled.
//
// A pass error is logged and the loop continues: the next tick is the retry.
func (r *FleetConfigReconciler) RunReconcileLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(startupDelay):
	}
	r.runPass(ctx)

	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runPass(ctx)
		}
	}
}

func (r *FleetConfigReconciler) runPass(ctx context.Context) {
	if _, err := r.Reconcile(ctx); err != nil {
		r.logger.Warn("fleet-config reconcile: pass failed",
			slog.String("error", err.Error()))
	}
}
