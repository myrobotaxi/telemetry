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
	// vehicle so a hung Tesla call cannot consume the whole pass.
	defaultFleetConfigCallTimeout = 45 * time.Second
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
	return c
}

// RunReconcileLoop runs a pass every Interval until ctx is cancelled. It does
// NOT sweep on entry — cmd/ runs the boot pass synchronously (with its own
// timeout) so a slow Tesla cannot delay startup indefinitely while still
// guaranteeing one pass happens promptly after a deploy.
//
// A pass error is logged and the loop continues: the next tick is the retry.
func (r *FleetConfigReconciler) RunReconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := r.Reconcile(ctx); err != nil {
				r.logger.Warn("fleet-config reconcile: pass failed",
					slog.String("error", err.Error()))
			}
		}
	}
}
