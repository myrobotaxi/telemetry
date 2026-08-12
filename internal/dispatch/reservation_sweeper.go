package dispatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// Reservation-time dispatch (MYR-179). An INSTANT ride's pickup nav fires when
// the owner accepts; a SCHEDULED ride's must fire at `scheduledFor`, which may
// be hours or days later. v1 changes only WHEN the existing leg-1 push runs:
//
//	accept of a scheduled ride → Dispatcher.process returns without claiming
//	                             (see dispatcher_leg.go); the row stays
//	                             latch-unclaimed / outcome-absent.
//	the leave-time arrives     → this sweeper claims the SAME dispatched_at
//	                             latch and runs the SAME runClaimedLeg push.
//
// The leave-time is scheduledFor MINUS a computed dispatch lead (MYR-535,
// client decision 2026-08-12): `scheduledFor` is the instant the car should
// ARRIVE at the pickup, so dispatching at that instant guaranteed the car left
// exactly one trip late. The lead is the estimated drive time from the car's
// current position to the pickup plus a small buffer, clamped to
// [LeadFloor, LeadCap] — see reservation_lead.go.
//
// Everything downstream is inherited, not re-derived: exactly-once (the latch
// admits one winner across any number of sweepers or ticks), the bounded
// retry, the outcome contract, and — critically — CRASH RECOVERY. A process
// that dies between claim and outcome leaves dispatched_at set /
// dispatch_status NULL, which is exactly the orphan shape the leg-1 startup
// reconciler already resolves; its query filters on the latch columns ONLY and
// has never mentioned scheduled_for, so scheduled rows were already in scope
// and it needed no widening.
//
// That inheritance is only sound while the sweeper preserves the instant
// path's TIMING invariant as well as its shape: the claim happens inside the
// worker, after a slot is acquired and immediately before the push, so
// claim→outcome is bounded by the dispatcher's OverallTimeout and sits safely
// under the reconciler's 5-minute age floor. The sweep pass itself only
// SELECTS candidates; it never latches a row it is not about to act on.

// DueReservation is one accepted reservation whose pickup push is due. Built
// by the cmd/ adapter from the lean due projection; the sweeper needs the
// pickup to push, the vehicle to check for busy, the owner to resolve a Tesla
// token, and ScheduledFor to judge the lateness deadline.
type DueReservation struct {
	RideRequestID string
	VehicleID     string
	RiderID       string
	OwnerID       string
	Pickup        events.RidePlace
	ScheduledFor  time.Time
}

// VehicleDispatchState is the one-read answer to the two vehicle-level
// questions the sweeper asks before claiming a due reservation. Both fields
// are stated POSITIVELY (true = the reservation may proceed on that axis) so
// no caller has to keep an inverted boolean straight.
type VehicleDispatchState struct {
	// InService: the owner has the car in for service. Holds the reservation.
	InService bool
	// RideShareEnabled: the owner still accepts rides against this car
	// (MYR-342). False means PAUSED — nothing about the vehicle itself. A car
	// with no control-state row reads as enabled.
	RideShareEnabled bool
}

// ReservationStore is the sweeper's store seam. Satisfied by the ride-request
// repo via a cmd/ adapter. The outcome WRITE still goes through the existing
// OutcomeStore leg-1 record seam — a reservation resolves into the very same
// columns an instant ride does.
type ReservationStore interface {
	// ListDueReservations returns accepted, latch-unclaimed reservations whose
	// scheduledFor is at or before dueBefore — the DISPATCH HORIZON, now +
	// LeadCap, so a candidate is visible for the whole window it could
	// legally leave in (MYR-535) — AND which are actionable this pass: either
	// the vehicle is free, or the reservation is already past expiredBefore
	// (= now - MaxLateness) and must be resolved regardless. Oldest first,
	// capped at limit.
	ListDueReservations(ctx context.Context, dueBefore, expiredBefore time.Time, limit int) ([]DueReservation, error)
	// VehicleHasActiveInstantRide reports whether the vehicle is mid-ride on
	// an active INSTANT ride (the MYR-266 per-vehicle busy predicate).
	VehicleHasActiveInstantRide(ctx context.Context, vehicleID string) (bool, error)
	// VehicleDispatchState reads, in ONE statement, the two vehicle-level
	// facts the sweeper must agree on before it claims: whether the car is in
	// service (MYR-372) and whether its owner still accepts rides (MYR-342).
	//
	// One read rather than two, matching the discipline the accept gate keeps
	// deliberately — that path resolves ownership and availability from a
	// single row so they cannot disagree, and there is no reason the
	// unattended path should hold itself to less. It also halves the
	// per-reservation round trips.
	//
	// An unknown vehicle id IS an error: this reads the vehicle row itself,
	// where absence is a real and reportable fact. (The ride-share flag alone
	// would have answered `true` for an unknown id, since it lives on a side
	// table with no opinion on existence — but a missing vehicle is not a
	// missing opinion, and the sweeper holds either way.)
	VehicleDispatchState(ctx context.Context, vehicleID string) (VehicleDispatchState, error)
	// VehiclePosition returns the car's freshest known position, or a nil
	// pair when the server holds none. Feeds the dispatch-lead estimate
	// (MYR-535) and NOTHING else — a wrong or stale position can only move
	// WHEN the car is dispatched (bounded by [LeadFloor, LeadCap]), never
	// whether or where. The (0, 0) no-fix sentinel may come back as a
	// non-nil pair (storage does not collapse it) and callers must treat it
	// as no fix, per vehicle-state-schema.md §2.3.
	VehiclePosition(ctx context.Context, vehicleID string) (lat, lng *float64, err error)
	// RiderMayRequestRides reports whether riderID is still permitted to
	// ride in vehicleID (MYR-369) — true when they OWN the car, and
	// otherwise true only when they hold a LIVE accepted share whose
	// allow_rides flag is set.
	//
	// A suspended grant answers false, identically to no grant at all: the
	// suspension invariant reaches the dispatch path through this method,
	// which is the only layer that runs outside a request and therefore the
	// only one that can catch an owner who withdrew access AFTER the
	// reservation was accepted.
	RiderMayRequestRides(ctx context.Context, riderID, vehicleID string) (bool, error)
	// ClaimReservationDispatch wins the leg-1 latch for a reservation, but
	// ONLY while the row is still `accepted` and still a reservation. false
	// means the row was claimed by someone else or moved on — skip silently.
	ClaimReservationDispatch(ctx context.Context, rideID string) (bool, error)
}

// ReservationConfig tunes the sweeper. Zero values get sane defaults via
// withDefaults, matching the Config convention in this package.
type ReservationConfig struct {
	// Enabled is the kill-switch (RESERVATION_DISPATCH_ENABLED). False stops
	// the sweeper from running at all: reservations stay accepted, unclaimed
	// and outcome-absent, so flipping it back on dispatches the ones still
	// inside the lateness window and honestly fails the rest. Distinct from
	// the dispatcher's own Enabled (DISPATCH_ENABLED), which — because the
	// sweeper reuses runClaimedLeg — still records a due reservation as
	// `skipped` exactly like an instant accept.
	Enabled bool
	// Interval is the sweep cadence. It bounds how late a punctual dispatch
	// can be (a reservation due just after a tick waits one interval).
	Interval time.Duration
	// MaxLateness is the lateness ceiling: how far past scheduledFor a
	// reservation may still be dispatched. Past it the reservation is failed
	// honestly, whatever the vehicle is doing. See pastDeadline.
	MaxLateness time.Duration
	// SweepTimeout bounds the due-list QUERY, so a database stall can never
	// wedge the ticker. It does NOT bound the per-reservation work, which runs
	// under the dispatcher's own OverallTimeout.
	SweepTimeout time.Duration
	// MaxPerSweep caps how many due reservations one pass considers. Because
	// claims are just-in-time, a candidate this pass does not reach is simply
	// re-selected next tick — nothing is lost by keeping it modest.
	MaxPerSweep int
	// MaxConcurrent caps how many reservations one pass works on at once. It
	// is the sweeper's OWN budget, deliberately separate from (and much
	// smaller than) the dispatcher's instant-dispatch pool.
	MaxConcurrent int
	// LeadBuffer / LeadFloor / LeadCap shape the dispatch lead (MYR-535):
	// lead = clamp(estimated drive time + LeadBuffer, LeadFloor, LeadCap),
	// and the car is dispatched once now >= scheduledFor - lead. The buffer
	// absorbs the owner walking to the car and the estimate's optimism; the
	// floor is what a car with no known position (or one already at the
	// pickup) gets; the cap bounds both the estimate's pessimism and the
	// SELECT horizon — no reservation is ever visible to a pass more than
	// LeadCap before its pickup instant.
	LeadBuffer time.Duration
	LeadFloor  time.Duration
	LeadCap    time.Duration
}

const (
	defaultSweepInterval = 30 * time.Second
	// defaultMaxLateness is the lateness ceiling for a due reservation. 30
	// minutes covers a typical in-progress trip (the vehicle-busy hold) without
	// stranding the reservation: past it we stop waiting and record an honest
	// failure rather than dial nav at a car that is still carrying someone else
	// — or at a car whose rider gave up hours ago, after downtime.
	defaultMaxLateness  = 30 * time.Minute
	defaultSweepTimeout = 30 * time.Second
	// defaultMaxPerSweep bounds one pass. Modest on purpose: claims are
	// just-in-time, so an unreached candidate reappears on the next 30s tick
	// rather than being skipped, and a small window keeps a post-outage
	// backlog from monopolising database connections.
	defaultMaxPerSweep = 25
	// defaultReservationConcurrency is the sweeper's own worker budget. It is
	// SEPARATE from the dispatcher's MaxConcurrent (which serves instant
	// accepts and rider starts) so a backlog of asleep-vehicle reservation
	// pushes — each up to OverallTimeout — can never queue in front of an
	// instant accept's pickup push. Small: reservation punctuality is measured
	// in tens of seconds, instant dispatch in hundreds of milliseconds.
	defaultReservationConcurrency = 2
	// Dispatch-lead defaults (MYR-535, client decision 2026-08-12: "computed
	// travel time … + ~3 min buffer, floored ~5 min, capped 30 min").
	defaultLeadBuffer = 3 * time.Minute
	defaultLeadFloor  = 5 * time.Minute
	defaultLeadCap    = 30 * time.Minute
)

func (c ReservationConfig) withDefaults() ReservationConfig {
	if c.Interval <= 0 {
		c.Interval = defaultSweepInterval
	}
	if c.MaxLateness <= 0 {
		c.MaxLateness = defaultMaxLateness
	}
	if c.SweepTimeout <= 0 {
		c.SweepTimeout = defaultSweepTimeout
	}
	if c.MaxPerSweep <= 0 {
		c.MaxPerSweep = defaultMaxPerSweep
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = defaultReservationConcurrency
	}
	if c.LeadBuffer <= 0 {
		c.LeadBuffer = defaultLeadBuffer
	}
	if c.LeadFloor <= 0 {
		c.LeadFloor = defaultLeadFloor
	}
	if c.LeadCap <= 0 {
		c.LeadCap = defaultLeadCap
	}
	return c
}

// ReservationSweeper fires the leg-1 pickup push for scheduled rides at their
// reservation instant. Construct with NewReservationSweeper, then Run it in a
// goroutine alongside the dispatcher.
type ReservationSweeper struct {
	dispatcher *Dispatcher
	store      ReservationStore
	bus        events.Bus
	cfg        ReservationConfig
	logger     *slog.Logger

	sem chan struct{}    // the sweeper's OWN worker budget (never the dispatcher's)
	now func() time.Time // injectable clock (lateness + due-boundary tests)

	// activities ends the rider's Live Activity when a reservation expires
	// (MYR-172). Nil is the ordinary unwired state, not a fault.
	activities RideActivityEnder
}

// NewReservationSweeper builds a sweeper over an already-constructed
// Dispatcher, whose leg-1 record seam and nav-push machinery it reuses.
// bus may be nil (the `ride.due` publish is then a no-op); logger may be nil.
func NewReservationSweeper(
	dispatcher *Dispatcher,
	store ReservationStore,
	bus events.Bus,
	cfg ReservationConfig,
	logger *slog.Logger,
) *ReservationSweeper {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	cfg = cfg.withDefaults()
	return &ReservationSweeper{
		dispatcher: dispatcher,
		store:      store,
		bus:        bus,
		cfg:        cfg,
		logger:     logger,
		sem:        make(chan struct{}, cfg.MaxConcurrent),
		now:        time.Now,
	}
}

// withClock injects a clock for deterministic lateness / due-boundary tests.
func (s *ReservationSweeper) withClock(now func() time.Time) *ReservationSweeper {
	if now != nil {
		s.now = now
	}
	return s
}

// Run drives the sweep on a ticker until ctx is canceled. Intended to be run
// in its own goroutine. It returns immediately when the kill-switch is off.
//
// Passes do NOT overlap: sweepOnce is called synchronously and waits for its
// own workers, so a slow pass simply delays the next tick (Ticker drops the
// ticks it cannot deliver) instead of stacking a second pass on top of the
// first. That is the backpressure that keeps a post-outage backlog from
// spawning unbounded work.
//
// Nothing is swept at startup before the first tick: the leg-1 startup
// reconciler runs first and must be allowed to resolve orphaned claims before
// the sweeper starts making new ones.
func (s *ReservationSweeper) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("reservation sweeper disabled (RESERVATION_DISPATCH_ENABLED=false)")
		return
	}
	s.logger.Info("reservation sweeper started",
		slog.Duration("interval", s.cfg.Interval),
		slog.Duration("max_lateness", s.cfg.MaxLateness),
		slog.Int("max_per_sweep", s.cfg.MaxPerSweep),
		slog.Int("max_concurrent", s.cfg.MaxConcurrent),
	)
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("reservation sweeper stopped")
			return
		case <-ticker.C:
			s.sweepOnce(ctx)
		}
	}
}
