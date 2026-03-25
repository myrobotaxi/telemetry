package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/pkg/sdk"
)

// VehicleStatusHandler handles GET /api/vehicle-status/{vin} requests. It
// validates the caller's JWT, verifies vehicle ownership, and returns the
// vehicle's current connection status. The frontend polls this endpoint
// during onboarding to detect when a vehicle connects.
type VehicleStatusHandler struct {
	auth     tokenValidator
	vehicles VehicleOwnerLookup
	presence VehiclePresence
	logger   *slog.Logger
}

// NewVehicleStatusHandler creates a handler that returns real-time vehicle
// connection status. The presence provider is typically the telemetry Receiver.
func NewVehicleStatusHandler(
	auth tokenValidator,
	vehicles VehicleOwnerLookup,
	presence VehiclePresence,
	logger *slog.Logger,
) *VehicleStatusHandler {
	return &VehicleStatusHandler{
		auth:     auth,
		vehicles: vehicles,
		presence: presence,
		logger:   logger,
	}
}

// vehicleStatusResponse is the JSON body returned by the vehicle status endpoint.
type vehicleStatusResponse struct {
	VIN            string  `json:"vin"`
	Connected      bool    `json:"connected"`
	LastMessageAt  *string `json:"last_message_at"`
	MessageCount   int64   `json:"message_count"`
	ConnectedSince *string `json:"connected_since"`
}

// vehicleStatusErrorResponse is the JSON body returned on errors.
type vehicleStatusErrorResponse struct {
	Error string `json:"error"`
}

// ServeHTTP handles the vehicle status request.
func (h *VehicleStatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	vin := r.PathValue("vin")
	if len(vin) != vinLength {
		h.writeError(w, http.StatusBadRequest, "invalid VIN: must be 17 characters")
		return
	}

	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, "missing Authorization header")
		return
	}

	ctx := r.Context()

	userID, err := h.auth.ValidateToken(ctx, token)
	if err != nil {
		h.logger.Warn("vehicle status: invalid token",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusUnauthorized, "invalid or expired token")
		return
	}

	if !h.verifyOwnership(ctx, w, vin, userID) {
		return
	}

	resp := h.buildStatusResponse(vin)
	h.writeJSON(w, http.StatusOK, resp)
}

// verifyOwnership checks that userID owns the vehicle identified by vin.
// Returns true if the ownership check passes. On failure it writes an HTTP
// error response and returns false.
func (h *VehicleStatusHandler) verifyOwnership(ctx context.Context, w http.ResponseWriter, vin, userID string) bool {
	ownerID, err := h.vehicles.GetVehicleOwner(ctx, vin)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "vehicle not found")
			return false
		}
		h.logger.Error("vehicle status: vehicle lookup failed",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}

	if ownerID != userID {
		h.logger.Warn("vehicle status: ownership mismatch",
			slog.String("vin", redactVIN(vin)),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, "you do not own this vehicle")
		return false
	}

	return true
}

// buildStatusResponse queries the presence provider and constructs the
// JSON response. Times are formatted as RFC 3339 or null when unavailable.
func (h *VehicleStatusHandler) buildStatusResponse(vin string) vehicleStatusResponse {
	resp := vehicleStatusResponse{
		VIN: redactVIN(vin),
	}

	info, connected := h.presence.ConnectionInfo(vin)
	if !connected {
		return resp
	}

	resp.Connected = true
	resp.MessageCount = info.MessageCount

	since := info.ConnectedSince.Format(time.RFC3339)
	resp.ConnectedSince = &since

	if !info.LastMessageAt.IsZero() {
		last := info.LastMessageAt.Format(time.RFC3339)
		resp.LastMessageAt = &last
	}

	return resp
}

// writeJSON marshals v as JSON and writes it with the given status code.
func (h *VehicleStatusHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

// writeError writes a JSON error response.
func (h *VehicleStatusHandler) writeError(w http.ResponseWriter, status int, msg string) {
	h.writeJSON(w, status, vehicleStatusErrorResponse{Error: msg})
}
