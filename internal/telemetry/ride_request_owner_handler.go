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

	// MYR-277: a vehicle that is in service or offline cannot fulfill the
	// ride — block the accept before the guarded transition + dispatch seam
	// fire. Decline is intentionally NOT gated (an owner may always decline).
	if h.rejectIfVehicleUnavailable(ctx, w, rec) {
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

// vehicleStatusOffline is the persisted status for a vehicle the server
// cannot currently reach. Together with serviceStatusInService it is the
// blocked set for an owner accept (MYR-277): neither state can fulfill a ride.
// `parked`/`driving`/`charging` are dispatchable and pass the gate.
const vehicleStatusOffline = "offline"

// rejectIfVehicleUnavailable is the MYR-277 dispatch-capability gate on
// accept. It reads the ride's target vehicle's CURRENT persisted status (via
// the same VehicleSnapshotReader the create path uses) and refuses the accept
// with 409 vehicle_unavailable when the vehicle is `in_service` (owner put it
// into service) or `offline` (unreachable) — a car in either state cannot be
// dispatched to fulfill the ride. This is a capability gate, distinct from the
// `requested`-only lifecycle 409 (ErrCodeConflict): the transition is legal,
// the vehicle just can't serve it. Reading the status HERE (not caching the
// loadForOwnerDecision read) keeps the gate current under a status flip
// between list and accept. On a status-lookup failure it fails CLOSED (500):
// we do not accept a ride we cannot confirm the vehicle can serve. Returns
// true (response already written) when the accept must stop.
//
// SCHEDULED rides are EXEMPT (MYR-313). "Can this car be dispatched right
// now?" is only the question an INSTANT accept is asking; a reservation days
// out says nothing about the car's status today, and refusing it stranded the
// owner (client report: a Saturday 5:30 PM request refused with "Vehicle is in
// service and can't be dispatched" while the car was in service that day).
// This aligns the gate with the two guards it is the analogue of — the
// per-rider `ride_active` index and the per-vehicle one-active-ride index
// (MYR-266), both partial on `scheduled_for IS NULL` — and with what §4.1.1
// already documents ("scheduled rides are exempt from both"). Availability at
// the reservation instant belongs to the scheduled-dispatch machinery
// (MYR-179), which must re-check the vehicle THEN; accepting a reservation is
// not dispatching it.
// MYR-316 amends the exemption WITHOUT weakening it. A scheduled accept is
// still exempt from the availability gate, but it is now additionally bound by
// the vehicle's service window: a reservation for a time BEFORE the car is
// expected back is refused with 400 invalid_request. That is the fact MYR-313's
// reasoning assumed and did not have — "a reservation days out says nothing
// about the car's status today" is true, but nothing made the reservation be
// days out. Consequently a scheduled accept now DOES read the vehicle (MYR-313
// short-circuited before the read), and the read FAILS OPEN for scheduled rides
// so a lookup failure cannot resurrect the MYR-313 defect.
func (h *RideRequestHandler) rejectIfVehicleUnavailable(ctx context.Context, w http.ResponseWriter, rec RideRequestData) bool {
	row, err := h.vehicles.GetByID(ctx, rec.VehicleID)
	if err != nil {
		if rec.ScheduledFor != nil {
			// Fail OPEN: the MYR-316 bound is advisory, and refusing a
			// reservation because we could not read the car is exactly the
			// stranding MYR-313 exists to prevent.
			h.logger.Warn("ride-request accept: vehicle lookup failed; scheduled accept proceeds unbounded",
				slog.String("ride_request_id", rec.ID),
				slog.String("vehicle_id", rec.VehicleID),
				slog.String("error", err.Error()),
			)
			return false
		}
		h.logger.Error("ride-request accept: vehicle status lookup failed",
			slog.String("ride_request_id", rec.ID),
			slog.String("vehicle_id", rec.VehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return true
	}

	// MYR-316 service-window bound. Applies to SCHEDULED accepts only (the
	// helper no-ops on a nil scheduledFor).
	if h.rejectIfBeforeServiceWindow(w, rec.ScheduledFor, row) {
		return true
	}

	// MYR-342 ride-sharing pause. Checked BEFORE the MYR-313 instant-only
	// short-circuit below, because — unlike the in-service/offline gate — it
	// applies to SCHEDULED accepts TOO. See ride_share_gate.go for the full
	// argument; in short, the MYR-313 exemption rests on service visits ENDING,
	// and an owner's pause has no such horizon. Accepting a reservation for a
	// car its owner has withdrawn indefinitely leaves the request in the owner's
	// queue and the rider expecting a car that is not coming.
	//
	// It rides the read ABOVE rather than adding one, which is also what keeps
	// the MYR-313 fail-open shape intact: a lookup failure on a scheduled accept
	// returns early up there, so an UNKNOWN pause state never blocks. Unknown is
	// not paused.
	if rejectIfRideSharePaused(w, h.logger, "ride-request accept", row, rec.OwnerID) {
		return true
	}

	// MYR-313: the availability gate below is INSTANT-only.
	if rec.ScheduledFor != nil {
		return false
	}

	switch row.Status {
	case serviceStatusInService:
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable, "Vehicle is in service and can't be dispatched")
		return true
	case vehicleStatusOffline:
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable, "Vehicle is offline and can't be dispatched")
		return true
	}
	return false
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
