package telemetry

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// The `upcomingForVehicle` slice of the owner incoming feed (MYR-360).
//
// WHY A QUERY PARAM AND NOT A NEW ENDPOINT. The question the client asks
// before an owner pauses ride sharing — "which confirmed reservations would
// this break?" — is a slice of the SAME owner-scoped feed, not a new resource.
// The existing feed could not answer it: `ServeIncoming` hardcodes status
// `requested` (decided rows leave the feed by construction, rest-api.md §7.8)
// and has no vehicle filter at all. So the param selects a different slice of
// one feed rather than growing a second endpoint family.
//
// WHAT THE SLICE IS. Same owner (`owner_id` = JWT sub — unchanged, so no
// cross-owner read is expressible), plus `vehicle_id = {vehicleId}`,
// `status = 'accepted'`, and a STRICTLY future `scheduled_for`. Ordered
// `scheduled_for ASC, id ASC` — SOONEST FIRST.
//
// THE ORDERING IS LOAD-BEARING, not cosmetic. It is a deliberate departure
// from the feed's default `created_at DESC, id DESC`: the client dialog names
// "the NEXT reservation", and under `created_at DESC` a paginated cut could
// omit the soonest one entirely (a reservation booked long ago for tomorrow
// sorts last). The cursor therefore anchors on `(scheduledFor, id)` and walks
// ascending, in the SAME opaque wire format as every other ride cursor — see
// ride_request_cursor.go.
//
// NO EXISTENCE ORACLE. The vehicle filter runs on the owner's OWN feed, so it
// needs no separate ownership check to be safe. But it must not become a way to
// probe for other people's cars either: an unknown or unowned vehicleId returns
// an EMPTY 200 page, never a 403/404 that would confirm the vehicle exists.
// Only a MALFORMED value is a 400, exactly like `limit`/`cursor`.

// queryUpcomingForVehicle is the query-parameter name that selects the slice.
const queryUpcomingForVehicle = "upcomingForVehicle"

// maxVehicleIDLen bounds the accepted vehicle id. Vehicle ids are cuids (~25
// characters); the cap exists so a pathological query string is rejected as
// malformed at the edge rather than travelling into a parameterised query.
const maxVehicleIDLen = 64

// serveUpcomingForVehicle serves the `upcomingForVehicle` slice. The caller has
// already authenticated; userID is the JWT subject and is the ONLY owner
// scoping applied — the query string cannot widen it.
func (h *RideRequestHandler) serveUpcomingForVehicle(w http.ResponseWriter, r *http.Request, userID string) {
	vehicleID, ok := h.parseUpcomingVehicle(w, r)
	if !ok {
		return
	}

	limit, ok := h.parseListLimit(w, r)
	if !ok {
		return
	}

	var cursor RideRequestUpcomingCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeRideUpcomingCursor(raw)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed cursor")
			return
		}
		cursor = c
	}

	page, err := h.store.ListUpcomingByOwnerVehiclePage(r.Context(), userID, vehicleID, cursor, limit)
	if err != nil {
		h.logger.Error("ride-request upcoming: store failed",
			slog.String("user_id", userID),
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.writeJSON(w, http.StatusOK, buildUpcomingRidePage(page))
}

// parseUpcomingVehicle validates the `upcomingForVehicle` value. Empty,
// whitespace-only or oversized is 400 invalid_request — the same class as a
// bad `limit`/`cursor`. An id that is well-formed but unknown or owned by
// somebody else is NOT rejected here: it flows into the query and yields an
// empty page, so this endpoint never confirms whether a vehicle exists.
func (h *RideRequestHandler) parseUpcomingVehicle(w http.ResponseWriter, r *http.Request) (string, bool) {
	vehicleID := strings.TrimSpace(r.URL.Query().Get(queryUpcomingForVehicle))
	if vehicleID == "" || len(vehicleID) > maxVehicleIDLen {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			queryUpcomingForVehicle+" must be a vehicle id")
		return "", false
	}
	return vehicleID, true
}

// buildUpcomingRidePage projects the slice into the unchanged
// RideRequestsListResponse envelope, anchoring nextCursor on the reservation
// instant rather than createdAt — the column this view is ordered by.
func buildUpcomingRidePage(page RideRequestListPage) rideRequestsPageResponse {
	return buildRidePageAnchoredBy(page, func(rec RideRequestData) (anchor cursorAnchor) {
		if rec.ScheduledFor != nil {
			return cursorAnchor{At: *rec.ScheduledFor, ID: rec.ID}
		}
		// Unreachable through the query (scheduled_for IS NOT NULL is in the
		// predicate); falling back to createdAt keeps the cursor decodable
		// rather than emitting a zero instant if that ever changed.
		return cursorAnchor{At: rec.CreatedAt, ID: rec.ID}
	})
}
