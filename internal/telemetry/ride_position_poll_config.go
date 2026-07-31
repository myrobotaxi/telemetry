package telemetry

// MYR-394 ride-position poller tuning. Kept in its own file, alongside the
// poller itself, so ride_position_poller.go stays inside the 300-line cap —
// the same split service_status_periodic.go uses for PeriodicPollConfig.

import "time"

// RidePositionPollConfig tunes the poller. Zero values get defaults via
// withDefaults, matching the convention used by PeriodicPollConfig and
// push.TickerConfig.
type RidePositionPollConfig struct {
	// Enabled is the kill-switch (RIDE_POSITION_POLL_ENABLED). False means no
	// poller is ever started and the reconcile loop never runs: tracking
	// degrades exactly to its pre-MYR-394 behaviour (the last streamed fix),
	// which is a safe place to stand while an operator investigates Fleet API
	// rate limiting.
	Enabled bool
	// Interval is the poll cadence (RIDE_POSITION_POLL_INTERVAL). At the ~25s
	// default one active ride costs ~2.4 vehicle_data GETs per minute.
	Interval time.Duration
	// JitterFraction spreads each wait so that N simultaneous rides — and N
	// replicas after a rolling deploy — do not align into a burst.
	JitterFraction float64
	// CallTimeout bounds ONE vehicle_data read. Separate from Interval so a
	// hung Tesla request can never overrun into the next cycle.
	CallTimeout time.Duration
	// ReconcileInterval is the cadence of the safety-net reconcile that
	// re-derives the registry from the database. Slow on purpose: the event
	// seams do the real work and this only catches what they dropped.
	ReconcileInterval time.Duration
	// ReconcileTimeout bounds the reconcile LIST query. It does NOT bound the
	// pollers the pass starts, which own their own lifetimes.
	ReconcileTimeout time.Duration
	// MaxActive caps how many vehicles may be polled at once — the blast-radius
	// guard on the Fleet API budget if the ride table ever goes wrong. A target
	// refused by the cap is simply picked up by a later reconcile.
	MaxActive int
}

const (
	// defaultRidePollInterval is the cadence. 25s sits in the middle of the
	// 20-30s band the client approved, and is chosen against what it bounds: a
	// car doing 30mph moves ~370 yards between fixes, which is close enough for
	// a "where is my car" map without pretending to be the 1s live stream.
	//
	// internal/config restates this number rather than importing it, exactly as
	// it restates defaultServiceRepollInterval and for the same reason: config
	// must not depend on internal/telemetry. This package applies its own
	// default to a non-positive value, so the two can only ever disagree about
	// which of two identical numbers was used. The acceptable RANGE around it
	// is purely a config-validation concern and lives only there.
	defaultRidePollInterval = 25 * time.Second

	// defaultRidePollJitter spreads each wait by ±10%, matching the in-service
	// re-poll. Enough to de-synchronise concurrent rides and replicas; small
	// enough that the cadence still reads as "about every 25 seconds".
	defaultRidePollJitter = 0.10
	// defaultRidePollCallTimeout bounds one vehicle_data read. Deliberately
	// SHORTER than defaultFleetAPITimeout (30s): at a 25s cadence a 30s budget
	// would let one slow request straddle two cycles, and a position fix that
	// arrives half a minute late is not worth waiting for — the next cycle is
	// closer than the answer.
	defaultRidePollCallTimeout = 15 * time.Second
	// defaultRideReconcileInterval is the safety-net cadence. Five minutes is
	// chosen against what it bounds: the worst case it repairs is a poller that
	// outlived its ride because the bus dropped its terminal event, which costs
	// ~12 wasted GETs before the sweep reaps it.
	defaultRideReconcileInterval = 5 * time.Minute
	// defaultRideReconcileTimeout bounds the LIST query only.
	defaultRideReconcileTimeout = 15 * time.Second
	// defaultRideMaxActive caps concurrent pollers. Generous against any
	// plausible number of simultaneous rides at this scale, so the cap is a
	// guard rail rather than a routine truncation.
	defaultRideMaxActive = 50
)

func (c RidePositionPollConfig) withDefaults() RidePositionPollConfig {
	if c.Interval <= 0 {
		c.Interval = defaultRidePollInterval
	}
	if c.JitterFraction <= 0 {
		c.JitterFraction = defaultRidePollJitter
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = defaultRidePollCallTimeout
	}
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = defaultRideReconcileInterval
	}
	if c.ReconcileTimeout <= 0 {
		c.ReconcileTimeout = defaultRideReconcileTimeout
	}
	if c.MaxActive <= 0 {
		c.MaxActive = defaultRideMaxActive
	}
	return c
}
