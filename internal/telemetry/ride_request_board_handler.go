package telemetry

import (
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// Rider board endpoint (P10 ride-hailing autonomous flow, MYR-265), a method on
// the same RideRequestHandler as the rest of the rider surface.
//
//   - POST /api/ride-requests/{id}/board — rider-only, the guarded
//     accepted→enroute transition (leg 2: rider aboard, car en route to
//     DROPOFF).
//
// Leg semantics (NO driver-confirm — the ride is autonomous):
//   - accepted = leg 1, car en route to PICKUP (owner accept already pushed the
//     pickup nav via the MYR-176 dispatch seam).
//   - board flips accepted→enroute = leg 2, and publishes the RideBoardedEvent
//     dispatch seam that triggers the DROPOFF nav push.
//   - the car parking at the dropoff (drive-end detector) drives
//     enroute→completed (see internal/ridecomplete).
//
// Idempotency: a board when the ride is ALREADY enroute is a 200 no-op that
// re-returns the current record and does NOT re-publish or re-dispatch. Every
// non-{accepted,enroute} state is 409 conflict. The dropoff nav push is
// exactly-once per board: only the winning guarded accepted→enroute write
// publishes the seam (mirrors the accept→ride.accepted exactly-once discipline).

// ServeBoard handles POST /api/ride-requests/{id}/board.
func (h *RideRequestHandler) ServeBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	rec, ok := h.loadForParty(ctx, w, r, userID)
	if !ok {
		return
	}
	if userID != rec.RiderID {
		// The owner is a party but board is rider-only; non-parties were
		// already 404'd by loadForParty.
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodePermissionDenied, "only the rider may board this ride")
		return
	}

	// Idempotent no-op: the rider is already aboard. A retried board returns the
	// current state (200) and does NOT re-publish the dropoff dispatch seam.
	if rec.Status == rideStatusEnroute {
		h.writeJSON(w, http.StatusOK, toRideRequestWire(rec))
		return
	}

	// Only accepted → enroute is legal. Friendly fast-path message; the guarded
	// write below is what actually decides under concurrency.
	if rec.Status != rideStatusAccepted {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "ride request cannot be boarded from status "+rec.Status)
		return
	}

	updated, ok := h.mutateStatus(ctx, w, rec, rideBoardableFrom, rideStatusEnroute)
	if !ok {
		return
	}

	// Guarded above: only the winning accepted→enroute write reaches this
	// publish, so the dropoff dispatch seam (and thus the Tesla nav push) fires
	// exactly once per board even under a double-tap / two-device race.
	h.publish(ctx, buildRideBoardedEvent(updated))

	h.writeJSON(w, http.StatusOK, toRideRequestWire(updated))
}

// rideBoardableFrom is the allowed-from set for a rider board; must stay in
// lockstep with the ServeBoard fast-path check and the rest-api.md §7.8 matrix.
// Only accepted → enroute. The guarded UpdateStatusFrom write decides under
// concurrency.
var rideBoardableFrom = []string{rideStatusAccepted}

// buildRideBoardedEvent projects the just-boarded record onto the leg-2
// dispatch-seam payload (MYR-265). Only the DROPOFF place is needed — the car
// is already at/near the pickup. The place travels plaintext on the internal
// bus (the repo already decrypted it); it is never broadcast to WS clients.
func buildRideBoardedEvent(rec RideRequestData) events.RideBoardedEvent {
	return events.RideBoardedEvent{
		RideRequestID: rec.ID,
		VehicleID:     rec.VehicleID,
		OwnerID:       rec.OwnerID,
		Dropoff:       toEventPlace(rec.Dropoff),
	}
}
