package telemetry

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// The two invite-scoped owner actions (MYR-184, rest-api.md §7.5.3 / §7.5.4).
// Unlike the vehicle-scoped routes these have no vehicleId in the path, so
// ownership is established by the invite's own owner_user_id — enforced in the
// SQL WHERE clause of every statement, not by a read-then-write here.

// ServeRevoke handles DELETE /api/invites/{inviteId}.
//
// Cancels a pending invite or revokes an accepted grant. The row is TOMBSTONED
// (status → revoked, revoked_at stamped), never hard-deleted: an access grant
// that silently vanishes from the database leaves no way to answer "who had
// access to this car in June".
//
// 204 with no body, and IDEMPOTENT — revoking an already-revoked invite is 204
// too, so a client retrying a dropped response does not see a spurious 404.
// A row that does not exist, or belongs to somebody else, is 404: the two are
// deliberately indistinguishable so this endpoint cannot be used to probe for
// other people's invite ids.
func (h *ShareInviteHandler) ServeRevoke(w http.ResponseWriter, r *http.Request) {
	inviteID, userID, ok := h.authInvite(w, r, "share invite revoke")
	if !ok {
		return
	}

	revokedViewerID, err := h.invites.RevokeInvite(r.Context(), inviteID, userID)
	if err != nil {
		h.writeInviteError(w, "share invite revoke", inviteID, err)
		return
	}

	// End the revoked viewer's access NOW rather than at the next cache
	// expiry. This is a security property, not a nicety: the cached access
	// set is what the WebSocket handshake and every per-vehicle handler
	// consult, so a stale entry is a live grant.
	if h.access != nil && revokedViewerID != "" {
		h.access.InvalidateVehicles(revokedViewerID)
	}

	h.logger.Info("share invite revoked",
		slog.String("invite_id", inviteID),
		slog.Bool("had_viewer", revokedViewerID != ""),
	)
	w.WriteHeader(http.StatusNoContent)
}

// ServeResend handles POST /api/invites/{inviteId}/resend.
//
// Mints a NEW code on the SAME row and resets the expiry to a full TTL from
// now, invalidating the previous code. The invite id and createdAt are
// unchanged, so a client holding the id keeps working and the owner's
// "sent {ago}" line still refers to the original send.
//
// PENDING ONLY. Resending an accepted grant is 409 conflict, because changing
// who holds an accepted grant is a revoke plus a fresh invite, not a resend —
// silently re-opening a live grant for redemption by a different person would
// be a quiet transfer of access.
func (h *ShareInviteHandler) ServeResend(w http.ResponseWriter, r *http.Request) {
	inviteID, userID, ok := h.authInvite(w, r, "share invite resend")
	if !ok {
		return
	}

	row, err := h.invites.ResendInvite(r.Context(), inviteID, userID)
	if err != nil {
		h.writeInviteError(w, "share invite resend", inviteID, err)
		return
	}

	h.logger.Info("share invite resent", slog.String("invite_id", inviteID))
	h.writeJSON(w, http.StatusOK, toShareInviteMasked(row, auth.RoleOwner))
}

// authInvite validates the bearer token and extracts the invite id. It does NOT
// pre-check ownership: every invite-scoped statement carries
// `owner_user_id = $n` itself, so a separate read here would only add a
// check-then-write window and a second way for the two to disagree.
func (h *ShareInviteHandler) authInvite(w http.ResponseWriter, r *http.Request, surface string) (string, string, bool) {
	inviteID := r.PathValue("inviteId")
	if inviteID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing inviteId")
		return "", "", false
	}

	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return "", "", false
	}

	userID, err := h.auth.ValidateToken(r.Context(), token)
	if err != nil {
		h.logger.Warn(surface+": invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return "", "", false
	}
	return inviteID, userID, true
}

// writeInviteError maps a store failure on an invite-scoped action.
//
// "Not found" covers three cases on purpose — the invite does not exist, it
// belongs to another owner, or it is a revoked tombstone — because telling them
// apart would turn the endpoint into an oracle for other people's invite ids.
func (h *ShareInviteHandler) writeInviteError(w http.ResponseWriter, surface, inviteID string, err error) {
	switch {
	case errors.Is(err, sdk.ErrNotFound):
		h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "invite not found")
	case errors.Is(err, ErrShareNotPending):
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"this invite has already been accepted")
	default:
		h.logger.Error(surface+" failed",
			slog.String("invite_id", inviteID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
	}
}
