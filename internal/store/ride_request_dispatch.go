// RideRequestRepo dispatch accessors (MYR-176). The nav-dispatch pipeline
// (internal/dispatch) reacts to an owner accept by pushing the pickup into
// the vehicle's Tesla navigation and records the outcome here. Split from
// ride_request_repo.go so the accept/decline lifecycle surface stays focused.
//
// Two writes, in order: ClaimDispatch latches the row exactly once (guard
// against a double-push on a re-delivered event), then RecordDispatchOutcome
// stamps the resolved status once the command Executor returns.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ClaimDispatch atomically claims the ride for dispatch by stamping
// dispatched_at, but ONLY when it is still NULL. It returns claimed=true when
// this call won the claim (the caller should proceed to push nav), or
// claimed=false when the row was already claimed by a prior delivery (the
// caller must skip — the exactly-once guarantee). A missing id is not an
// error here: it also yields claimed=false (nothing to dispatch).
func (r *RideRequestRepo) ClaimDispatch(ctx context.Context, id string) (bool, error) {
	start := time.Now()
	var claimedID string
	err := r.pool.QueryRow(ctx, queryRideRequestClaimDispatch, id).Scan(&claimedID)
	r.metrics.ObserveQueryDuration("ride_request.claim_dispatch", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		r.metrics.IncQueryError("ride_request.claim_dispatch")
		return false, fmt.Errorf("RideRequestRepo.ClaimDispatch(%s): %w", id, err)
	}
	return true, nil
}

// RecordDispatchOutcome persists the resolved dispatch status and, for a
// failure, the opaque error code (errCode is nil for sent/skipped). The row
// is expected to be already claimed; a missing id returns
// ErrRideRequestNotFound.
func (r *RideRequestRepo) RecordDispatchOutcome(ctx context.Context, id string, status DispatchStatus, errCode *string) error {
	start := time.Now()
	tag, err := r.pool.Exec(ctx, queryRideRequestRecordDispatch, id, string(status), errCode)
	r.metrics.ObserveQueryDuration("ride_request.record_dispatch", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("ride_request.record_dispatch")
		return fmt.Errorf("RideRequestRepo.RecordDispatchOutcome(%s): %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("RideRequestRepo.RecordDispatchOutcome(%s): %w", id, ErrRideRequestNotFound)
	}
	return nil
}

// ListInterruptedDispatches returns the ids of rides claimed for dispatch
// (dispatched_at set) whose outcome never resolved (dispatch_status NULL) and
// whose claim is older than olderThan — dispatches orphaned by a crash/SIGTERM
// in the claim→record window. The startup reconciler (internal/dispatch)
// resolves each. No match is not an error (returns an empty slice).
func (r *RideRequestRepo) ListInterruptedDispatches(ctx context.Context, olderThan time.Duration) ([]string, error) {
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryRideRequestListInterrupted, olderThan.Seconds())
	r.metrics.ObserveQueryDuration("ride_request.list_interrupted_dispatches", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("ride_request.list_interrupted_dispatches")
		return nil, fmt.Errorf("RideRequestRepo.ListInterruptedDispatches: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("RideRequestRepo.ListInterruptedDispatches scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("RideRequestRepo.ListInterruptedDispatches rows: %w", err)
	}
	return ids, nil
}

// ClaimDropoffDispatch is the leg-2 (dropoff) analogue of ClaimDispatch
// (MYR-265): it stamps dropoff_dispatched_at only when still NULL, returning
// claimed=true when this call won the exactly-once claim or claimed=false when
// a prior delivery already claimed it (the caller must skip). A missing id
// yields claimed=false (nothing to dispatch), not an error.
func (r *RideRequestRepo) ClaimDropoffDispatch(ctx context.Context, id string) (bool, error) {
	start := time.Now()
	var claimedID string
	err := r.pool.QueryRow(ctx, queryRideRequestClaimDropoffDispatch, id).Scan(&claimedID)
	r.metrics.ObserveQueryDuration("ride_request.claim_dropoff_dispatch", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		r.metrics.IncQueryError("ride_request.claim_dropoff_dispatch")
		return false, fmt.Errorf("RideRequestRepo.ClaimDropoffDispatch(%s): %w", id, err)
	}
	return true, nil
}

// RecordDropoffDispatchOutcome persists the resolved leg-2 dispatch status and,
// for a failure, the opaque error code (errCode is nil for sent/skipped). The
// row is expected already claimed; a missing id returns ErrRideRequestNotFound.
func (r *RideRequestRepo) RecordDropoffDispatchOutcome(ctx context.Context, id string, status DispatchStatus, errCode *string) error {
	start := time.Now()
	tag, err := r.pool.Exec(ctx, queryRideRequestRecordDropoffDispatch, id, string(status), errCode)
	r.metrics.ObserveQueryDuration("ride_request.record_dropoff_dispatch", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("ride_request.record_dropoff_dispatch")
		return fmt.Errorf("RideRequestRepo.RecordDropoffDispatchOutcome(%s): %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("RideRequestRepo.RecordDropoffDispatchOutcome(%s): %w", id, ErrRideRequestNotFound)
	}
	return nil
}

// ListInterruptedDropoffDispatches is the leg-2 (dropoff) analogue of
// ListInterruptedDispatches (MYR-266): it returns the ids of rides claimed for
// the dropoff push (dropoff_dispatched_at set) whose outcome never resolved
// (dropoff_dispatch_status NULL) and whose claim is older than olderThan —
// dropoff dispatches orphaned by a crash/SIGTERM in the claim→record window.
// The leg-2 startup reconciler (internal/dispatch) resolves each. A dropoff
// that already resolved (sent/failed/skipped) has a non-NULL status and is
// excluded, so a car that already got its dropoff nav is never re-touched. No
// match is not an error (returns an empty slice).
func (r *RideRequestRepo) ListInterruptedDropoffDispatches(ctx context.Context, olderThan time.Duration) ([]string, error) {
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryRideRequestListInterruptedDropoff, olderThan.Seconds())
	r.metrics.ObserveQueryDuration("ride_request.list_interrupted_dropoff_dispatches", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("ride_request.list_interrupted_dropoff_dispatches")
		return nil, fmt.Errorf("RideRequestRepo.ListInterruptedDropoffDispatches: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("RideRequestRepo.ListInterruptedDropoffDispatches scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("RideRequestRepo.ListInterruptedDropoffDispatches rows: %w", err)
	}
	return ids, nil
}
