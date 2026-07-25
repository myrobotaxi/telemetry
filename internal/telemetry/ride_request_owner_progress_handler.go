package telemetry

import (
	"context"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// Owner progress endpoints (P10 owner-driven dispatch v2, MYR-270). The owner
// drives the pickup/dropoff handshake that brackets the rider's start:
//
//   - POST /api/ride-requests/{id}/picked-up   — OWNER-only, accepted→arrived
//     ("Picked up": the rider is aboard, awaiting the rider's Start).
//   - POST /api/ride-requests/{id}/dropped-off — OWNER-only, enroute→completed
//     ("Dropped off": the ride is finished).
//
// Completion is now OWNER-confirmed: MYR-270 removed the MYR-265 drive-end
// auto-completion (internal/ridecomplete), so a car parking no longer closes a
// ride — the owner does, here.
//
// Both endpoints are owner-only (the rider is a party but cannot confirm → 403;
// a non-party → 404), enforce transition legality in the handler (the store
// stays a dumb persistence layer), are idempotent (a repeat of the destination
// state is a 200 no-op; every other state is 409 conflict), and publish the
// summary ride_status_changed via mutateStatus. NEITHER pushes Tesla nav — the
// only nav push in this flow is the DROPOFF, fired by the rider start endpoint.

// ServePickedUp handles POST /api/ride-requests/{id}/picked-up. Owner-only;
// legal only from accepted → arrived (409 otherwise). Idempotent: an already
// `arrived` ride is a 200 no-op that re-returns the current record.
func (h *RideRequestHandler) ServePickedUp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}
	rec, ok := h.loadForOwner(ctx, w, r, userID)
	if !ok {
		return
	}

	// Idempotent no-op: the owner already confirmed pickup.
	if rec.Status == rideStatusArrived {
		h.writeJSON(w, http.StatusOK, toRideRequestWire(rec))
		return
	}
	// Only accepted → arrived is legal. Friendly fast-path message; the guarded
	// write below is what actually decides under concurrency.
	if rec.Status != rideStatusAccepted {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "ride request cannot be marked picked-up from status "+rec.Status)
		return
	}

	updated, ok := h.mutateStatus(ctx, w, rec, ridePickedUpFrom, rideStatusArrived)
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, toRideRequestWire(updated))
}

// ServeDroppedOff handles POST /api/ride-requests/{id}/dropped-off. Owner-only;
// legal only from enroute → completed (409 otherwise). Idempotent: an already
// `completed` ride is a 200 no-op that re-returns the current record.
func (h *RideRequestHandler) ServeDroppedOff(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}
	rec, ok := h.loadForOwner(ctx, w, r, userID)
	if !ok {
		return
	}

	// Idempotent no-op: the ride is already completed.
	if rec.Status == rideStatusCompleted {
		h.writeJSON(w, http.StatusOK, toRideRequestWire(rec))
		return
	}
	// Only enroute → completed is legal. Friendly fast-path message; the guarded
	// write below is what actually decides under concurrency.
	if rec.Status != rideStatusEnroute {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "ride request cannot be marked dropped-off from status "+rec.Status)
		return
	}

	updated, ok := h.mutateStatus(ctx, w, rec, rideDroppedOffFrom, rideStatusCompleted)
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, toRideRequestWire(updated))
}

// ridePickedUpFrom / rideDroppedOffFrom are the owner handshake allowed-from
// sets; must stay in lockstep with the fast-path checks above and the
// rest-api.md §7.8 matrix. The guarded UpdateStatusFrom write is what decides
// under concurrency.
var (
	ridePickedUpFrom   = []string{rideStatusAccepted}
	rideDroppedOffFrom = []string{rideStatusEnroute}
)

// loadForOwner fetches the ride, enforces party membership (non-party → 404,
// same no-existence-leak rule as loadForParty), then the owner-only role (the
// rider is a party but cannot confirm pickup/dropoff → 403). Unlike
// loadForOwnerDecision it applies NO status fast-path — the
// picked-up/dropped-off handlers each enforce their own transition legality and
// idempotent no-op.
func (h *RideRequestHandler) loadForOwner(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) (RideRequestData, bool) {
	rec, ok := h.loadForParty(ctx, w, r, userID)
	if !ok {
		return RideRequestData{}, false
	}
	if userID != rec.OwnerID {
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodePermissionDenied, "only the vehicle owner may confirm this ride")
		return RideRequestData{}, false
	}
	return rec, true
}
