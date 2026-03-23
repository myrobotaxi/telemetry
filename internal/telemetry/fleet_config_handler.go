package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// vinLength is the standard length of a Vehicle Identification Number.
const vinLength = 17

// tokenValidator validates JWT tokens and returns the authenticated user ID.
// Matches auth.JWTAuthenticator.ValidateToken.
type tokenValidator interface {
	ValidateToken(ctx context.Context, token string) (userID string, err error)
}

// VehicleOwnerLookup resolves a VIN to its owning user ID. Implementations
// should return an error wrapping store.ErrVehicleNotFound when the VIN is
// not registered.
type VehicleOwnerLookup interface {
	GetVehicleOwner(ctx context.Context, vin string) (userID string, err error)
}

// FleetConfigHandler handles POST /api/fleet-config/{vin} requests. It
// validates the caller's JWT, verifies vehicle ownership, and pushes a
// telemetry configuration to the vehicle via the Fleet API proxy.
type FleetConfigHandler struct {
	auth     tokenValidator
	vehicles VehicleOwnerLookup
	fleet    *FleetAPIClient
	hostname string
	port     int
	ca       string
	logger   *slog.Logger
}

// NewFleetConfigHandler creates a handler that pushes fleet telemetry config
// for a single vehicle. The hostname, port, and ca describe the telemetry
// server that the vehicle should connect to after configuration.
func NewFleetConfigHandler(
	auth tokenValidator,
	vehicles VehicleOwnerLookup,
	fleet *FleetAPIClient,
	hostname string,
	port int,
	ca string,
	logger *slog.Logger,
) *FleetConfigHandler {
	return &FleetConfigHandler{
		auth:     auth,
		vehicles: vehicles,
		fleet:    fleet,
		hostname: hostname,
		port:     port,
		ca:       ca,
		logger:   logger,
	}
}

// ServeHTTP handles the fleet config push request.
func (h *FleetConfigHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		h.logger.Warn("fleet config: invalid token",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusUnauthorized, "invalid or expired token")
		return
	}

	ownerID, err := h.vehicles.GetVehicleOwner(ctx, vin)
	if err != nil {
		h.handleVehicleLookupError(w, vin, err)
		return
	}

	if ownerID != userID {
		h.logger.Warn("fleet config: ownership mismatch",
			slog.String("vin", redactVIN(vin)),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, "you do not own this vehicle")
		return
	}

	req := FleetConfigRequest{
		VINs: []string{vin},
		Config: FleetConfig{
			Hostname:    h.hostname,
			Port:        h.port,
			CA:          h.ca,
			Fields:      DefaultFieldConfig(),
			PreferTyped: true,
		},
	}

	result, err := h.fleet.PushTelemetryConfig(ctx, token, req)
	if err != nil {
		h.handleFleetAPIError(w, vin, err)
		return
	}

	h.logger.Info("fleet config pushed",
		slog.String("vin", redactVIN(vin)),
		slog.Int("updated", result.Response.UpdatedVehicles),
		slog.Int("skipped", len(result.Response.SkippedVehicles)),
	)

	if reason, skipped := result.Response.SkippedVehicles[vin]; skipped {
		h.writeError(w, http.StatusConflict, fmt.Sprintf("vehicle skipped: %s", reason))
		return
	}

	h.writeJSON(w, http.StatusOK, fleetConfigResponse{
		Status: "ok",
		VIN:    vin,
	})
}

// handleVehicleLookupError maps vehicle lookup errors to HTTP responses.
func (h *FleetConfigHandler) handleVehicleLookupError(w http.ResponseWriter, vin string, err error) {
	// Check for a "not found" error by inspecting the error chain.
	// We use string matching on the unwrapped sentinel rather than importing
	// store (which would create a dependency from telemetry → store).
	if isNotFoundError(err) {
		h.writeError(w, http.StatusNotFound, "vehicle not found")
		return
	}

	h.logger.Error("fleet config: vehicle lookup failed",
		slog.String("vin", redactVIN(vin)),
		slog.String("error", err.Error()),
	)
	h.writeError(w, http.StatusInternalServerError, "internal error")
}

// handleFleetAPIError maps Fleet API errors to HTTP responses.
func (h *FleetConfigHandler) handleFleetAPIError(w http.ResponseWriter, vin string, err error) {
	var apiErr *FleetAPIError
	if errors.As(err, &apiErr) {
		h.logger.Error("fleet config: proxy error",
			slog.String("vin", redactVIN(vin)),
			slog.Int("status", apiErr.StatusCode),
			slog.String("body", apiErr.Body),
		)
		if apiErr.StatusCode >= 500 {
			h.writeError(w, http.StatusBadGateway, "fleet API error")
			return
		}
		h.writeError(w, http.StatusBadGateway, fmt.Sprintf("fleet API rejected request: %s", apiErr.Body))
		return
	}

	h.logger.Error("fleet config: push failed",
		slog.String("vin", redactVIN(vin)),
		slog.String("error", err.Error()),
	)
	h.writeError(w, http.StatusBadGateway, "failed to reach fleet API proxy")
}

// writeJSON marshals v as JSON and writes it with the given status code.
func (h *FleetConfigHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func (h *FleetConfigHandler) writeError(w http.ResponseWriter, status int, msg string) {
	h.writeJSON(w, status, fleetConfigErrorResponse{Error: msg})
}

// extractBearerToken extracts the token from an "Authorization: Bearer <token>"
// header. Returns empty string if the header is missing or malformed.
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return auth[len(prefix):]
}

// isNotFoundError checks whether err (or any wrapped error) has an Error()
// string containing "not found". This avoids a direct import of the store
// package from the telemetry package.
func isNotFoundError(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if e.Error() == "vehicle not found" {
			return true
		}
	}
	return false
}

// fleetConfigResponse is the JSON body returned on a successful config push.
type fleetConfigResponse struct {
	Status string `json:"status"`
	VIN    string `json:"vin"`
}

// fleetConfigErrorResponse is the JSON body returned when a config push fails.
type fleetConfigErrorResponse struct {
	Error string `json:"error"`
}
