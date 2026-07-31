// The per-vehicle ride-window conflict gate (MYR-383): a vehicle cannot
// promise two rides in one window.
//
// THE BOUNDARY THIS CLOSES. Reservation<->reservation collision was a
// DOCUMENTED v1 boundary (MYR-179, rest-api.md §7.8 "v1 boundaries"): the
// dispatch-side busy predicate deliberately excludes scheduled rides, so two
// reservations for one car at overlapping times both dispatched, and nothing
// upstream refused the second booking either. In production (TestFlight r14) a
// rider booked a second reservation into a window an ACCEPTED reservation
// already held; nothing refused it and the collision surfaced only when the
// owner declined it by hand minutes later. The fix belongs at BOOKING time,
// which is what this is.
//
// RACE CONSTRUCTION — why a lock and not an index. Every other guard on this
// table is a partial UNIQUE index (0004 per-rider, 0013 per-vehicle), and an
// exclusion constraint over a tsrange with btree_gist was the obvious next
// reach. It does not fit, for three independent reasons:
//
//  1. HALF THE PREDICATE IS RELATIVE TO NOW(). An active INSTANT ride occupies
//     a window around the CURRENT time, which no index expression can hold —
//     index predicates must be IMMUTABLE. An EXCLUDE constraint could therefore
//     only cover the reservation<->reservation arm, leaving the instant arm to
//     be checked some other way: two mechanisms for one rule, which is exactly
//     the drift this package keeps eliminating (see activeInstantRidePredicate).
//  2. IT WOULD NEED A DESTRUCTIVE PRE-DEDUP. A constraint cannot be created
//     over data that violates it, so the migration would have to CANCEL real,
//     live, already-accepted production reservations to install itself — the
//     0004/0013 dedup pattern applied to rows a human is expecting a car for.
//  3. btree_gist is an EXTENSION. `CREATE EXTENSION` from an application
//     migration role is a deploy-time gamble this gate does not need to take.
//
// So the arbitration is a per-vehicle ADVISORY TRANSACTION LOCK wrapped around
// check-and-write. It gives the same guarantee the indexes do — the DATABASE
// decides, not a pre-check read — with the predicate expressed once, in SQL
// both landing sites share:
//
//	BEGIN; pg_advisory_xact_lock(<vehicle>); <conflict probe>; <insert|update>; COMMIT
//
// Two conflicting bookings for one car serialize on the lock; the second's
// probe runs after the first COMMIT (READ COMMITTED takes a fresh snapshot per
// statement) and sees it. Bookings for DIFFERENT cars never contend. The lock
// is released by COMMIT/ROLLBACK — including on a panic or a dropped
// connection — so there is no leak path and no unlock to forget.
//
// WHAT IS NOT LOCKED, deliberately: an INSTANT create. It inserts at status
// `requested`, which is outside the active-instant arm, so it cannot add to any
// window; its own guards (0004, 0013, MYR-277) are unchanged. Every mutation
// that CAN add to a window — a reservation insert, and any accept — takes the
// lock.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RideConflictWindow is the MINIMUM SEPARATION between two rides on ONE
// vehicle. A booking is refused when another open ride on the same car sits
// STRICTLY closer than this to it; exactly this far apart is allowed.
//
// 45 minutes is a PRODUCT GUESS, not a derived quantity — roughly "a ride plus
// the drive to the next pickup" for the single-city fleet v1 serves. It is
// deliberately the only place the number appears: the SQL takes it as a bind
// parameter rather than embedding an interval literal, and no client, contract
// enum, or migration encodes it, so changing this line changes the rule
// everywhere. Widening it refuses more bookings and strands nobody; narrowing
// it never invalidates a booking already taken.
const RideConflictWindow = 45 * time.Minute

// RideWindowConflictError is returned by the booking paths when the target
// vehicle is already promised to another open ride inside RideConflictWindow of
// the requested instant (MYR-383). It wraps ErrRideWindowConflict, so callers
// match with errors.Is and read the detail with errors.As.
//
// ConflictAt is the conflicting ride's instant — its `scheduledFor` — or NIL
// when the conflict is an ACTIVE INSTANT ride, which has no scheduled instant
// to name (it is happening now). Pending reports whether that claim is a
// still-undecided `requested` reservation rather than a committed one, so the
// caller can say "already requested" instead of the untrue "already booked".
//
// Those two are the ONLY things carried out of the probe: the other ride's id,
// rider, pickup and dropoff are somebody else's business (P1,
// data-classification.md §1.9), while the instant and the pending flag are P0
// operational timing, safe to echo back to the caller and to log.
type RideWindowConflictError struct {
	ConflictAt *time.Time
	Pending    bool
}

func (e *RideWindowConflictError) Error() string {
	switch {
	case e.ConflictAt == nil:
		return "vehicle is already on a ride within the booking window"
	case e.Pending:
		return fmt.Sprintf("vehicle already has a ride request for %s", e.ConflictAt.UTC().Format(time.RFC3339))
	default:
		return fmt.Sprintf("vehicle is already booked at %s", e.ConflictAt.UTC().Format(time.RFC3339))
	}
}

// Unwrap makes every RideWindowConflictError match errors.Is(err,
// ErrRideWindowConflict), so a caller that does not need the instant does not
// have to know the concrete type exists.
func (e *RideWindowConflictError) Unwrap() error { return ErrRideWindowConflict }

// rideConflictTx is the transaction surface the gate needs: a lock, a probe,
// and the write they protect. pgx.Tx satisfies it. Taking the interface (rather
// than pgx.Tx) keeps every helper here unable to commit, roll back, or reach
// the pool — the only two callers that own a transaction are in this package.
type rideConflictTx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// lockVehicleForBooking takes the per-vehicle booking lock for the rest of tx,
// keyed by vehicle id. Used by the reservation INSERT, which knows its vehicle
// before it knows anything else.
func lockVehicleForBooking(ctx context.Context, tx rideConflictTx, vehicleID string) error {
	if _, err := tx.Exec(ctx, queryRideVehicleLock, rideVehicleLockNamespace, vehicleID); err != nil {
		return fmt.Errorf("store: lock vehicle booking window (vehicle=%s): %w", vehicleID, err)
	}
	return nil
}

// lockVehicleForRide takes the same lock keyed by RIDE id — it locks whichever
// vehicle the ride targets. Used by the accept path, which starts from a ride.
// An unknown ride locks nothing and is NOT an error: the guarded write that
// follows is what reports a missing row, and inventing a second not-found path
// here would only give the two ways to disagree.
func lockVehicleForRide(ctx context.Context, tx rideConflictTx, rideID string) error {
	if _, err := tx.Exec(ctx, queryRideVehicleLockByRide, rideVehicleLockNamespace, rideID); err != nil {
		return fmt.Errorf("store: lock vehicle booking window (ride=%s): %w", rideID, err)
	}
	return nil
}

// readRideBooking reads what a ride is promising — which vehicle, and for when
// — from INSIDE the lock, so a reschedule that moved the instant a moment
// earlier is already visible. scheduledFor is nil for an instant ride. An
// unknown id returns ErrRideRequestNotFound.
func readRideBooking(ctx context.Context, tx rideConflictTx, rideID string) (string, *time.Time, error) {
	var vehicleID string
	var scheduledFor *time.Time
	err := tx.QueryRow(ctx, queryRideRequestBooking, rideID).Scan(&vehicleID, &scheduledFor)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, fmt.Errorf("store: read ride booking(%s): %w", rideID, ErrRideRequestNotFound)
	}
	if err != nil {
		return "", nil, fmt.Errorf("store: read ride booking(%s): %w", rideID, err)
	}
	return vehicleID, scheduledFor, nil
}

// checkRideWindowConflict is THE conflict check — the one implementation both
// landing sites call, running the one predicate both share
// (rideWindowConflictPredicate). It answers the question "is this vehicle
// already promised to somebody within RideConflictWindow of `at`?" and returns
// a *RideWindowConflictError naming the conflicting instant when it is.
//
// excludeID is the ride being accepted, so a reservation cannot conflict with
// itself; create passes "" (which matches no id). countPending decides whether
// a still-undecided `requested` reservation counts as occupying the window —
// TRUE for create, FALSE for accept; the full argument is on
// rideWindowConflictPredicate.
//
// MUST be called with the vehicle's booking lock already held — without it this
// is a bare pre-check read and two concurrent bookings can both pass it.
func checkRideWindowConflict(
	ctx context.Context, tx rideConflictTx, vehicleID string, at time.Time, excludeID string, countPending bool,
) error {
	var conflictAt *time.Time
	var pending bool
	err := tx.QueryRow(ctx, queryRideWindowConflict,
		vehicleID, at, excludeID, RideConflictWindow.Seconds(), countPending,
	).Scan(&conflictAt, &pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // the window is free
	}
	if err != nil {
		return fmt.Errorf("store: ride-window conflict probe (vehicle=%s): %w", vehicleID, err)
	}
	return &RideWindowConflictError{ConflictAt: conflictAt, Pending: pending}
}

// createReservationGuarded inserts a RESERVATION under the per-vehicle booking
// lock, refusing it when the window is already taken (MYR-383). The lock, the
// probe and the INSERT are one transaction, so two concurrent conflicting
// creates cannot both commit: the loser's probe runs after the winner's insert
// is visible and returns *RideWindowConflictError.
//
// Only reservations come through here. An instant create takes the plain
// single-statement path in Create — it cannot occupy a window at `requested`,
// so locking it would buy nothing and slow the hot path.
func (r *RideRequestRepo) createReservationGuarded(ctx context.Context, req *RideRequestRecord, c ridePlaceCipher) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("RideRequestRepo.Create(%s): begin: %w", req.ID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	if err := lockVehicleForBooking(ctx, tx, req.VehicleID); err != nil {
		return err
	}
	// countPending TRUE: a rider must not be handed a booking that collides
	// with somebody's still-undecided request for the same slot.
	if err := checkRideWindowConflict(ctx, tx, req.VehicleID, *req.ScheduledFor, "", true); err != nil {
		return err
	}
	if err := r.insertRideRequest(ctx, tx, req, c); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("RideRequestRepo.Create(%s): commit: %w", req.ID, err)
	}
	return nil
}
