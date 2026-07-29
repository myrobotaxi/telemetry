package telemetry

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// ShareInviteHandler serves the OWNER-FACING vehicle-sharing endpoints
// (MYR-184, rest-api.md §7.5):
//
//	POST   /api/vehicles/{vehicleId}/invites   mint a code
//	GET    /api/vehicles/{vehicleId}/invites   list invites + viewers
//	DELETE /api/invites/{inviteId}             cancel / revoke
//	POST   /api/invites/{inviteId}/resend      new code + reset expiry
//
// EVERY route here is owner-only, and none of them has a viewer branch. That is
// not an oversight to be "completed" later: a viewer who could list a car's
// invites would read the owner's private labels for other people, and a viewer
// who could mint one could re-share a car that is not theirs.
//
// Ownership is enforced twice on the vehicle-scoped routes — once here (to
// produce the right status and log line) and once in the SQL, which carries
// `owner_user_id = $n` on every mutation. The SQL predicate is the one that
// actually holds under concurrency; this check is the good error message.
type ShareInviteHandler struct {
	auth     tokenValidator
	vehicles VehicleSnapshotReader
	invites  ShareInviteStore
	// access busts a revoked viewer's cached access set. Optional.
	access AccessCacheInvalidator
	logger *slog.Logger
}

// NewShareInviteHandler builds the owner-facing sharing handler. invalidator
// may be nil, in which case a revocation becomes effective on the next cache
// expiry rather than immediately.
func NewShareInviteHandler(
	tokens tokenValidator,
	vehicles VehicleSnapshotReader,
	invites ShareInviteStore,
	invalidator AccessCacheInvalidator,
	logger *slog.Logger,
) *ShareInviteHandler {
	return &ShareInviteHandler{
		auth:     tokens,
		vehicles: vehicles,
		invites:  invites,
		access:   invalidator,
		logger:   logger,
	}
}

// ServeCreate handles POST /api/vehicles/{vehicleId}/invites.
//
// Mints ONE code shared by N rows, one per vehicle in the set. The response is
// the PATH vehicle's row — the sibling rows are not returned, and the code on
// the returned row is the one to hand out for all of them.
func (h *ShareInviteHandler) ServeCreate(w http.ResponseWriter, r *http.Request) {
	vehicleID, userID, ok := h.authOwner(w, r, "share invite create")
	if !ok {
		return
	}

	var body createShareInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return
	}

	in, reason := validateCreateInvite(body, vehicleID, userID)
	if reason != "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, reason)
		return
	}

	row, err := h.invites.CreateInvite(r.Context(), in)
	if err != nil {
		h.writeCreateError(w, vehicleID, userID, err)
		return
	}

	// The invite id, never the code or the label — both are P1.
	h.logger.Info("share invite created",
		slog.String("invite_id", row.ID),
		slog.String("vehicle_id", vehicleID),
		slog.Int("vehicle_count", len(in.VehicleIDs)),
		slog.String("permission", in.Permission),
	)
	h.writeJSON(w, http.StatusCreated, toShareInviteMasked(&row, auth.RoleOwner))
}

// writeCreateError maps a create failure onto a response. A vehicle in the set
// that the caller does not own is a 403 — the path vehicle already proved the
// caller is an owner of something, so there is no existence to protect here.
func (h *ShareInviteHandler) writeCreateError(w http.ResponseWriter, vehicleID, userID string, err error) {
	if errors.Is(err, ErrShareVehicleNotOwned) {
		h.logger.Warn("share invite create: set contains an unowned vehicle",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodePermissionDenied,
			"every vehicle in the invite must be one you own")
		return
	}
	h.logger.Error("share invite create failed",
		slog.String("vehicle_id", vehicleID),
		slog.String("error", err.Error()),
	)
	h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
}

// validateCreateInvite checks the body and normalizes the vehicle set.
// Returns a non-empty reason when the request must be rejected 400.
func validateCreateInvite(body createShareInviteRequest, pathVehicleID, ownerID string) (in ShareInviteCreateInput, rejectReason string) {
	label := strings.TrimSpace(body.Label)
	switch {
	case label == "":
		return ShareInviteCreateInput{}, "label is required"
	case len(label) > maxShareInviteLabelLen:
		return ShareInviteCreateInput{}, "label is too long"
	case !validSharePermission(body.Permission):
		return ShareInviteCreateInput{}, "permission must be one of live, live_history, rides"
	}

	// vehicleIds is OPTIONAL: omitting it is exactly equivalent to sending
	// the path vehicle alone.
	ids := body.VehicleIDs
	if body.VehicleIDs == nil {
		ids = []string{pathVehicleID}
	}
	if len(ids) == 0 {
		return ShareInviteCreateInput{}, "vehicleIds must not be empty"
	}
	if len(ids) > maxShareInviteVehicles {
		return ShareInviteCreateInput{}, "vehicleIds exceeds the per-invite limit"
	}

	// The PATH vehicle is what authorizes the call, so a set that omits it
	// is rejected rather than silently extended: accepting it would let a
	// caller use one owned vehicle's path to mint invites for a set that
	// never included it.
	deduped := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	includesPath := false
	for _, id := range ids {
		if id == "" {
			return ShareInviteCreateInput{}, "vehicleIds must not contain empty ids"
		}
		if id == pathVehicleID {
			includesPath = true
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		deduped = append(deduped, id)
	}
	if !includesPath {
		return ShareInviteCreateInput{}, "vehicleIds must include the vehicle in the path"
	}

	return ShareInviteCreateInput{
		OwnerUserID:   ownerID,
		PathVehicleID: pathVehicleID,
		VehicleIDs:    deduped,
		Label:         label,
		Permission:    body.Permission,
	}, ""
}

// ServeList handles GET /api/vehicles/{vehicleId}/invites.
//
// Returns pending invites and accepted grants, newest first. Revoked rows are
// never serialized. `code` is present only on pending rows.
func (h *ShareInviteHandler) ServeList(w http.ResponseWriter, r *http.Request) {
	vehicleID, userID, ok := h.authOwner(w, r, "share invite list")
	if !ok {
		return
	}

	rows, err := h.invites.ListInvitesForVehicle(r.Context(), vehicleID, userID)
	if err != nil {
		h.logger.Error("share invite list failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	// Always an array, never null: an owner with no invites gets [].
	invites := make([]map[string]any, 0, len(rows))
	for i := range rows {
		invites = append(invites, toShareInviteMasked(&rows[i], auth.RoleOwner))
	}
	h.writeJSON(w, http.StatusOK, shareInviteListResponse{Invites: invites})
}

// authOwner validates the bearer token and confirms the caller OWNS the path
// vehicle, writing the appropriate error response on failure.
//
// An unknown vehicle is 404 and a known-but-not-yours vehicle is 403, matching
// the rest of the per-vehicle surface. There is no viewer branch: a viewer's
// vehicle read succeeds, so they reach the ownership check and are refused
// exactly as an unrelated caller is.
func (h *ShareInviteHandler) authOwner(w http.ResponseWriter, r *http.Request, surface string) (vehicleID, userID string, ok bool) {
	vehicleID = r.PathValue("vehicleId")
	if vehicleID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing vehicleId")
		return "", "", false
	}

	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return "", "", false
	}

	ctx := r.Context()
	userID, err := h.auth.ValidateToken(ctx, token)
	if err != nil {
		h.logger.Warn(surface+": invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return "", "", false
	}

	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return "", "", false
		}
		h.logger.Error(surface+": vehicle lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return "", "", false
	}
	if row.UserID != userID {
		h.logger.Warn(surface+": not the owner",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
		return "", "", false
	}
	return vehicleID, userID, true
}

// writeJSON marshals v as JSON with the given status code.
func (h *ShareInviteHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("share invite: writeJSON encode failed", slog.String("error", err.Error()))
	}
}

// writeError writes the REST error envelope (rest-api.md §4.1).
func (h *ShareInviteHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
