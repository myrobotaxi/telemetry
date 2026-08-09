package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// ShareRedeemHandler serves POST /api/invites/redeem — the RIDER-side join
// (MYR-184, rest-api.md §7.5.5).
//
// This is the only sharing endpoint an invited party ever calls, and the only
// one whose response they ever see. It deliberately returns NONE of the
// owner-facing ShareInvite shape: no label (the owner's private memo for this
// person), no code (still live for the other rows of a multi-vehicle invite),
// no invite id.
type ShareRedeemHandler struct {
	auth     tokenValidator
	redeem   ShareRedeemStore
	vehicles SharedVehicleLister
	limiter  *redeemLimiter
	// access busts the redeemer's cached vehicle set so the car they just
	// joined appears on the very next request.
	access AccessCacheInvalidator
	logger *slog.Logger
}

// NewShareRedeemHandler builds the redeem handler with the default per-user
// rate limit. invalidator may be nil (the grant then becomes visible on the
// next access-set cache expiry instead of immediately).
func NewShareRedeemHandler(
	tokens tokenValidator,
	redeem ShareRedeemStore,
	vehicles SharedVehicleLister,
	invalidator AccessCacheInvalidator,
	logger *slog.Logger,
) *ShareRedeemHandler {
	return &ShareRedeemHandler{
		auth:     tokens,
		redeem:   redeem,
		vehicles: vehicles,
		limiter:  newRedeemLimiter(redeemRateLimit, redeemRateWindow),
		access:   invalidator,
		logger:   logger,
	}
}

// ServeHTTP handles POST /api/invites/redeem.
func (h *ShareRedeemHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return
	}

	ctx := r.Context()
	userID, err := h.auth.ValidateToken(ctx, token)
	if err != nil {
		h.logger.Warn("share redeem: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	// Rate-limit BEFORE the body is examined, so a malformed-body flood
	// cannot be used to probe timing, and count the attempt whatever its
	// outcome.
	if !h.limiter.allow(userID) {
		h.logger.Warn("share redeem: rate limited", slog.String("user_id", userID))
		h.writeError(w, http.StatusTooManyRequests, wserrors.ErrCodeRateLimited,
			"too many redemption attempts — try again in a minute")
		return
	}

	code, ok := h.decodeCode(w, r)
	if !ok {
		return
	}

	grants, err := h.redeem.RedeemCode(ctx, code, userID)
	if err != nil {
		h.writeRedeemError(w, userID, err)
		return
	}
	h.writeGrants(ctx, w, userID, grants)
}

// decodeCode reads and normalizes the submitted code.
//
// Normalization mirrors the client's entry field (upper-case, strip everything
// outside [A-Z0-9]) so a code pasted with a stray space or hyphen still works.
// A code that is still malformed after that is 400 — never 404 — because the
// 404 body is reserved for "this code does not grant you anything", and
// blurring the two would make the deliberate invalid/expired/consumed
// indistinguishability harder to reason about, not easier.
//
// The code is NEVER logged and never echoed into the error body: it is a live
// bearer credential, and an error message repeating it is a confirmation oracle.
func (h *ShareRedeemHandler) decodeCode(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body redeemShareInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return "", false
	}

	code := normalizeShareCode(body.Code)
	if !validShareCodeFormat(code) {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"code must be 6 characters, A-Z and 0-9")
		return "", false
	}
	return code, true
}

// writeGrants builds the redeemer-facing 200 body from what the redemption
// granted.
func (h *ShareRedeemHandler) writeGrants(ctx context.Context, w http.ResponseWriter, userID string, grants []ShareGrantRow) {
	if len(grants) == 0 {
		// Unreachable by contract — a redemption that granted nothing
		// returns an error — but a 200 with an empty vehicle list would be
		// a schema violation, so refuse rather than emit one.
		h.logger.Error("share redeem: succeeded with zero grants", slog.String("user_id", userID))
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	// The grant is live in the database now; make it visible immediately
	// rather than after the access-set cache lapses. Done BEFORE the
	// catalog read so that read cannot race an empty cached set.
	if h.access != nil {
		h.access.InvalidateVehicles(userID)
	}

	vehicleIDs := make([]string, 0, len(grants))
	for _, g := range grants {
		vehicleIDs = append(vehicleIDs, g.VehicleID)
	}

	rows, err := h.vehicles.ListSharedByIDs(ctx, userID, vehicleIDs)
	if err != nil {
		h.logger.Error("share redeem: shared catalog read failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	ownerFirstName, err := h.redeem.OwnerFirstName(ctx, grants[0].OwnerUserID)
	if err != nil {
		h.logger.Error("share redeem: owner name lookup failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.logger.Info("share invite redeemed",
		slog.String("user_id", userID),
		slog.Int("vehicle_count", len(rows)),
	)
	h.writeJSON(w, http.StatusOK, redeemShareInviteResponse{
		OwnerFirstName: ownerFirstName,
		Vehicles:       buildSharedSummaries(rows),
	})
}

// writeRedeemError maps a redemption failure.
//
// The three dead-code cases — unknown, expired, and consumed by somebody else —
// ALL answer 404 with the same body. That is the contract, and it is the point:
// an enumerating caller must not learn that a guessed code exists but has
// already been used, which would be a hit worth following up.
func (h *ShareRedeemHandler) writeRedeemError(w http.ResponseWriter, userID string, err error) {
	switch {
	case errors.Is(err, sdk.ErrNotFound):
		h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "invite code not found")
	case errors.Is(err, ErrShareSelfRedeem):
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"you already own this vehicle")
	case errors.Is(err, ErrShareAlreadyGranted):
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"this vehicle is already shared with you")
	default:
		// The code is P1 and is deliberately absent from this log line.
		h.logger.Error("share redeem failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
	}
}

// normalizeShareCode upper-cases and strips every character outside [A-Z0-9],
// exactly as the client's entry field does.
func normalizeShareCode(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	for _, r := range strings.ToUpper(in) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// validShareCodeFormat reports whether s is exactly 6 characters of [A-Z0-9].
func validShareCodeFormat(s string) bool {
	if len(s) != shareCodeWireLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// shareCodeWireLength is the fixed code length on the wire.
const shareCodeWireLength = 6

// buildSharedSummaries projects granted vehicles through the VIEWER
// VehicleSummary mask and stamps role + sharePermission.
//
// The rows go through the SAME mask GET /api/vehicles applies to a viewer row
// (auth.RoleViewer), not a bespoke projection: a redeemer must not see one
// field more here than they will see in their catalog a second later.
func buildSharedSummaries(rows []SharedVehicleRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	// One clock reading for the whole response — see viewerSummaryMap.
	now := time.Now()
	for i := range rows {
		row := &rows[i]
		out = append(out, viewerSummaryMap(row.VehicleCatalogRow, auth.ShareGrant{AllowRides: row.AllowRides}, now))
	}
	return out
}

// writeJSON marshals v as JSON with the given status code.
func (h *ShareRedeemHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("share redeem: writeJSON encode failed", slog.String("error", err.Error()))
	}
}

// writeError writes the REST error envelope (rest-api.md §4.1).
func (h *ShareRedeemHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
