// RideRequestRepo reschedule-negotiation writes (MYR-192). Split from
// ride_request_repo.go so the accessor file stays within the 300-line budget;
// both methods share the same scanRideRequest path as the other single-row
// accessors, which resolves RequesterName inline (MYR-229) with no extra
// lookup.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ProposeReschedule records the rider's proposed new pickup time and opens
// the reschedule negotiation (RescheduleStatus 'requested' — the owner is
// asked to re-confirm; MYR-192). The main Status is untouched: the design
// keeps the reservation alive while the ask is pending. Returns the
// post-update record, or ErrRideRequestNotFound.
func (r *RideRequestRepo) ProposeReschedule(ctx context.Context, id string, proposedFor time.Time) (RideRequestRecord, error) {
	start := time.Now()
	row := r.pool.QueryRow(ctx, queryRideRequestProposeReschedule, id, proposedFor)
	rec, err := r.scanRideRequest(row)
	r.metrics.ObserveQueryDuration("ride_request.propose_reschedule", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.ProposeReschedule(%s): %w", id, ErrRideRequestNotFound)
	}
	if err != nil {
		r.metrics.IncQueryError("ride_request.propose_reschedule")
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.ProposeReschedule(%s): %w", id, err)
	}
	return rec, nil
}

// ResolveReschedule closes an open reschedule negotiation. confirmed=true
// adopts the proposed time into ScheduledFor and marks the sub-state
// 'confirmed'; confirmed=false marks it 'declined' and keeps the original
// reservation. Rows without an open 'requested' negotiation don't match —
// that (or a missing id) returns ErrRideRequestNotFound.
func (r *RideRequestRepo) ResolveReschedule(ctx context.Context, id string, confirmed bool) (RideRequestRecord, error) {
	start := time.Now()
	row := r.pool.QueryRow(ctx, queryRideRequestResolveReschedule, id, confirmed)
	rec, err := r.scanRideRequest(row)
	r.metrics.ObserveQueryDuration("ride_request.resolve_reschedule", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.ResolveReschedule(%s): %w", id, ErrRideRequestNotFound)
	}
	if err != nil {
		r.metrics.IncQueryError("ride_request.resolve_reschedule")
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.ResolveReschedule(%s): %w", id, err)
	}
	return rec, nil
}
