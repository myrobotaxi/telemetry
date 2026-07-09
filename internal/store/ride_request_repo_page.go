// RideRequestRepo keyset-paginated read paths (MYR-174/175). The simple
// capped ListByRider/ListByOwner (ride_request_repo_list.go) return a bare
// slice for internal callers; these Page variants add the opaque-cursor
// contract the REST list endpoints expose (RideRequestsListResponse) —
// limit+1 probe for hasMore, resume-after-(created_at,id). Kept in a
// separate file so the cursor machinery stays adjacent to its queries and
// the list file remains the minimal-surface capped read.

package store

import (
	"context"
	"fmt"
	"time"
)

// RideRequestListCursor is the (createdAt, id) anchor pair the HTTP layer
// encodes into the opaque cursor. The zero value means "first page" — the
// repo treats an empty ID as "no anchor" and runs the un-anchored query.
type RideRequestListCursor struct {
	CreatedAt time.Time
	ID        string
}

// IsZero reports whether the cursor carries no anchor (first page).
func (c RideRequestListCursor) IsZero() bool {
	return c.ID == "" && c.CreatedAt.IsZero()
}

// RideRequestListPage is one page of a keyset scan. HasMore is true when the
// limit+1 probe returned an extra row; Items is trimmed back to limit.
type RideRequestListPage struct {
	Items   []RideRequestRecord
	HasMore bool
}

// ListByRiderPage returns a page of the rider's own requests, newest first,
// resuming after cursor. limit <= 0 applies defaultRideRequestListLimit.
func (r *RideRequestRepo) ListByRiderPage(ctx context.Context, riderID string, cursor RideRequestListCursor, limit int) (RideRequestListPage, error) {
	limit = normalizeLimit(limit)
	var (
		recs []RideRequestRecord
		err  error
	)
	if cursor.IsZero() {
		recs, err = r.list(ctx, "ride_request.list_by_rider", queryRideRequestsByRider, riderID, limit+1)
	} else {
		recs, err = r.list(ctx, "ride_request.list_by_rider", queryRideRequestsByRiderCursor, riderID, cursor.CreatedAt, cursor.ID, limit+1)
	}
	if err != nil {
		return RideRequestListPage{}, fmt.Errorf("RideRequestRepo.ListByRiderPage(%s): %w", riderID, err)
	}
	return trimPage(recs, limit), nil
}

// ListByOwnerPage returns a page of requests addressed to the owner, newest
// first, resuming after cursor, optionally filtered to one lifecycle status
// (nil = all — the incoming feed passes RideRequestStatusRequested and its
// scheduled variants). limit <= 0 applies defaultRideRequestListLimit.
func (r *RideRequestRepo) ListByOwnerPage(ctx context.Context, ownerID string, status *RideRequestStatus, cursor RideRequestListCursor, limit int) (RideRequestListPage, error) {
	limit = normalizeLimit(limit)
	recs, err := r.listByOwnerPage(ctx, ownerID, status, cursor, limit+1)
	if err != nil {
		return RideRequestListPage{}, fmt.Errorf("RideRequestRepo.ListByOwnerPage(%s): %w", ownerID, err)
	}
	return trimPage(recs, limit), nil
}

// listByOwnerPage picks the owner query variant across the status-filter ×
// cursor matrix and runs it with the given probe size.
func (r *RideRequestRepo) listByOwnerPage(ctx context.Context, ownerID string, status *RideRequestStatus, cursor RideRequestListCursor, probe int) ([]RideRequestRecord, error) {
	const op = "ride_request.list_by_owner"
	switch {
	case status == nil && cursor.IsZero():
		return r.list(ctx, op, queryRideRequestsByOwner, ownerID, probe)
	case status == nil:
		return r.list(ctx, op, queryRideRequestsByOwnerCursor, ownerID, cursor.CreatedAt, cursor.ID, probe)
	case cursor.IsZero():
		return r.list(ctx, op, queryRideRequestsByOwnerAndStatus, ownerID, string(*status), probe)
	default:
		return r.list(ctx, op, queryRideRequestsByOwnerAndStatusCursor, ownerID, string(*status), cursor.CreatedAt, cursor.ID, probe)
	}
}

// trimPage converts a limit+1 probe result into a page: HasMore is true when
// the extra row came back, and Items is sliced back to the requested limit.
func trimPage(recs []RideRequestRecord, limit int) RideRequestListPage {
	hasMore := len(recs) > limit
	if hasMore {
		recs = recs[:limit]
	}
	return RideRequestListPage{Items: recs, HasMore: hasMore}
}
