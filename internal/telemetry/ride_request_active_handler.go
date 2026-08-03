package telemetry

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// The `activeForVehicle` slice of the owner incoming feed (MYR-400).
//
// THE GAP IT CLOSES. Until now no owner-scoped read could answer "WHAT RIDE IS
// MY CAR SERVING RIGHT NOW?". §7.8's two owner surfaces are both about rides
// that are NOT live: the default feed is status `requested` (decided rows leave
// it by construction) and `upcomingForVehicle` is `accepted` AND STRICTLY
// FUTURE. A ride in flight fell between them. iOS worked around it with a
// DEVICE-PERSISTED ride-id pointer plus the party-only
// `GET /api/ride-requests/{id}` (MYR-396) — but a pointer only remembers what
// THAT device did, so a ride accepted on a second device, or before a
// reinstall, was unrecoverable, and a `ride_status_changed` frame about a ride
// the client did not hold had to be DROPPED because there was no way to fetch
// it. Both are one owner-scoped read away, and this is that read.
//
// WHY A QUERY PARAM AND NOT A NEW ENDPOINT — the MYR-360 argument, unchanged.
// This is a slice of the SAME owner-scoped feed, not a new resource, so it
// follows the grammar `upcomingForVehicle` established rather than growing a
// second endpoint family. The two params are siblings and complementary: a
// future reservation is in `upcoming` and NOT in `active`; the same reservation
// is in `active` once it goes live. They are NOT disjoint by construction — the
// overlap set (a dispatched reservation whose instant is still future) is empty
// today only because of how the sweeper claims, and becomes reachable when
// reschedule ships. store/ride_request_active.go states the set and the
// MYR-192 requirement that closes it.
//
// WHAT THE SLICE IS. Same owner (`owner_id` = JWT sub — unchanged, so no
// cross-owner read is expressible), plus `vehicle_id = {vehicleId}` and the
// LIVE-RIDE predicate: a committed status (`accepted`/`arrived`/`enroute` — the
// set the per-vehicle one-active-ride index covers) minus a reservation that is
// still DORMANT (store.rideActivePredicate, which reuses MYR-376's dormancy
// definition verbatim rather than restating it). Ordered `created_at DESC,
// id DESC` — the feed's DEFAULT — so this slice reuses the ordinary descending
// (createdAt, id) cursor and adds no new cursor type.
//
// NO EXISTENCE ORACLE — identical to `upcomingForVehicle`. The vehicle filter
// runs on the owner's OWN feed, so an unknown or unowned vehicleId returns an
// EMPTY 200 page, never a 403/404 that would confirm the vehicle exists. Only a
// MALFORMED value is a 400, exactly like `limit`/`cursor`.

// queryActiveForVehicle is the query-parameter name that selects the slice.
const queryActiveForVehicle = "activeForVehicle"

// serveActiveForVehicle serves the `activeForVehicle` slice. The caller has
// already authenticated; userID is the JWT subject and is the ONLY owner
// scoping applied — the query string cannot widen it.
func (h *RideRequestHandler) serveActiveForVehicle(w http.ResponseWriter, r *http.Request, userID string) {
	vehicleID, ok := h.parseVehicleFilter(w, r, queryActiveForVehicle)
	if !ok {
		return
	}

	// The DEFAULT (createdAt, id) descending cursor, parsed by the same helper
	// the default feed uses — this slice keeps the feed's ordering, so it needs
	// no cursor of its own.
	limit, cursor, ok := h.parseListQuery(w, r)
	if !ok {
		return
	}

	page, err := h.store.ListActiveByOwnerVehiclePage(r.Context(), userID, vehicleID, cursor, limit)
	if err != nil {
		h.logger.Error("ride-request active: store failed",
			slog.String("user_id", userID),
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	// The unchanged RideRequestsListResponse envelope with the unchanged
	// (createdAt, id) anchor — buildRidePage, byte-for-byte what the default
	// feed emits.
	h.writeJSON(w, http.StatusOK, buildRidePage(page))
}

// parseVehicleFilter validates a vehicle-id query param on the owner feed.
// Empty, whitespace-only or oversized is 400 invalid_request — the same class
// as a bad `limit`/`cursor`. An id that is well-formed but UNKNOWN or owned by
// somebody else is NOT rejected here: it flows into the query and yields an
// empty page, so neither filter ever confirms whether a vehicle exists.
//
// Shared by `upcomingForVehicle` (MYR-360) and `activeForVehicle` (MYR-400):
// two sibling filters that disagreed about what a well-formed vehicle id is
// would be a validation seam, and the param name travels through so each still
// names itself in its own 400.
func (h *RideRequestHandler) parseVehicleFilter(w http.ResponseWriter, r *http.Request, param string) (string, bool) {
	vehicleID := strings.TrimSpace(r.URL.Query().Get(param))
	if vehicleID == "" || len(vehicleID) > maxVehicleIDLen {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			param+" must be a vehicle id")
		return "", false
	}
	return vehicleID, true
}
