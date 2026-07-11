// RideRequestRepo list read paths (MYR-173): the rider's history /
// scheduled lists (MYR-174) and the owner's incoming feed filterable by
// status (MYR-175). Ordered created_at DESC, id DESC to match the
// contracts RideRequestsListResponse envelope; cursor encoding on top of
// this ordering is the HTTP layer's concern.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// defaultRideRequestListLimit caps unbounded list calls (limit <= 0).
const defaultRideRequestListLimit = 100

// ListByRider returns up to limit ride requests created by the rider,
// newest first. limit <= 0 applies defaultRideRequestListLimit.
func (r *RideRequestRepo) ListByRider(ctx context.Context, riderID string, limit int) ([]RideRequestRecord, error) {
	recs, err := r.list(ctx, "ride_request.list_by_rider", queryRideRequestsByRider, riderID, normalizeLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("RideRequestRepo.ListByRider(%s): %w", riderID, err)
	}
	return recs, nil
}

// ListByOwner returns up to limit ride requests addressed to the owner,
// newest first, optionally filtered to one lifecycle status (nil = all —
// e.g. RideRequestStatusRequested for the incoming-request feed).
func (r *RideRequestRepo) ListByOwner(ctx context.Context, ownerID string, status *RideRequestStatus, limit int) ([]RideRequestRecord, error) {
	var (
		recs []RideRequestRecord
		err  error
	)
	if status == nil {
		recs, err = r.list(ctx, "ride_request.list_by_owner", queryRideRequestsByOwner, ownerID, normalizeLimit(limit))
	} else {
		recs, err = r.list(ctx, "ride_request.list_by_owner", queryRideRequestsByOwnerAndStatus, ownerID, string(*status), normalizeLimit(limit))
	}
	if err != nil {
		return nil, fmt.Errorf("RideRequestRepo.ListByOwner(%s): %w", ownerID, err)
	}
	return recs, nil
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return defaultRideRequestListLimit
	}
	return limit
}

// list runs one of the rideRequestColumns SELECTs and scans + decrypts
// every row. Returns an empty (non-nil) slice when nothing matches so
// callers can marshal it straight into the envelope's `items: []`.
func (r *RideRequestRepo) list(ctx context.Context, op, query string, args ...any) ([]RideRequestRecord, error) {
	start := time.Now()
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	recs := make([]RideRequestRecord, 0)
	for rows.Next() {
		rec, scanErr := r.scanRideRequest(pgx.Row(rows))
		if scanErr != nil {
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			r.metrics.IncQueryError(op)
			return nil, scanErr
		}
		recs = append(recs, rec)
	}
	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("rows: %w", err)
	}
	// Each row already carries RequesterName: scanRideRequest resolves it from
	// the query's inline requesterIdentitySelect (MYR-229) — no separate
	// lookup, so the list paths never issue a per-row or batched "User" query.
	return recs, nil
}
