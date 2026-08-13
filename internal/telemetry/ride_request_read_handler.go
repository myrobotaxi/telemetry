package telemetry

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// Ride-request read paths (MYR-174): GET /api/ride-requests/{id} (party-only
// detail) and GET /api/ride-requests (the rider's own list). The list query
// parsing + envelope building are free functions so the owner incoming feed
// (MYR-175) reuses them.

// ServeGet handles GET /api/ride-requests/{id}. Party-only (rider or vehicle
// owner); non-parties get a 404 (no existence leak).
func (h *RideRequestHandler) ServeGet(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}
	rec, ok := h.loadForParty(r.Context(), w, r, userID)
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, h.rideWire(rec))
}

// ServeList handles GET /api/ride-requests — the authenticated rider's own
// requests, newest first, cursor-paginated (RideRequestsListResponse).
func (h *RideRequestHandler) ServeList(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	limit, cursor, ok := h.parseListQuery(w, r)
	if !ok {
		return
	}

	page, err := h.store.ListByRiderPage(r.Context(), userID, cursor, limit)
	if err != nil {
		h.logger.Error("ride-request list: store failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.writeJSON(w, http.StatusOK, h.buildRidePage(page))
}

// parseListQuery validates the shared `limit` and `cursor` query params
// (rest-api.md §4.2.1). Returns ok=false after writing a 400 on bad input.
func (h *RideRequestHandler) parseListQuery(w http.ResponseWriter, r *http.Request) (int, RideRequestListCursor, bool) {
	limit, ok := h.parseListLimit(w, r)
	if !ok {
		return 0, RideRequestListCursor{}, false
	}

	var cursor RideRequestListCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeRideCursor(raw)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed cursor")
			return 0, RideRequestListCursor{}, false
		}
		cursor = c
	}

	return limit, cursor, true
}

// parseListLimit validates the shared `limit` param. Split out of
// parseListQuery so the upcoming-reservations slice (MYR-360), whose cursor is
// a different typed anchor, reuses the identical bounds and identical 400.
func (h *RideRequestHandler) parseListLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return rideListDefaultLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < rideListMinLimit || n > rideListMaxLimit {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "limit must be an integer in [1, 100]")
		return 0, false
	}
	return n, true
}

// cursorAnchor is the (timestamp, id) pair a page's nextCursor is built from.
// Which timestamp it names depends on the view's ordering — see
// ride_request_cursor.go.
type cursorAnchor struct {
	At time.Time
	ID string
}

// buildRidePage projects a store page into the wire envelope, computing
// nextCursor from the last item's (createdAt, id) when more pages remain —
// the default `createdAt DESC` ordering of the rider list and the owner
// incoming feed.
func (h *RideRequestHandler) buildRidePage(page RideRequestListPage) rideRequestsPageResponse {
	return h.buildRidePageAnchoredBy(page, func(rec RideRequestData) cursorAnchor {
		return cursorAnchor{At: rec.CreatedAt, ID: rec.ID}
	})
}

// buildRidePageAnchoredBy projects a store page into the wire envelope,
// deriving nextCursor from the last item via `anchorOf`. Items is always a
// non-nil slice so it marshals to `[]`, never `null`.
func (h *RideRequestHandler) buildRidePageAnchoredBy(page RideRequestListPage, anchorOf func(RideRequestData) cursorAnchor) rideRequestsPageResponse {
	items := make([]rideRequestWire, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, h.rideWire(page.Items[i]))
	}

	var nextCursor *string
	if page.HasMore && len(page.Items) > 0 {
		anchor := anchorOf(page.Items[len(page.Items)-1])
		encoded := encodeRideCursor(anchor.At, anchor.ID)
		nextCursor = &encoded
	}

	return rideRequestsPageResponse{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    page.HasMore,
	}
}
