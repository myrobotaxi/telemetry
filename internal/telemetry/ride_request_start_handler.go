package telemetry

import (
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// Rider start endpoint (P10 owner-driven dispatch v2, MYR-270) — replaces the
// retired MYR-265 /board endpoint. RIDER-only, the guarded arrived→enroute
// transition (leg 2: car en route to the DROPOFF).
//
// Owner-driven handshake (reuses the existing status enum, no new values):
//   - accepted  = leg 1, car en route to the PICKUP (owner accepted; the pickup
//     nav was already pushed on accept via the MYR-176 dispatch seam).
//   - arrived   = rider picked up, awaiting the rider's start (set by the OWNER
//     tapping "Picked up" — POST .../picked-up).
//   - enroute   = leg 2, car en route to the DROPOFF. Set HERE by the RIDER
//     tapping "Start ride"; THIS is what pushes the DROPOFF nav (the
//     ride.started dispatch seam).
//   - completed = dropped off (set by the OWNER tapping "Dropped off").
//
// The rider CANNOT start before the owner confirms pickup: start is legal ONLY
// from `arrived`, so a still-`accepted` ride (owner has not tapped "Picked up")
// is a 409.
//
// Idempotency: a start when the ride is ALREADY enroute is a 200 no-op that
// re-returns the current record and does NOT re-publish or re-dispatch. Every
// non-{arrived,enroute} state is 409 conflict. The dropoff nav push is
// exactly-once per start: only the winning guarded arrived→enroute write
// publishes the seam (mirrors the accept→ride.accepted exactly-once discipline).

// ServeStart handles POST /api/ride-requests/{id}/start.
func (h *RideRequestHandler) ServeStart(w http.ResponseWriter, r *http.Request) {
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
		// The owner is a party but start is rider-only; non-parties were
		// already 404'd by loadForParty.
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodePermissionDenied, "only the rider may start this ride")
		return
	}

	// Idempotent no-op: the ride is already underway. A retried start returns
	// the current state (200) and does NOT re-publish the dropoff dispatch seam.
	if rec.Status == rideStatusEnroute {
		h.writeJSON(w, http.StatusOK, toRideRequestWire(rec))
		return
	}

	// Only arrived → enroute is legal. Friendly fast-path message; the guarded
	// write below is what actually decides under concurrency. A still-`accepted`
	// ride (owner has not confirmed pickup) lands here → 409, so the rider
	// cannot start before the owner taps "Picked up".
	if rec.Status != rideStatusArrived {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "ride request cannot be started from status "+rec.Status)
		return
	}

	updated, ok := h.mutateStatus(ctx, w, rec, rideStartableFrom, rideStatusEnroute)
	if !ok {
		return
	}

	// Guarded above: only the winning arrived→enroute write reaches this
	// publish, so the dropoff dispatch seam (and thus the Tesla nav push) fires
	// exactly once per start even under a double-tap / two-device race.
	h.publish(ctx, buildRideStartedEvent(updated))

	h.writeJSON(w, http.StatusOK, toRideRequestWire(updated))
}

// rideStartableFrom is the allowed-from set for a rider start; must stay in
// lockstep with the ServeStart fast-path check and the rest-api.md §7.8 matrix.
// Only arrived → enroute. The guarded UpdateStatusFrom write decides under
// concurrency.
var rideStartableFrom = []string{rideStatusArrived}

// buildRideStartedEvent projects the just-started record onto the leg-2
// dispatch-seam payload (MYR-270). Only the DROPOFF place is needed — the car
// is already at/near the pickup. The place travels plaintext on the internal
// bus (the repo already decrypted it); it is never broadcast to WS clients.
func buildRideStartedEvent(rec RideRequestData) events.RideStartedEvent {
	return events.RideStartedEvent{
		RideRequestID: rec.ID,
		VehicleID:     rec.VehicleID,
		OwnerID:       rec.OwnerID,
		Dropoff:       toEventPlace(rec.Dropoff),
	}
}
