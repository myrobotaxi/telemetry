package telemetry

import (
	"log/slog"
	"net/http"
	"strconv"

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
	h.writeJSON(w, http.StatusOK, toRideRequestWire(rec))
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

	h.writeJSON(w, http.StatusOK, buildRidePage(page))
}

// parseListQuery validates the shared `limit` and `cursor` query params
// (rest-api.md §4.2.1). Returns ok=false after writing a 400 on bad input.
func (h *RideRequestHandler) parseListQuery(w http.ResponseWriter, r *http.Request) (int, RideRequestListCursor, bool) {
	limit := rideListDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < rideListMinLimit || n > rideListMaxLimit {
			h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "limit must be an integer in [1, 100]")
			return 0, RideRequestListCursor{}, false
		}
		limit = n
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

// buildRidePage projects a store page into the wire envelope, computing
// nextCursor from the last item when more pages remain. Items is always a
// non-nil slice so it marshals to `[]`, never `null`.
func buildRidePage(page RideRequestListPage) rideRequestsPageResponse {
	items := make([]rideRequestWire, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, toRideRequestWire(page.Items[i]))
	}

	var nextCursor *string
	if page.HasMore && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		encoded := encodeRideCursor(last.CreatedAt, last.ID)
		nextCursor = &encoded
	}

	return rideRequestsPageResponse{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    page.HasMore,
	}
}
