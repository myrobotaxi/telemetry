package telemetry

// MYR-320 periodic in-service re-poll — a STRUCTURAL fix, not a new feature.
//
// Every REST read this monitor performs is EDGE-triggered: a connectivity
// connect/disconnect (MYR-259) or a live ServiceMode transition (MYR-259). That
// is a good trigger for a car that comes and goes, and a useless one for the
// case it matters most in. A car sitting AT A SERVICE CENTRE is typically
// offline for days: it emits no telemetry, so no ServiceMode edge fires, and it
// raises no mTLS connection, so no connectivity edge fires either. Nothing ever
// re-reads it. Two consequences, both observed:
//
//   - A `service_etc` that Tesla publishes MID-VISIT (the appointment record
//     appears a day after drop-off, or the estimate slips) is never picked up.
//     serviceEstimatedEndAt stays null — the exact value MYR-316 exists to
//     supply and MYR-313 schedules against.
//   - The REST-only detail fields (`trim`, `trimLabel`, `fsdVersion`,
//     `seatCoolingCapable`) are only ever acquired on an edge, so a car that has
//     not produced one since the feature shipped never populates them at all.
//
// The fix is a periodic pass that runs the SAME read bundle the edge path runs,
// for every vehicle currently persisted as in_service. It adds no new read
// semantics whatsoever: it calls resolveViaRead, which is the edge path, so it
// inherits the per-VIN debounce (no double-fetch when an edge just fired), the
// MYR-300 stream-recency gate, the non-waking guarantee, and every non-fatal
// failure rule already established. The ONLY thing new here is the trigger.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"log/slog"
	"time"
)

// InServiceLister lists the VINs of vehicles currently persisted as
// `in_service`. Satisfied by an adapter over store.VehicleRepo. Declared at the
// consumer site so the monitor is unit-testable with a fake — NO real Tesla call
// and no real database access ever fires from a test.
//
// The limit is a hard cap on one pass. An unreached vehicle is simply re-listed
// next tick, so a small cap costs latency, never coverage.
type InServiceLister interface {
	ListInServiceVINs(ctx context.Context, limit int) ([]string, error)
}

// PeriodicPollConfig tunes the in-service re-poll. Zero values get sane defaults
// via withDefaults, matching the Config convention elsewhere in the codebase.
type PeriodicPollConfig struct {
	// Enabled is the kill-switch (SERVICE_REPOLL_ENABLED). False means Run
	// returns immediately having issued NO reads: the monitor degrades exactly
	// to its pre-MYR-320, edge-only behaviour, which is a safe place to stand
	// while an operator investigates Fleet API rate limiting.
	Enabled bool
	// Interval is the re-poll cadence (SERVICE_REPOLL_INTERVAL). It bounds how
	// stale an in-service car's service window and detail fields can get.
	Interval time.Duration
	// JitterFraction spreads each wait by up to ±this fraction of Interval, so
	// N server instances (and N restarts) do not align their passes into a
	// synchronised burst against the Fleet API. 0 disables jitter.
	JitterFraction float64
	// StartupDelay is how long after Run starts the FIRST pass fires. Short on
	// purpose: a deploy must not have to wait a full Interval before an
	// in-service car is re-read, which is half the reason this exists. Jittered
	// like the interval so a rolling deploy does not stampede.
	StartupDelay time.Duration
	// ListTimeout bounds the in-service LIST query so a database stall cannot
	// wedge the loop. It does NOT bound the per-vehicle reads, which each run
	// under their own defaultFleetAPITimeout context exactly as an edge read
	// does.
	ListTimeout time.Duration
	// MaxPerPass caps one pass. A fleet's in-service population is tiny by
	// nature (a handful of cars at a service centre), so this is a blast-radius
	// guard against a status column gone wrong, not a throughput knob.
	MaxPerPass int
}

const (
	// defaultRepollInterval is the re-poll cadence. 15 minutes is chosen
	// against what it bounds: a service estimate that moves is worth knowing
	// within the quarter-hour, and nothing downstream reacts faster than a
	// human reading a screen. At 4 GETs per in-service car per pass (vehicle,
	// service_data, vehicle_data, release_notes) and a handful of cars in
	// service at once, that is a rounding error against Tesla's rate limits.
	defaultRepollInterval = 15 * time.Minute
	// defaultRepollJitter spreads each wait by ±10%. Enough to de-synchronise
	// instances and restarts; small enough that the cadence still reads as
	// "about every 15 minutes" in the logs.
	defaultRepollJitter = 0.10
	// defaultRepollStartupDelay lets the process finish coming up — pools warm,
	// subscriptions live, the leg-1 reconciler settled — before the first pass
	// adds Fleet API traffic, without making a deploy wait a whole interval.
	defaultRepollStartupDelay = 30 * time.Second
	// defaultRepollListTimeout bounds the LIST query only.
	defaultRepollListTimeout = 15 * time.Second
	// defaultRepollMaxPerPass bounds one pass. Deliberately generous relative to
	// any plausible in-service population, so the cap is a guard rail rather
	// than a routine truncation.
	defaultRepollMaxPerPass = 100
)

func (c PeriodicPollConfig) withDefaults() PeriodicPollConfig {
	if c.Interval <= 0 {
		c.Interval = defaultRepollInterval
	}
	if c.JitterFraction <= 0 {
		c.JitterFraction = defaultRepollJitter
	}
	if c.StartupDelay <= 0 {
		c.StartupDelay = defaultRepollStartupDelay
	}
	if c.ListTimeout <= 0 {
		c.ListTimeout = defaultRepollListTimeout
	}
	if c.MaxPerPass <= 0 {
		c.MaxPerPass = defaultRepollMaxPerPass
	}
	return c
}

// WithPeriodicInServicePoll enables the MYR-320 periodic re-poll. A nil lister
// leaves it disabled, so a deployment without the adapter wired keeps the
// pre-MYR-320 edge-only behaviour.
func WithPeriodicInServicePoll(lister InServiceLister, cfg PeriodicPollConfig) ServiceStatusMonitorOption {
	return func(m *ServiceStatusMonitor) {
		if lister == nil {
			return
		}
		m.inServiceVINs = lister
		m.periodic = cfg.withDefaults()
	}
}

// RunPeriodicInServicePoll runs the re-poll loop until ctx is cancelled. Started
// as its own goroutine by cmd/ wiring, alongside (not inside) Start, because it
// is a timer, not a subscription.
//
// The loop is startup-pass-then-interval: one pass after StartupDelay so a
// deploy takes effect in seconds rather than a quarter-hour, then a pass every
// jittered Interval. Both waits are jittered so restarts and replicas do not
// align.
func (m *ServiceStatusMonitor) RunPeriodicInServicePoll(ctx context.Context) {
	if m.inServiceVINs == nil || !m.periodic.Enabled {
		m.logger.Info("service-status: periodic in-service re-poll disabled",
			slog.Bool("wired", m.inServiceVINs != nil),
			slog.Bool("enabled", m.periodic.Enabled))
		return
	}

	m.logger.Info("service-status: periodic in-service re-poll started",
		slog.Duration("interval", m.periodic.Interval),
		slog.Duration("startup_delay", m.periodic.StartupDelay),
		slog.Float64("jitter_fraction", m.periodic.JitterFraction),
		slog.Int("max_per_pass", m.periodic.MaxPerPass))

	wait := m.periodic.StartupDelay
	for {
		timer := time.NewTimer(jitterDuration(wait, m.periodic.JitterFraction))
		select {
		case <-ctx.Done():
			timer.Stop()
			m.logger.Info("service-status: periodic in-service re-poll stopped")
			return
		case <-timer.C:
		}
		m.RunInServicePass(ctx)
		wait = m.periodic.Interval
	}
}

// RunInServicePass performs ONE pass: list the in-service VINs and run the
// ordinary edge read bundle for each.
//
// Exported so tests drive a pass deterministically instead of racing a timer,
// and so an operator-triggered pass can be added later without duplicating this.
//
// Per-VIN work goes through resolveViaRead — the SAME function the connectivity
// edge calls — which is what makes this a trigger change and nothing more:
//
//   - the per-VIN cooldown (allow(), 45s) is checked inside it, so a vehicle the
//     edge path just read is SKIPPED rather than double-fetched;
//   - the MYR-300 stream-recency gate still drops stream-sourceable fields for a
//     car that is in fact streaming;
//   - it never force-wakes, so an asleep car answers 408 and is skipped
//     non-fatally;
//   - status is reconciled too, so a car that left service while offline stops
//     being listed here at all.
//
// Vehicles are processed SEQUENTIALLY. The in-service population is small, and
// serial work keeps the Fleet API request rate flat and predictable — there is
// nothing to gain from racing a handful of reads that nobody is waiting on.
func (m *ServiceStatusMonitor) RunInServicePass(ctx context.Context) {
	cfg := m.periodic.withDefaults()

	listCtx, cancel := context.WithTimeout(ctx, cfg.ListTimeout)
	vins, err := m.inServiceVINs.ListInServiceVINs(listCtx, cfg.MaxPerPass)
	cancel()
	if err != nil {
		m.logger.Warn("service-status: in-service list failed, skipping pass (non-fatal)",
			slog.String("error", err.Error()))
		return
	}
	if len(vins) == 0 {
		m.logger.Debug("service-status: periodic pass found no in-service vehicles")
		return
	}

	m.logger.Info("service-status: periodic in-service pass starting",
		slog.Int("vehicles", len(vins)))

	for _, vin := range vins {
		if ctx.Err() != nil {
			return // shutting down; the next process's startup pass picks these up
		}
		// A fresh bounded context per vehicle, exactly as the connectivity-edge
		// handler builds one, so one unresponsive car cannot stall the rest of
		// the pass.
		vinCtx, vinCancel := context.WithTimeout(ctx, defaultFleetAPITimeout)
		m.resolveViaRead(vinCtx, vin, "periodic")
		vinCancel()
	}
}

// jitterDuration spreads d by up to ±fraction of itself, so replicas and
// restarts do not align their passes into a synchronised burst against the Fleet
// API.
//
// crypto/rand rather than math/rand: the codebase already reaches for it
// wherever randomness is needed (internal/drives/debounce.go), it costs nothing
// at this cadence, and it keeps gosec quiet without a suppression comment. A
// read failure falls back to the UNJITTERED duration — losing de-synchronisation
// is trivial; failing to schedule the pass at all would not be.
func jitterDuration(d time.Duration, fraction float64) time.Duration {
	if d <= 0 {
		return 0
	}
	if fraction <= 0 {
		return d
	}

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return d
	}
	// Map the 64 random bits onto [-1, +1), then scale by fraction*d.
	unit := float64(binary.BigEndian.Uint64(b[:])>>11)/float64(1<<53)*2 - 1
	jittered := time.Duration(float64(d) * (1 + fraction*unit))
	if jittered <= 0 {
		return d
	}
	return jittered
}
