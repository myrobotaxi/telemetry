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

// RideShareWriter persists the OWNER's ride-sharing switch for a vehicle.
// Satisfied by *store.VehicleRepo.SetRideShareEnabled.
//
// Implementations MUST assign unconditionally — `false` is a decision, not an
// absence — and MUST NOT route the write through the shared control-state
// upsert, which telemetry frames also travel down (see
// internal/store/vehicle_ride_share.go for why that would be an access-control
// bug rather than a style one).
type RideShareWriter interface {
	SetRideShareEnabled(ctx context.Context, vehicleID string, enabled bool) error
}

// VehicleRideShareHandler serves
// PUT /api/tesla/vehicles/{vehicleId}/ride-share — the owner's switch for
// whether their car accepts ride requests at all (MYR-342, rest-api.md §7.18).
//
// Why it exists: before this endpoint an owner had two ways to stop riders
// booking their car and neither was a switch. They could mark it `in_service`
// — a lie about where the car is, and one that MYR-313 deliberately lets
// SCHEDULED rides through anyway — or they could decline every incoming request
// by hand, forever. Nothing said "the car is fine, I am simply not lending it
// out right now".
//
// The endpoint is a purely LOCAL owner-scoped write to the Go-owned
// go_vehicle_control_state side table (migration 0021): no tesla-http-proxy, no
// Tesla token, no Tesla call, and so it is ALWAYS MOUNTED, exactly like the
// §7.16 service-window sibling it is modelled on. There is no Tesla concept of
// "this owner is lending their car out" to talk to.
//
// Ownership mirrors the sibling §7.12 / §7.14 / §7.15 / §7.16 endpoints exactly:
// unknown vehicleId → `404 not_found`, INDISTINGUISHABLE from a vehicle the
// caller simply cannot see (never leak existence); a real ownership mismatch →
// `403 vehicle_not_owned`. NEITHER writes anything. The distinction matters more
// here than on most siblings, because a viewer holding even the top `rides`
// share tier reaches this endpoint legitimately-authenticated and must still be
// refused: the pause is the OWNER's switch, and a rider being able to flip it
// would invert the feature.
type VehicleRideShareHandler struct {
	auth      tokenValidator
	vehicles  VehicleSnapshotReader
	rideShare RideShareWriter
	logger    *slog.Logger
}

// NewVehicleRideShareHandler wires the owner ride-share toggle handler.
func NewVehicleRideShareHandler(
	auth tokenValidator,
	vehicles VehicleSnapshotReader,
	rideShare RideShareWriter,
	logger *slog.Logger,
) *VehicleRideShareHandler {
	return &VehicleRideShareHandler{
		auth:      auth,
		vehicles:  vehicles,
		rideShare: rideShare,
		logger:    logger,
	}
}

// vehicleRideShareRequest is the request body: `{"enabled": false}`.
//
// A POINTER so an ABSENT key is distinguishable from an explicit `false`. It is
// REQUIRED here, unlike the §7.16 service-window body where absence means
// "clear": this field has no clear. Both values are decisions, so a body that
// names neither is a client bug and gets a 400 rather than being silently read
// as one of them — guessing wrong would either un-pause a car its owner paused
// or withdraw a car its owner never touched.
type vehicleRideShareRequest struct {
	Enabled *bool `json:"enabled"`
}

// vehicleRideShareResponse echoes the RESOLVED state so a client can adopt it
// without a re-read — there is no WebSocket delta for this field, and the toggle
// that just moved is the thing least able to afford a stale position.
type vehicleRideShareResponse struct {
	VehicleID string `json:"vehicleId"`
	// Enabled is the stored value. This server writes exactly what was asked,
	// so it always equals the request — the field is echoed anyway because the
	// contract's rule is that the client adopts the SERVER's answer, which
	// keeps a future server free to refuse or coerce without breaking clients.
	Enabled bool `json:"enabled"`
}

// ServeHTTP routes PUT only.
func (h *VehicleRideShareHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}
	h.handle(w, r)
}

// handle authenticates the caller, decodes the switch, verifies ownership, and
// writes.
//
// ORDER IS DELIBERATE and matches §7.16: the body is decoded BEFORE the
// ownership lookup, so a malformed request from a stranger costs a 400 and no
// database read, and — more importantly — a malformed request from the owner
// never reaches the write.
func (h *VehicleRideShareHandler) handle(w http.ResponseWriter, r *http.Request) {
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
		h.logger.Warn("ride share: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	enabled, ok := h.decodeEnabled(w, r)
	if !ok {
		return
	}

	if !h.verifyOwnership(ctx, w, vehicleID, userID) {
		return
	}

	if err := h.rideShare.SetRideShareEnabled(ctx, vehicleID, enabled); err != nil {
		h.logger.Error("ride share: write failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.logger.Info("ride sharing updated",
		slog.String("vehicle_id", vehicleID),
		slog.String("user_id", userID),
		slog.Bool("enabled", enabled),
	)

	h.writeJSON(w, http.StatusOK, vehicleRideShareResponse{
		VehicleID: vehicleID,
		Enabled:   enabled,
	})
}

// decodeEnabled parses and validates the body. Returns ok=false after writing a
// 400.
//
// Unknown keys are refused (DisallowUnknownFields), matching the schema's
// additionalProperties:false and the sibling §7.16 decoder. An EMPTY body is a
// 400 here rather than the §7.16 "clear": there is no third state to fall back
// to, so silence is ambiguous rather than expressive.
func (h *VehicleRideShareHandler) decodeEnabled(w http.ResponseWriter, r *http.Request) (bool, bool) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var body vehicleRideShareRequest
	if err := dec.Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return false, false
	}
	if body.Enabled == nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "enabled is required")
		return false, false
	}
	return *body.Enabled, true
}

// verifyOwnership resolves vehicleId → owner and enforces that the caller owns
// it. Matches the §7.12 / §7.14 / §7.16 convention exactly: unknown vehicle →
// 404 (never leak existence), ownership mismatch → 403.
//
// This check is the ONLY gate on the write. The side table has no userId column,
// so — as with the service-window writer — there is no owner-scoped SQL
// predicate underneath to catch a mistake here. It is also the sole reason a
// share-holding VIEWER cannot pause somebody else's car: no share tier is
// consulted anywhere on this path, deliberately, because there is no tier that
// should grant it.
func (h *VehicleRideShareHandler) verifyOwnership(ctx context.Context, w http.ResponseWriter, vehicleID, userID string) bool {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return false
		}
		h.logger.Error("ride share: lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return false
	}
	if row.UserID != userID {
		h.logger.Warn("ride share: ownership mismatch",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
		return false
	}
	return true
}

// writeJSON / writeError mirror the sibling §7.12 / §7.14 / §7.15 / §7.16
// handlers (rest-api.md §4.1 typed error envelope).
func (h *VehicleRideShareHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

func (h *VehicleRideShareHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
