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
	// default one active ride costs ~2.4 vehicle_data GETs per minute — ONE
	// Tesla request per cycle, which is a property of the interface the poller
	// holds (ridePositionRefresher) rather than of this comment: the two
	// MYR-320 enrichers that would make it two GETs are not reachable from it.
	Interval time.Duration
	// MaxLifetime is the ceiling on how long ONE vehicle's poller may run
	// before it stops itself, however open the ride still looks.
	//
	// It exists because "the ride is still open" is not a promise that anything
	// will ever close it. There is no automatic terminal transition anywhere in
	// this system — a rider no-show leaves the row at `arrived`, and a
	// dispatched reservation nobody cancels stays `accepted` with
	// dispatch_status='sent' — so a purely status-based gate is unbounded, and
	// one abandoned ride would spend Fleet API budget every 25 seconds until
	// the process restarted (~3,500 GETs/day, forever).
	//
	// This is the DEFENSIVE half. The authoritative half is the matching age
	// bound in store.pollableRidePredicate, which stops the reconcile from
	// re-adopting a ride this stale; without it, this deadline would only be
	// honoured until the next reconcile pass handed the poller straight back.
	// Keep the two horizons in step.
	MaxLifetime time.Duration
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
	//
	// It bounds ONE GET and its retries, and the retry count that matters here
	// is now zero for the case that dominates: 503 (Tesla's "this vehicle is
	// not answering") is no longer retryable, so an asleep car costs one
	// request per cycle rather than four inside this same budget.
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

	// defaultRideMaxLifetime is the poll-lifetime ceiling, and it MUST match
	// the horizon in store.pollableRidePredicate (six hours) — see MaxLifetime
	// on why one without the other does nothing.
	//
	// Six hours is chosen to be far outside any real ride and just inside the
	// waste it bounds. A leg long enough to reach it is a car driving for six
	// straight hours, which means a car that is ONLINE AND STREAMING — and a
	// streaming car's poll cycles are already no-ops, because layer 1 of the
	// stream yield skips the REST call entirely. So the ride that loses its
	// poller at the ceiling is, with high confidence, the one that was never
	// going anywhere.
	defaultRideMaxLifetime = 6 * time.Hour
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
	if c.MaxLifetime <= 0 {
		c.MaxLifetime = defaultRideMaxLifetime
	}
	return c
}
