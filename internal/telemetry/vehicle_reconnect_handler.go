package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// FleetConfigPusher installs a vehicle's fleet-telemetry config at Tesla —
// EXACTLY the call first pairing makes, minus the OAuth. Satisfied by the same
// cmd/-level pusher the link-time hook and the MYR-448 reconciler use, so there
// is one construction of the config request in the process and not two that
// drift on a field-map change.
//
// Nil means the tesla-http-proxy is unconfigured. The route is still mounted —
// see the handler's Enablement note.
type FleetConfigPusher interface {
	PushForVIN(ctx context.Context, token, vin string) error
}

// SuspensionClearer removes a vehicle's inactivity episode. Satisfied by
// *store.TelemetrySuspensionRepo.ClearForVehicle.
type SuspensionClearer interface {
	ClearForVehicle(ctx context.Context, vehicleID string) error
}

// VehicleReconnectHandler serves
// POST /api/tesla/vehicles/{vehicleId}/reconnect — the owner's way back from an
// inactivity suspension (MYR-592, rest-api.md §7.28).
//
// ── WHAT IT IS AND WHY IT IS SO SMALL ───────────────────────────────────────
//
// The suspension sweeper removes exactly ONE thing: the vehicle's
// fleet-telemetry config at Tesla. The OAuth grant, the virtual key, the
// `Vehicle` row, every share and every ride survive it. So the way back is to
// put that one thing back — and nothing else. There is no OAuth round trip, no
// consent screen, no lock/unlock dance, no re-pairing: those belong to FIRST
// pairing, and the contract (vehicle-summary.schema.json) makes the seamlessness
// of this call the whole justification for suspending narrowly in the first
// place.
//
// ── THE TWO OFFERS ──────────────────────────────────────────────────────────
//
// The contract requires the owner's disconnect notice to carry exactly two:
// RE-CONNECT (this endpoint) and UNLINK COMPLETELY (the §7.12 teardown, which
// already clears the same episode row). This is the first of the pair; nothing
// here is a partial teardown, and there is deliberately no third option.
//
// ── ORDER: TESLA FIRST, THEN THE STAMP ──────────────────────────────────────
//
// The push runs BEFORE the episode is cleared, mirroring the sweeper's
// delete-then-stamp. Clearing first would produce the state the platform cannot
// explain from the other direction: a catalog saying "connected" for a car with
// no config, an owner with no notice, and nothing left to press. In this order a
// failure leaves the suspension visible and the button live, which is exactly
// what a person who just failed to reconnect should see.
//
// ── OWNERSHIP ───────────────────────────────────────────────────────────────
//
// Identical to §7.12 / §7.14 / §7.15 / §7.16 / §7.18: unknown vehicleId →
// `404 not_found`, INDISTINGUISHABLE from a vehicle the caller simply cannot
// see; a real ownership mismatch → `403 vehicle_not_owned`. Neither writes
// anything and neither reaches Tesla. The distinction earns its keep here for
// the same reason it does on the pause switch: a VIEWER holding the top `rides`
// tier reaches this endpoint legitimately authenticated and must still be
// refused — the suspension followed the OWNER's inactivity and the bill is the
// owner's, so a rider restarting it would be spending somebody else's money.
type VehicleReconnectHandler struct {
	auth        tokenValidator
	vehicles    VehicleSnapshotReader
	tokens      teslaTokenResolver
	fleet       FleetConfigPusher
	suspensions SuspensionClearer
	logger      *slog.Logger
}

// NewVehicleReconnectHandler wires the owner reconnect handler. fleet may be
// nil (no tesla-http-proxy configured), in which case every call answers
// `502` — the honest answer, since there is no way to install a config.
func NewVehicleReconnectHandler(
	auth tokenValidator,
	vehicles VehicleSnapshotReader,
	tokens teslaTokenResolver,
	fleet FleetConfigPusher,
	suspensions SuspensionClearer,
	logger *slog.Logger,
) *VehicleReconnectHandler {
	return &VehicleReconnectHandler{
		auth:        auth,
		vehicles:    vehicles,
		tokens:      tokens,
		fleet:       fleet,
		suspensions: suspensions,
		logger:      logger,
	}
}

// vehicleReconnectResponse echoes the RESOLVED state so the client can drop its
// disconnect notice without a re-read of §7.0.
//
// `telemetrySuspendedAt` is always null in a 200 body — the call would not have
// succeeded otherwise — and it is emitted ANYWAY, as an explicit null, rather
// than omitted. The field is what the notice is rendered from, so handing back
// the same key the catalog uses lets a client apply one code path to both
// surfaces instead of inferring the clear from an HTTP status. Same reasoning
// the §7.18 ride-share echo gives: the client adopts the SERVER's answer.
type vehicleReconnectResponse struct {
	VehicleID string `json:"vehicleId"`
	// TelemetrySuspendedAt is nil on success. Typed as a pointer so it
	// serialises as `null` rather than as an empty string.
	TelemetrySuspendedAt *string `json:"telemetrySuspendedAt"`
}

// ServeHTTP routes POST only.
func (h *VehicleReconnectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}
	h.handle(w, r)
}

// handle authenticates, verifies ownership, re-installs the config, and clears
// the episode.
//
// NO REQUEST BODY, and none is read. There is one thing this endpoint does and
// no parameter could change it; accepting a body would only create a shape to
// get wrong.
//
// IDEMPOTENT (§4.5). Calling it on a vehicle that was never suspended re-pushes
// the config Tesla already holds and clears an episode that does not exist —
// both no-ops — and answers the same `200`. That matters more than tidiness: an
// owner who reconnects, background-refreshes, and taps again must not be told
// their car is in a state it is not.
func (h *VehicleReconnectHandler) handle(w http.ResponseWriter, r *http.Request) {
	vehicleID := strings.TrimSpace(r.PathValue("vehicleId"))
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
		h.logger.Warn("vehicle reconnect: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	vin, ok := h.resolveOwnedVIN(ctx, w, vehicleID, userID)
	if !ok {
		return
	}

	if !h.pushConfig(ctx, w, vehicleID, userID, vin) {
		return
	}

	if err := h.suspensions.ClearForVehicle(ctx, vehicleID); err != nil {
		// The config IS back. Reporting a failure now would leave the owner
		// pressing a button that has already worked, so this is a 500 the
		// client retries into an idempotent success — but say plainly in the
		// log that the Tesla side is already done.
		h.logger.Error("vehicle reconnect: config re-installed but the suspension stamp survived",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.logger.Info("vehicle telemetry reconnected",
		slog.String("vehicle_id", vehicleID),
		slog.String("user_id", userID),
		slog.String("vin", redactVIN(vin)),
	)

	h.writeJSON(w, http.StatusOK, vehicleReconnectResponse{VehicleID: vehicleID})
}

// resolveOwnedVIN enforces ownership and returns the VIN to push against.
// Matches the §7.12 convention exactly: unknown → 404 (never leak existence),
// mismatch → 403.
func (h *VehicleReconnectHandler) resolveOwnedVIN(
	ctx context.Context, w http.ResponseWriter, vehicleID, userID string,
) (string, bool) {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return "", false
		}
		h.logger.Error("vehicle reconnect: lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return "", false
	}
	if row.UserID != userID {
		h.logger.Warn("vehicle reconnect: ownership mismatch",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
		return "", false
	}
	if row.VIN == "" {
		// A car with no VIN was never streaming and has no config to install.
		// 409 rather than 500: nothing failed, the request simply does not
		// apply to this vehicle.
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeInvalidRequest,
			"this vehicle has no VIN and has never streamed")
		return "", false
	}
	return row.VIN, true
}

// writeJSON / writeError mirror the sibling §7.12 / §7.14 / §7.16 / §7.18
// handlers (rest-api.md §4.1 typed error envelope).
func (h *VehicleReconnectHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

func (h *VehicleReconnectHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
