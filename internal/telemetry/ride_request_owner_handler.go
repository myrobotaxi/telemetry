package telemetry

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// Owner-facing ride-request endpoints (P10, MYR-175), methods on the same
// RideRequestHandler as the rider surface (identical dependency set):
//
//   - GET  /api/ride-requests/incoming      — the owner's feed of open
//     requests across their vehicles (drives the IncomingRequestSheet, both
//     the "now" and scheduled variants).
//   - POST /api/ride-requests/{id}/accept   — owner-only, requested → accepted.
//   - POST /api/ride-requests/{id}/decline  — owner-only, requested → declined.
//
// Transition legality is enforced HERE (the store is a dumb persistence
// layer): accept/decline are legal only from `requested`; every other
// current status is 409 conflict per the rest-api.md §7.8 matrix. The
// enroute/arrived/completed transitions belong to MYR-176/177 — those
// endpoints do not exist yet, so those states are unreachable via this
// surface and any attempt to accept/decline a ride already past `requested`
// is rejected with the documented 409.
//
// Accept additionally publishes the RideAcceptedEvent dispatch seam
// (`ride.accepted` topic) carrying the pickup/dropoff places + booked-for
// passenger contact, which MYR-176 subscribes to for the Tesla
// navigation_request push. No Tesla calls happen here; the event is
// internal-only and never reaches the WS broadcast path.

// ServeIncoming handles GET /api/ride-requests/incoming — requests addressed
// to the authenticated owner (owner_id == JWT sub) still in `requested`
// (on-demand AND scheduled variants — a scheduled request is `requested` with
// scheduledFor set, not a separate status), newest first, cursor-paginated
// with the same RideRequestsListResponse envelope + (createdAt, id) cursor as
// the rider list.
func (h *RideRequestHandler) ServeIncoming(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	limit, cursor, ok := h.parseListQuery(w, r)
	if !ok {
		return
	}

	status := rideStatusRequested
	page, err := h.store.ListByOwnerPage(r.Context(), userID, &status, cursor, limit)
	if err != nil {
		h.logger.Error("ride-request incoming: store failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.writeJSON(w, http.StatusOK, buildRidePage(page))
}

// ServeAccept handles POST /api/ride-requests/{id}/accept. Owner-only;
// legal only from `requested` → `accepted` (409 otherwise). On success it
// publishes BOTH the ride_status_changed summary (via mutateStatus) and the
// RideAcceptedEvent dispatch seam for MYR-176.
func (h *RideRequestHandler) ServeAccept(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	rec, ok := h.loadForOwnerDecision(ctx, w, r, userID)
	if !ok {
		return
	}

	updated, ok := h.mutateStatus(ctx, w, rec, rideDecidableFrom, rideStatusAccepted)
	if !ok {
		return
	}

	// Guarded above: only the winning requested→accepted write reaches this
	// publish, so the dispatch seam fires exactly once per accept even under
	// a double-tap / two-device race.
	h.publish(ctx, buildRideAcceptedEvent(updated))

	h.writeJSON(w, http.StatusOK, toRideRequestWire(updated))
}

// ServeDecline handles POST /api/ride-requests/{id}/decline. Owner-only;
// legal only from `requested` → `declined` (409 otherwise).
func (h *RideRequestHandler) ServeDecline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	rec, ok := h.loadForOwnerDecision(ctx, w, r, userID)
	if !ok {
		return
	}

	updated, ok := h.mutateStatus(ctx, w, rec, rideDecidableFrom, rideStatusDeclined)
	if !ok {
		return
	}

	h.writeJSON(w, http.StatusOK, toRideRequestWire(updated))
}

// rideDecidableFrom is the allowed-from set for an owner accept/decline;
// must stay in lockstep with the loadForOwnerDecision fast-path check and
// the rest-api.md §7.8 matrix. The guarded UpdateStatusFrom write is what
// decides under concurrency.
var rideDecidableFrom = []string{rideStatusRequested}

// loadForOwnerDecision fetches the ride, enforces party membership (non-party
// → 404, same no-existence-leak rule as loadForParty), then the owner-only
// role (the rider is a party but cannot accept/decline → 403), then the
// `requested`-only legality fast-path (→ 409 conflict; friendly message —
// the guarded write in mutateStatus is what actually decides under
// concurrency). On any failure the response has been written and ok=false.
func (h *RideRequestHandler) loadForOwnerDecision(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) (RideRequestData, bool) {
	rec, ok := h.loadForParty(ctx, w, r, userID)
	if !ok {
		return RideRequestData{}, false
	}

	if userID != rec.OwnerID {
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodePermissionDenied, "only the vehicle owner may decide this request")
		return RideRequestData{}, false
	}

	if rec.Status != rideStatusRequested {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "ride request cannot be decided from status "+rec.Status)
		return RideRequestData{}, false
	}

	return rec, true
}

// buildRideAcceptedEvent projects the accepted record onto the dispatch-seam
// payload. Places travel plaintext on the internal bus (the repo already
// decrypted them); PassengerName/Phone are flattened to empty strings when
// absent so MYR-176 can branch on emptiness without nil checks.
func buildRideAcceptedEvent(rec RideRequestData) events.RideAcceptedEvent {
	ev := events.RideAcceptedEvent{
		RideRequestID: rec.ID,
		VehicleID:     rec.VehicleID,
		RiderID:       rec.RiderID,
		OwnerID:       rec.OwnerID,
		Pickup:        toEventPlace(rec.Pickup),
		Dropoff:       toEventPlace(rec.Dropoff),
		ScheduledFor:  rec.ScheduledFor,
	}
	if rec.PassengerName != nil {
		ev.PassengerName = *rec.PassengerName
	}
	if rec.PassengerPhone != nil {
		ev.PassengerPhone = *rec.PassengerPhone
	}
	if rec.AcceptedAt != nil {
		ev.AcceptedAt = *rec.AcceptedAt
	} else {
		// UpdateStatus stamps accepted_at on first entry; fall back to
		// updated_at defensively so the event always carries an instant.
		ev.AcceptedAt = rec.UpdatedAt
	}
	return ev
}

// toEventPlace converts the handler place shape to the events-bus shape
// (Address flattened to "" when absent).
func toEventPlace(p RidePlaceData) events.RidePlace {
	out := events.RidePlace{
		Latitude:  p.Latitude,
		Longitude: p.Longitude,
		Label:     p.Label,
	}
	if p.Address != nil {
		out.Address = *p.Address
	}
	return out
}
