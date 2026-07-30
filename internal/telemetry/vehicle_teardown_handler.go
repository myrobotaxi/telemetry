package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// VehicleTeardownHandler serves DELETE /api/tesla/vehicles/{vehicleId} — the
// owner "Remove this car" full-teardown endpoint (MYR-258,
// docs/architecture/car-offboarding.md). It is the reverse of the
// owner-onboarding link flow.
//
// Sequence (car-offboarding.md §5.1):
//
//  1. AuthZ: validate bearer → userId; resolve vehicleId → (VIN, owner) via
//     GetByID; the caller MUST own the vehicle (enforced here AND, defensively,
//     at the SQL layer inside the teardown writer).
//  2. Resolve the Tesla token (best-effort). If absent, skip step 3.
//  3. Best-effort DELETE fleet_telemetry_config at Tesla (non-fatal — the local
//     teardown is authoritative and a stale config self-expires while our
//     receiver rejects the stream regardless). Skipped for an empty VIN or when
//     no config-deleter is wired (tests/CI, or no proxy configured).
//  3a. ON A LAST-VEHICLE REMOVAL ONLY: best-effort POST to Tesla's OAuth
//     revocation endpoint, killing the grant itself (MYR-366). It runs HERE,
//     before step 4, because step 4 deletes the refresh token the call needs.
//     A mid-fleet removal skips it — the owner's other cars still stream, and
//     revoking would break every one of them.
//  4. Authoritative local teardown transaction (deletes Vehicle + cascade;
//     clears Account tokens + resets Settings on a last-vehicle removal; writes
//     the vehicle_deleted audit row). The Vehicle delete fires the existing
//     vehicle_deleted NOTIFY → dispatcher closes WS/mTLS streams + evicts caches.
//  5. Respond with the honest post-state + the two owner-only follow-ups (the
//     consent-revoke URL and the manual virtual-key-removal steps).
//
// Idempotent in the "equivalent final state" sense (rest-api.md §4.5): a
// second DELETE of an already-removed car returns 404 not_found, which clients
// MAY treat as a successful terminal state. The store's AlreadyGone→empty-tx
// path only covers the narrow race where the row is deleted between the
// handler's ownership lookup and RemoveVehicle.
type VehicleTeardownHandler struct {
	auth     tokenValidator
	vehicles VehicleSnapshotReader
	tokens   teslaTokenResolver
	fleet    FleetConfigDeleter // nil disables the Tesla-side config delete
	teardown VehicleTeardownWriter
	cfg      VehicleTeardownConfig
	// grant actively revokes the Tesla OAuth grant on a last-vehicle removal
	// (MYR-366). Optional — nil when no Tesla OAuth client is configured, in
	// which case the tokens are simply deleted without a revoke call, exactly
	// as before MYR-366. Set via WithTeslaGrantRevocation rather than a
	// constructor parameter so the existing eight call sites are untouched.
	grant  teslaLinkRevoker
	logger *slog.Logger
}

// VehicleTeardownConfig carries the non-dependency settings.
type VehicleTeardownConfig struct {
	// RevokeClientID is the Tesla OAuth client_id embedded in the
	// owner-confirmed consent-revoke URL. Empty omits the revokeUrl field.
	RevokeClientID string
	// RevokeBackURL is the app deep link Tesla returns to after the owner
	// confirms the revoke (e.g. "myrobotaxi://tesla-unlinked").
	RevokeBackURL string
}

// NewVehicleTeardownHandler wires the teardown handler. fleet may be nil (no
// proxy configured / tests) — the Tesla-side config delete is then skipped and
// the local teardown still runs.
func NewVehicleTeardownHandler(
	auth tokenValidator,
	vehicles VehicleSnapshotReader,
	tokens teslaTokenResolver,
	fleet FleetConfigDeleter,
	teardown VehicleTeardownWriter,
	cfg VehicleTeardownConfig,
	logger *slog.Logger,
	opts ...VehicleTeardownOption,
) *VehicleTeardownHandler {
	h := &VehicleTeardownHandler{
		auth:     auth,
		vehicles: vehicles,
		tokens:   tokens,
		fleet:    fleet,
		teardown: teardown,
		cfg:      cfg,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ServeHTTP routes DELETE only.
func (h *VehicleTeardownHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}
	h.handle(w, r)
}

// handle authenticates the caller, verifies ownership, and runs the teardown
// sequence.
func (h *VehicleTeardownHandler) handle(w http.ResponseWriter, r *http.Request) {
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

	ctx := r.Context()

	userID, err := h.auth.ValidateToken(ctx, token)
	if err != nil {
		h.logger.Warn("vehicle teardown: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	vin, ok := h.resolveOwnedVIN(ctx, w, vehicleID, userID)
	if !ok {
		return
	}

	// Best-effort Tesla-side stream-config delete (must run before the token is
	// cleared). Non-fatal — the local teardown is authoritative.
	streamConfigDeleted := h.deleteStreamConfig(ctx, userID, vin)

	// Best-effort active revocation of the Tesla OAuth grant, on a last-vehicle
	// removal only. Also before the teardown, and for a sharper reason: the
	// teardown DELETEs the "Account" row that holds the refresh token this call
	// presents (MYR-366).
	grantRevoked := h.revokeTeslaGrant(ctx, userID, vehicleID)

	// Authoritative local teardown (ownership re-enforced at the SQL layer).
	result, err := h.teardown.RemoveVehicle(ctx, userID, vehicleID)
	if err != nil {
		h.logger.Error("vehicle teardown: local teardown failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	if result.WasLastVehicle && !grantRevoked {
		// The store's locked count is authoritative; ours was a pre-check.
		// They disagree only when the owner's fleet changed underneath this
		// request, or when Tesla refused. Either way the tokens are now gone
		// unrevoked, and the owner-confirmed revokeUrl in the response is the
		// remaining path — worth a line, never worth a failure.
		h.logger.Warn("vehicle teardown: tesla tokens cleared without an accepted revocation",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
		)
	}

	h.logger.Info("vehicle teardown complete",
		slog.String("vehicle_id", vehicleID),
		slog.String("user_id", userID),
		slog.Bool("already_gone", result.AlreadyGone),
		slog.Bool("was_last_vehicle", result.WasLastVehicle),
		slog.Bool("stream_config_deleted", streamConfigDeleted),
		slog.Bool("tesla_grant_revoked", grantRevoked),
	)

	h.writeJSON(w, http.StatusOK, h.buildResponse(result, streamConfigDeleted))
}

// resolveOwnedVIN resolves vehicleId → VIN and enforces ownership. On an
// unknown vehicle it returns 404 (indistinguishable from ownership-filtered —
// never leak existence); on a real ownership mismatch it returns 403. Either
// way no teardown runs. Returns ok=false after writing the error.
func (h *VehicleTeardownHandler) resolveOwnedVIN(ctx context.Context, w http.ResponseWriter, vehicleID, userID string) (string, bool) {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			// Unknown vehicle → 404. This also covers idempotent re-removal:
			// the second DELETE of an already-gone car lands here as a clean
			// not-found rather than a teardown.
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return "", false
		}
		h.logger.Error("vehicle teardown: lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return "", false
	}

	if row.UserID != userID {
		h.logger.Warn("vehicle teardown: ownership mismatch",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
		return "", false
	}

	return row.VIN, true
}

// deleteStreamConfig best-effort deletes the vehicle's Tesla telemetry config.
// Returns whether the delete succeeded. Never fails the request: a missing
// token, empty VIN, unwired deleter, or a Tesla error are all logged and
// swallowed (car-offboarding.md §5.2).
func (h *VehicleTeardownHandler) deleteStreamConfig(ctx context.Context, userID, vin string) bool {
	if h.fleet == nil || vin == "" {
		return false
	}

	tok, err := h.tokens.Resolve(ctx, userID)
	if err != nil {
		h.logger.Warn("vehicle teardown: no Tesla token — skipping stream-config delete",
			slog.String("user_id", userID),
			slog.String("vin", redactVIN(vin)),
		)
		return false
	}

	if err := h.fleet.DeleteTelemetryConfig(ctx, tok.AccessToken, vin); err != nil {
		// Non-fatal: the local teardown still removes the owner's data, and a
		// stale config self-expires while our receiver rejects the stream.
		h.logger.Warn("vehicle teardown: stream-config delete failed (non-fatal)",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		return false
	}
	return true
}

// revokeTeslaGrant best-effort revokes the owner's Tesla OAuth grant when this
// removal takes their last car (MYR-366). Returns whether Tesla accepted it.
// Never fails the request: an unwired revoker, a mid-fleet removal, a DB read
// error, a network error and a Tesla 5xx all return false and the
// authoritative local teardown proceeds unchanged.
func (h *VehicleTeardownHandler) revokeTeslaGrant(ctx context.Context, userID, vehicleID string) bool {
	if h.grant == nil {
		return false
	}
	return h.grant.RevokeIfLastVehicle(ctx, userID, vehicleID)
}

// buildResponse assembles the honest post-teardown body.
func (h *VehicleTeardownHandler) buildResponse(result VehicleTeardownResult, streamConfigDeleted bool) vehicleTeardownResponse {
	return vehicleTeardownResponse{
		Removed:             true, // authoritative: gone from the app (removed or already-gone)
		WasLastVehicle:      result.WasLastVehicle,
		TeslaTokensCleared:  result.TeslaTokensCleared,
		StreamConfigDeleted: streamConfigDeleted,
		RevokeURL:           buildRevokeURL(h.cfg.RevokeClientID, h.cfg.RevokeBackURL),
		VirtualKeyRemoval: virtualKeyRemoval{
			Required:    true,
			Automatable: false,
			Steps:       defaultVirtualKeyRemovalSteps(),
		},
	}
}

// writeJSON / writeError mirror the FleetConfigHandler helpers (rest-api.md
// §4.1 typed error envelope).
func (h *VehicleTeardownHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

func (h *VehicleTeardownHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
