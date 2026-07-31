// The GUARDED ride-request transitions (MYR-174/175 race fix, MYR-376
// reservation-dormancy gate). Split out of ride_request_repo.go so the two
// variants — and the single miss-classifier they share — sit together: they
// differ only in their WHERE clause, and the whole safety argument is that
// every transition decision happens INSIDE one UPDATE.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// UpdateStatusFrom is the GUARDED transition (MYR-174/175 race fix): it
// persists the same stamped status change as UpdateStatus, but only when
// the row's CURRENT status is in `from` — a single-statement
// `WHERE id = $1 AND status = ANY($3)` so concurrent conflicting mutations
// serialize in the database and exactly one wins. The HTTP handlers use
// this for every lifecycle transition; only the winning call may publish
// the WS / dispatch events (exactly-once ride.accepted seam).
//
// On a zero-row match the miss is disambiguated with a follow-up read (the
// guard's correctness does not depend on it): an unknown id returns
// ErrRideRequestNotFound, an existing row whose status is outside `from`
// returns ErrRideRequestConflict.
func (r *RideRequestRepo) UpdateStatusFrom(ctx context.Context, id string, from []RideRequestStatus, to RideRequestStatus) (RideRequestRecord, error) {
	return r.updateStatusGuarded(ctx, statusGuard{
		name:  "UpdateStatusFrom",
		op:    "ride_request.update_status_from",
		query: queryRideRequestUpdateStatusFrom,
	}, id, from, to)
}

// UpdateStatusFromDispatched is UpdateStatusFrom plus the MYR-376 RESERVATION
// DORMANCY predicate, and it backs exactly one caller: the owner pickup
// transition (accepted → arrived).
//
// A reservation is DORMANT between accept and its DUE INSTANT. The MYR-179
// sweeper is what normally moves it into its live phase — at due time it claims,
// pushes the leg-1 pickup nav, records dispatch_status='sent' and publishes
// `ride.due`. Before due, the car has been told nothing, and in production an
// owner could still tap "Picked up" a full day early, stranding the ride at
// `arrived` where the rider's `start` would push a real dropoff nav to a vehicle
// that was in service.
//
// DORMANCY IS TIME-BOUNDED, NOT DISPATCH-BOUNDED. It ends at whichever comes
// first: dispatch resolving 'sent', or `scheduled_for` arriving. §7.8's
// reservation-expiry contract promises that a reservation the sweeper FAILED
// (`reservation_expired`) stays `accepted` and its parties "may still cancel or
// proceed manually" — a predicate demanding 'sent' would strand every such ride
// with cancel as its only exit. Past due, a manual pickup IS the documented
// recovery path. The defect being closed is strictly the PRE-DUE pickup.
//
// The refusal is expressed in SQL, not in a pre-check: the dormancy predicate
// rides the SAME UPDATE as the status guard, so a sweeper dispatching
// concurrently with a pickup tap is arbitrated by the database exactly like two
// racing transitions. No pickup can commit against a row that is dormant at the
// instant of the write, no matter how stale the caller's read was.
//
// INSTANT RIDES ARE UNAFFECTED — `scheduled_for IS NULL` satisfies the
// predicate outright, including when their nav push failed or was skipped. The
// owner drives the car either way; a failed notification must not break a
// working ride.
//
// A zero-row miss is classified exactly like UpdateStatusFrom's, with one extra
// outcome: a row still inside the allowed-from set that is a dormant
// reservation returns ErrRideRequestReservationDormant, so the HTTP layer can
// name the actual reason in its 409 rather than blaming the status.
func (r *RideRequestRepo) UpdateStatusFromDispatched(ctx context.Context, id string, from []RideRequestStatus, to RideRequestStatus) (RideRequestRecord, error) {
	return r.updateStatusGuarded(ctx, statusGuard{
		name:            "UpdateStatusFromDispatched",
		op:              "ride_request.update_status_from_dispatched",
		query:           queryRideRequestUpdateStatusFromDispatched,
		dormancyGuarded: true,
	}, id, from, to)
}

// statusGuard names one guarded-transition statement: the SQL to run, the
// metrics label to record it under, and the method name that appears in wrapped
// errors. Each variant supplies its own; everything else about the write is
// identical, which is the point.
type statusGuard struct {
	name  string
	op    string
	query string
	// dormancyGuarded marks the variant whose WHERE also carries the MYR-376
	// reservation-dormancy predicate. It changes NOTHING about the write — only
	// how a zero-row miss is explained to the caller.
	dormancyGuarded bool
}

// updateStatusGuarded runs one guarded transition statement and classifies its
// outcome. Shared by UpdateStatusFrom and UpdateStatusFromDispatched so the two
// can never drift in how they stamp a row or map a miss.
func (r *RideRequestRepo) updateStatusGuarded(
	ctx context.Context, g statusGuard, id string, from []RideRequestStatus, to RideRequestStatus,
) (RideRequestRecord, error) {
	fromStrs := make([]string, 0, len(from))
	for _, s := range from {
		fromStrs = append(fromStrs, string(s))
	}

	start := time.Now()
	row := r.pool.QueryRow(ctx, g.query, id, string(to), fromStrs)
	rec, err := r.scanRideRequest(row)
	r.metrics.ObserveQueryDuration(g.op, time.Since(start).Seconds())
	if err == nil {
		return rec, nil
	}
	// The per-vehicle one-active-ride guard (0013) losing the race is an
	// expected outcome, not a query fault — surface the typed sentinel without
	// incrementing the error counter (mirrors the Create active-ride path). This
	// fires on the guarded requested->accepted UPDATE when another active
	// instant ride already holds the target vehicle (MYR-266).
	if isVehicleRideActiveViolation(err) {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.%s(%s): %w", g.name, id, ErrVehicleRideActive)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		r.metrics.IncQueryError(g.op)
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.%s(%s): %w", g.name, id, err)
	}
	return RideRequestRecord{}, r.explainGuardMiss(ctx, g, id, from, to)
}

// explainGuardMiss disambiguates a zero-row guarded write. PURELY DIAGNOSTIC:
// whatever the follow-up read observes, the guarded write has already refused
// the transition — this only decides which sentinel (and therefore which 409
// message) the caller gets. An unknown id is ErrRideRequestNotFound; a row whose
// status left the allowed-from set is ErrRideRequestConflict; and, for the
// dormancy-guarded variant only, a still-in-set RESERVATION is
// ErrRideRequestReservationDormant.
//
// WHY THE DORMANCY ARM RE-READS NEITHER dispatch_status NOR THE CLOCK. The
// write's own view is the only authority, and this read happens after it. Both
// of the dormancy predicate's escape hatches are things that can only ARRIVE in
// the gap, never depart: a sweeper can commit dispatch_status='sent', and
// `NOW()` can cross `scheduled_for`. Either would make a row that WAS dormant at
// the instant of the write look live here, and re-evaluating the predicate would
// report the refusal as a status conflict — a message blaming a status that is
// in fact still legal. (Observed under the pickup-vs-sweeper race test, every
// few rounds; the clock arm makes the same misreport reachable for any pickup
// tapped in the last moments before due.) So the classification is DERIVED from
// what the miss itself proves instead:
//
//	the row exists, its status IS in the allowed-from set, and the write still
//	matched nothing  ⇒  the other conjunct — dormancy — is what refused it.
//
// That derivation needs exactly one fact about time, and it is the safe
// direction: this read happens AFTER the write, and the lifecycle is monotonic,
// so a status still in the from-set now was necessarily in it at write time —
// hence the status conjunct held and dormancy is what did not. Nothing here
// depends on the row still being dormant when it is read; a caller retrying past
// due will simply be allowed through, which is the contract.
//
// `scheduled_for IS NOT NULL` is immutable for a ride's lifetime (the reschedule
// negotiation moves a reservation's instant, never its existence — rest-api.md
// §7.8), so an instant ride can never reach this arm. Both 409 causes carry the
// same wire code regardless; this only decides which reason the owner is shown.
//
// Status is checked BEFORE dormancy on purpose: when both are true, "this ride
// is no longer in a state that can be picked up" is the more useful answer.
func (r *RideRequestRepo) explainGuardMiss(
	ctx context.Context, g statusGuard, id string, from []RideRequestStatus, to RideRequestStatus,
) error {
	rec, getErr := r.GetByID(ctx, id)
	if getErr != nil {
		if errors.Is(getErr, ErrRideRequestNotFound) {
			return fmt.Errorf("RideRequestRepo.%s(%s): %w", g.name, id, ErrRideRequestNotFound)
		}
		return fmt.Errorf("RideRequestRepo.%s(%s): disambiguate miss: %w", g.name, id, getErr)
	}
	if g.dormancyGuarded && rec.ScheduledFor != nil && containsRideStatus(from, rec.Status) {
		return fmt.Errorf("RideRequestRepo.%s(%s): %w", g.name, id, ErrRideRequestReservationDormant)
	}
	return fmt.Errorf("RideRequestRepo.%s(%s -> %s): %w", g.name, id, to, ErrRideRequestConflict)
}

// containsRideStatus reports whether status is a member of set.
func containsRideStatus(set []RideRequestStatus, status RideRequestStatus) bool {
	for _, s := range set {
		if s == status {
			return true
		}
	}
	return false
}
