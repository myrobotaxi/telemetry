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

// VehiclePlateWriter persists the already-normalized owner-entered license
// plate on a vehicle the caller owns, reporting whether a row matched.
// Satisfied by *store.VehicleRepo via a thin adapter in cmd/telemetry-server.
//
// Implementations MUST scope the UPDATE to the caller (`WHERE "userId" = …`)
// so the write is fail-closed even if a caller reaches it without the
// handler's ownership check, and MUST NOT re-normalize — normalization is this
// handler's job so the rule lives in exactly one place.
type VehiclePlateWriter interface {
	UpdateLicensePlate(ctx context.Context, userID, vehicleID, plate string) (bool, error)
}

// VehiclePlateHandler serves PUT /api/tesla/vehicles/{vehicleId}/plate — the
// owner-only "what plate is on this car?" write endpoint (MYR-286,
// rest-api.md §7.14).
//
// The plate is NOT sourced from Tesla: the Fleet API exposes no plate on any
// endpoint, telemetry field, or proto, so this endpoint is the ONLY way the
// column is ever populated. It writes the Prisma-owned `Vehicle.licensePlate`
// column through the narrow owner-scoped carve-out documented in
// data-lifecycle.md §1.4 (no migration — the column already exists).
//
// Ownership mirrors the sibling §7.12 teardown exactly: an unknown vehicleId is
// `404 not_found` (indistinguishable from ownership-filtered — never leak
// existence), a real ownership mismatch is `403 vehicle_not_owned`. Neither
// writes anything. The write itself is owner-scoped at the SQL layer too.
//
// Idempotent: PUTting the same plate twice yields the same 200 and the same
// stored value. Submitting an empty plate CLEARS it (writes the empty string) —
// that is a deliberate part of the contract, not a validation failure.
type VehiclePlateHandler struct {
	auth     tokenValidator
	vehicles VehicleSnapshotReader
	plates   VehiclePlateWriter
	logger   *slog.Logger
}

// NewVehiclePlateHandler wires the plate-write handler.
func NewVehiclePlateHandler(
	auth tokenValidator,
	vehicles VehicleSnapshotReader,
	plates VehiclePlateWriter,
	logger *slog.Logger,
) *VehiclePlateHandler {
	return &VehiclePlateHandler{
		auth:     auth,
		vehicles: vehicles,
		plates:   plates,
		logger:   logger,
	}
}

// vehiclePlateRequest is the request body: `{"plate": "abc 1234"}`.
type vehiclePlateRequest struct {
	Plate string `json:"plate"`
}

// vehiclePlateResponse echoes the NORMALIZED stored value so a client can adopt
// it optimistically without re-reading — the plate is delivered by no WebSocket
// frame, so this response is the only immediate source of the new value.
type vehiclePlateResponse struct {
	VehicleID string `json:"vehicleId"`
	// LicensePlate is the stored, normalized plate. Empty string means the
	// plate was cleared — same "no plate set" convention as the read paths.
	LicensePlate string `json:"licensePlate"`
}

// ServeHTTP routes PUT only.
func (h *VehiclePlateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}
	h.handle(w, r)
}

// handle authenticates the caller, verifies ownership, normalizes + validates
// the submitted plate, and writes it.
func (h *VehiclePlateHandler) handle(w http.ResponseWriter, r *http.Request) {
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
		h.logger.Warn("vehicle plate: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	plate, ok := h.decodePlate(w, r)
	if !ok {
		return
	}

	if !h.verifyOwnership(ctx, w, vehicleID, userID) {
		return
	}

	updated, err := h.plates.UpdateLicensePlate(ctx, userID, vehicleID, plate)
	if err != nil {
		h.logger.Error("vehicle plate: update failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}
	if !updated {
		// The ownership check above passed, so a zero-row write means the car was
		// deleted in the gap between the two. Report it as the same not-found the
		// lookup would have produced rather than a false 200.
		h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
		return
	}

	// P1: the plate value itself is NEVER logged (data-classification.md §1.3).
	// Only the P0 ids and a set/cleared shape marker.
	h.logger.Info("vehicle plate updated",
		slog.String("vehicle_id", vehicleID),
		slog.String("user_id", userID),
		slog.Bool("cleared", plate == ""),
	)

	h.writeJSON(w, http.StatusOK, vehiclePlateResponse{VehicleID: vehicleID, LicensePlate: plate})
}

// decodePlate parses the body and returns the NORMALIZED plate. Returns
// ok=false after writing a 400 for malformed JSON or a plate that violates the
// charset / length rule AFTER normalization.
func (h *VehiclePlateHandler) decodePlate(w http.ResponseWriter, r *http.Request) (string, bool) {
	// Strict decode (unknown keys are a 400), matching the sibling
	// RideRequestHandler.decodeCreateBody convention — a typo'd key must fail
	// loudly rather than silently clearing the plate.
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body vehiclePlateRequest
	if err := dec.Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return "", false
	}

	plate := NormalizeLicensePlate(body.Plate)
	if err := ValidateLicensePlate(plate); err != nil {
		// The rejected value is P1 — describe the RULE, never echo the input.
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, err.Error())
		return "", false
	}
	return plate, true
}

// verifyOwnership resolves vehicleId → owner and enforces that the caller owns
// it. Matches the §7.12 teardown convention exactly: unknown vehicle → 404
// (never leak existence), ownership mismatch → 403. Returns false after
// writing the error.
func (h *VehiclePlateHandler) verifyOwnership(ctx context.Context, w http.ResponseWriter, vehicleID, userID string) bool {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return false
		}
		h.logger.Error("vehicle plate: lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return false
	}
	if row.UserID != userID {
		h.logger.Warn("vehicle plate: ownership mismatch",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
		return false
	}
	return true
}

// writeJSON / writeError mirror the VehicleTeardownHandler helpers (rest-api.md
// §4.1 typed error envelope).
func (h *VehiclePlateHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

func (h *VehiclePlateHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
