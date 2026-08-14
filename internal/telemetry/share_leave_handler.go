package telemetry

import (
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// MYR-469 — DELETE /api/vehicles/{vehicleId}/share: the RIDER leaves a car
// somebody shared with them.
//
// The external-beta report was an absence: "I can't delete the lunar car as
// rider" — every removal path was owner-side (revoke, MYR-258's offboarding),
// and a rider's only ways out were asking the owner or deleting their whole
// account. This is the viewer-side mirror of ServeRevoke: the same tombstone,
// the same access teardown, keyed on the VEHICLE because a vehicle id is the
// only identity the rider holds (the §7.0 catalog rows carry no invite id).
//
// 204 with no body, IDEMPOTENT, and deliberately information-free: a vehicle
// the caller never had access to and one they already left are
// indistinguishable (both 204), so the endpoint cannot be used to probe which
// vehicle ids exist — ServeRevoke's own anti-enumeration stance, restated for
// a caller who is not the owner.
//
// THE ONE REFUSAL is 409 while the caller has a live ride on the car: the
// ride's telemetry access rides the grant, so a leave mid-ride would be the
// MYR-449 dark stream self-inflicted — a rider aboard a car their own app can
// no longer see. The guard is INSIDE the store's UPDATE (NOT EXISTS), so a
// ride created between a check and the write cannot slip through, and the 409
// is only ever spoken when an accepted grant genuinely exists to hold
// (an owner self-riding a never-shared car gets the ordinary 204 no-op).
func (h *ShareInviteHandler) ServeLeave(w http.ResponseWriter, r *http.Request) {
	vehicleID := r.PathValue("vehicleId")
	if vehicleID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing vehicleId")
		return
	}

	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return
	}
	userID, err := h.auth.ValidateToken(r.Context(), token)
	if err != nil {
		h.logger.Warn("share leave: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	outcome, err := h.invites.LeaveVehicleShares(r.Context(), vehicleID, userID)
	if err != nil {
		h.logger.Error("share leave failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()))
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "could not leave vehicle")
		return
	}
	if outcome == ShareLeaveRefusedLiveRide {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"a ride on this vehicle is still in progress")
		return
	}

	// The same two-step teardown the owner's revoke performs, in the same
	// order, because a leave is a revoke the viewer wrote themselves: bust the
	// caller's cached access set so the NEXT handshake refuses the car, then
	// end any socket that already got past one (MYR-373) — a connection opened
	// before the leave holds its access set frozen and would keep streaming
	// this car's live GPS to somebody who just walked away from it.
	if h.access != nil {
		h.access.InvalidateVehicles(userID)
	}
	h.endLiveAccess(userID, vehicleID, "left")

	h.logger.Info("share left by viewer", slog.String("vehicle_id", vehicleID))
	w.WriteHeader(http.StatusNoContent)
}
