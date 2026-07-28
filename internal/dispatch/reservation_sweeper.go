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
//	scheduledFor arrives       → this sweeper claims the SAME dispatched_at
//	                             latch and runs the SAME runClaimedLeg push.
//
// Everything downstream is inherited, not re-derived: exactly-once (the latch
// admits one winner across any number of sweepers or ticks), the bounded
// retry, the outcome contract, and — critically — CRASH RECOVERY. A process
// that dies between claim and outcome leaves dispatched_at set /
// dispatch_status NULL, which is exactly the orphan shape the leg-1 startup
// reconciler already resolves; its query filters on the latch columns ONLY and
// has never mentioned scheduled_for, so scheduled rows were already in scope
// and it needed no widening.

// DueReservation is one accepted reservation whose pickup push is due. Built
// by the cmd/ adapter from the ride row; the sweeper needs the pickup to push,
// the vehicle to check for busy, the owner to resolve a Tesla token, and
// ScheduledFor to judge the busy-hold deadline.
type DueReservation struct {
	RideRequestID string
	VehicleID     string
	RiderID       string
	OwnerID       string
	Pickup        events.RidePlace
	ScheduledFor  time.Time
}

// ReservationStore is the sweeper's read seam. Satisfied by the ride-request
// repo via a cmd/ adapter. The sweeper's WRITES go through the existing
// OutcomeStore leg-1 pair — it claims and records the very same columns the
// instant path does.
type ReservationStore interface {
	// ListDueReservations returns accepted reservations that are still
	// latch-unclaimed and whose scheduledFor is at or before now, oldest
	// first, capped at limit.
	ListDueReservations(ctx context.Context, now time.Time, limit int) ([]DueReservation, error)
	// VehicleHasActiveInstantRide reports whether the vehicle is mid-ride on
	// an active INSTANT ride (the MYR-266 per-vehicle busy predicate).
	VehicleHasActiveInstantRide(ctx context.Context, vehicleID string) (bool, error)
}

// ReservationConfig tunes the sweeper. Zero values get sane defaults via
// withDefaults, matching the Config convention in this package.
type ReservationConfig struct {
	// Enabled is the kill-switch (RESERVATION_DISPATCH_ENABLED). False stops
	// the sweeper from running at all: reservations stay accepted, unclaimed
	// and outcome-absent, so flipping it back on dispatches the ones still
	// inside the busy-hold window and honestly fails the rest. Distinct from
	// the dispatcher's own Enabled (DISPATCH_ENABLED), which — because the
	// sweeper reuses runClaimedLeg — still records a due reservation as
	// `skipped` exactly like an instant accept.
	Enabled bool
	// Interval is the sweep cadence. It bounds how late a punctual dispatch
	// can be (a reservation due just after a tick waits one interval).
	Interval time.Duration
	// BusyHold is how long past scheduledFor a due reservation keeps waiting
	// for a busy vehicle before it is failed honestly. See holdExpired.
	BusyHold time.Duration
	// SweepTimeout bounds one whole sweep pass (list + per-row claim), so a
	// database stall can never wedge the ticker. It does NOT bound the nav
	// pushes, which run on the dispatcher's own OverallTimeout.
	SweepTimeout time.Duration
	// MaxPerSweep caps how many due reservations one pass claims. A backlog
	// beyond it drains on following ticks, oldest reservation first.
	MaxPerSweep int
}

const (
	defaultSweepInterval = 30 * time.Second
	// defaultBusyHold is the grace period a due reservation waits for a
	// vehicle that is mid-ride. 30 minutes covers a typical in-progress trip
	// without stranding the reservation indefinitely: past it we stop waiting
	// and record an honest failure rather than dial nav at a car that is
	// still carrying someone else.
	defaultBusyHold     = 30 * time.Minute
	defaultSweepTimeout = 30 * time.Second
	defaultMaxPerSweep  = 200
)

func (c ReservationConfig) withDefaults() ReservationConfig {
	if c.Interval <= 0 {
		c.Interval = defaultSweepInterval
	}
	if c.BusyHold <= 0 {
		c.BusyHold = defaultBusyHold
	}
	if c.SweepTimeout <= 0 {
		c.SweepTimeout = defaultSweepTimeout
	}
	if c.MaxPerSweep <= 0 {
		c.MaxPerSweep = defaultMaxPerSweep
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

	now func() time.Time // injectable clock (busy-hold + due-boundary tests)
}

// NewReservationSweeper builds a sweeper over an already-constructed
// Dispatcher, whose leg-1 claim/record seams and nav-push machinery it reuses.
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
	return &ReservationSweeper{
		dispatcher: dispatcher,
		store:      store,
		bus:        bus,
		cfg:        cfg.withDefaults(),
		logger:     logger,
		now:        time.Now,
	}
}

// withClock injects a clock for deterministic busy-hold / due-boundary tests.
func (s *ReservationSweeper) withClock(now func() time.Time) *ReservationSweeper {
	if now != nil {
		s.now = now
	}
	return s
}

// Run drives the sweep on a ticker until ctx is canceled. Intended to be run
// in its own goroutine. It returns immediately when the kill-switch is off.
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
		slog.Duration("busy_hold", s.cfg.BusyHold),
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
