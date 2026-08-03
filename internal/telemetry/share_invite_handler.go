package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

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
	// access busts a revoked viewer's cached access set — which stops the
	// NEXT connection. Optional.
	access AccessCacheInvalidator
	// sockets ends the CURRENT one (MYR-373). Optional; see
	// ShareAccessNotifier for why the cache bust alone does not cover it.
	sockets ShareAccessNotifier
	// links signs the `shareUrl` on pending rows (MYR-368). Nil means no
	// signing key is configured and the field is simply omitted.
	links  *InviteLinkSigner
	logger *slog.Logger
}

// ShareInviteHandlerOption configures optional handler behavior. Variadic
// rather than another positional parameter: the constructor already takes six,
// and the things being added here are independently optional side channels,
// not new required collaborators.
type ShareInviteHandlerOption func(*ShareInviteHandler)

// WithShareAccessNotifier attaches the live-socket teardown channel used when
// an owner revokes or suspends a grant (MYR-373). Omitting it leaves the
// pre-MYR-373 behavior: the grant is gone from every surface except an
// already-open WebSocket, which lapses at reconnect.
func WithShareAccessNotifier(n ShareAccessNotifier) ShareInviteHandlerOption {
	return func(h *ShareInviteHandler) { h.sockets = n }
}

// NewShareInviteHandler builds the owner-facing sharing handler. invalidator
// may be nil, in which case a revocation becomes effective on the next cache
// expiry rather than immediately. links may be nil, in which case pending rows
// carry `code` with no `shareUrl` and a client falls back to sharing the code.
func NewShareInviteHandler(
	tokens tokenValidator,
	vehicles VehicleSnapshotReader,
	invites ShareInviteStore,
	invalidator AccessCacheInvalidator,
	links *InviteLinkSigner,
	logger *slog.Logger,
	opts ...ShareInviteHandlerOption,
) *ShareInviteHandler {
	h := &ShareInviteHandler{
		auth:     tokens,
		vehicles: vehicles,
		invites:  invites,
		access:   invalidator,
		links:    links,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// endLiveAccess is the second half of every access-narrowing mutation: the
// cache bust stops the next handshake, this ends the connection that already
// completed one.
//
// ORDER MATTERS AND IS THE CALLER'S RESPONSIBILITY — every call site busts the
// cache first. The client reacts to the close by reconnecting, and that
// reconnect re-reads the access set; if the notification overtook the bust,
// the reconnect could be served the pre-mutation set and re-open the very
// stream the close just ended.
//
// Best-effort by construction. A nil notifier, an empty grantee id, or a
// grantee who simply is not connected are all ordinary no-ops — the mutation
// itself has already committed and the owner's 200 does not depend on anyone
// being online to hear about it.
func (h *ShareInviteHandler) endLiveAccess(granteeUserID, vehicleID, reason string) {
	if h.sockets == nil || granteeUserID == "" {
		return
	}
	h.sockets.ShareAccessRevoked(granteeUserID, vehicleID, reason)
}

// linkCtx assembles the signing context for one request: the key plus the
// CALLING OWNER's display name, resolved once per request rather than once per
// row.
//
// A name lookup failure DEGRADES rather than fails the request. The name is
// decoration on a link whose security comes from the signature, so refusing to
// mint an invite because a display name could not be read would trade a
// cosmetic loss for a functional one. The link is still signed — over an empty
// `from`, which is a canonical form the verifier already handles — and the
// `from` parameter is simply absent.
//
// Skipped entirely when there is no signing key: without one there is no
// shareUrl to decorate, so the query would be pure waste.
func (h *ShareInviteHandler) linkCtx(ctx context.Context, ownerUserID string) inviteLinkCtx {
	if h.links == nil {
		return inviteLinkCtx{}
	}
	name, err := h.invites.OwnerFirstName(ctx, ownerUserID)
	if err != nil {
		// The name itself is P1 and is not logged; only that it failed.
		h.logger.Warn("share invite: owner name lookup failed, link omits from",
			slog.String("user_id", ownerUserID),
			slog.String("error", err.Error()),
		)
		return inviteLinkCtx{signer: h.links}
	}
	return inviteLinkCtx{signer: h.links, ownerName: name}
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
	h.writeJSON(w, http.StatusCreated,
		toShareInviteMasked(&row, auth.RoleOwner, h.linkCtx(r.Context(), userID)))
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
	link := h.linkCtx(r.Context(), userID)
	invites := make([]map[string]any, 0, len(rows))
	for i := range rows {
		invites = append(invites, toShareInviteMasked(&rows[i], auth.RoleOwner, link))
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
