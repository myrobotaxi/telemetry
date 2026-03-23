package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/pkg/sdk"
)

// vinLength is the standard length of a Vehicle Identification Number.
const vinLength = 17

// tokenValidator validates JWT tokens and returns the authenticated user ID.
// Matches auth.JWTAuthenticator.ValidateToken.
type tokenValidator interface {
	ValidateToken(ctx context.Context, token string) (userID string, err error)
}

// VehicleOwnerLookup resolves a VIN to its owning user ID. Implementations
// should return an error wrapping sdk.ErrNotFound when the VIN is not
// registered.
type VehicleOwnerLookup interface {
	GetVehicleOwner(ctx context.Context, vin string) (userID string, err error)
}

// TeslaTokenProvider retrieves the Tesla OAuth access token for a user.
// The token is read from the database (stored during Tesla account linking).
type TeslaTokenProvider interface {
	GetTeslaToken(ctx context.Context, userID string) (TeslaToken, error)
}

// TeslaToken holds a Tesla OAuth2 access token with its expiry.
type TeslaToken struct {
	AccessToken string
	ExpiresAt   time.Time // zero value means no expiry info
}

// EndpointConfig describes the telemetry server that vehicles
// should connect to after fleet config is pushed.
type EndpointConfig struct {
	Hostname string
	Port     int
	CA       string // PEM-encoded CA cert
}

// FleetConfigHandler handles POST /api/fleet-config/{vin} requests. It
// validates the caller's JWT, verifies vehicle ownership, and pushes a
// telemetry configuration to the vehicle via the Fleet API proxy.
type FleetConfigHandler struct {
	auth     tokenValidator
	vehicles VehicleOwnerLookup
	tokens   TeslaTokenProvider
	fleet    *FleetAPIClient
	endpoint EndpointConfig
	logger   *slog.Logger
}

// NewFleetConfigHandler creates a handler that pushes fleet telemetry config
// for a single vehicle. The endpoint describes the telemetry server that the
// vehicle should connect to after configuration. The tokens provider is used
// to fetch the user's Tesla OAuth token for authenticating with the Fleet API.
func NewFleetConfigHandler(
	auth tokenValidator,
	vehicles VehicleOwnerLookup,
	tokens TeslaTokenProvider,
	fleet *FleetAPIClient,
	endpoint EndpointConfig,
	logger *slog.Logger,
) *FleetConfigHandler {
	return &FleetConfigHandler{
		auth:     auth,
		vehicles: vehicles,
		tokens:   tokens,
		fleet:    fleet,
		endpoint: endpoint,
		logger:   logger,
	}
}

// ServeHTTP handles the fleet config push request.
func (h *FleetConfigHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

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

	if !h.verifyOwnership(ctx, w, vin, userID) {
		return
	}

	teslaTok, ok := h.resolveTeslaToken(ctx, w, userID)
	if !ok {
		return
	}

	req := FleetConfigRequest{
		VINs: []string{vin},
		Config: FleetConfig{
			Hostname:   h.endpoint.Hostname,
			Port:       h.endpoint.Port,
			CA:         h.endpoint.CA,
			Fields:     DefaultFieldConfig(),
			AlertTypes: []string{"service"},
		},
	}

	result, err := h.fleet.PushTelemetryConfig(ctx, teslaTok.AccessToken, req)
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
		Status: "configured",
		VIN:    redactVIN(vin),
	})
}

// verifyOwnership checks that userID owns the vehicle identified by vin.
// Returns true if the ownership check passes. On failure it writes an HTTP
// error response and returns false.
func (h *FleetConfigHandler) verifyOwnership(ctx context.Context, w http.ResponseWriter, vin, userID string) bool {
	ownerID, err := h.vehicles.GetVehicleOwner(ctx, vin)
	if err != nil {
		h.handleVehicleLookupError(w, vin, err)
		return false
	}

	if ownerID != userID {
		h.logger.Warn("fleet config: ownership mismatch",
			slog.String("vin", redactVIN(vin)),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, "you do not own this vehicle")
		return false
	}

	return true
}

// resolveTeslaToken fetches the user's Tesla OAuth token and validates its
// expiry. Returns the token and true on success. On failure it writes an
// HTTP error response and returns false.
func (h *FleetConfigHandler) resolveTeslaToken(ctx context.Context, w http.ResponseWriter, userID string) (TeslaToken, bool) {
	tok, err := h.tokens.GetTeslaToken(ctx, userID)
	if err != nil {
		h.handleTeslaTokenError(w, userID, err)
		return TeslaToken{}, false
	}

	if !tok.ExpiresAt.IsZero() && tok.ExpiresAt.Before(time.Now()) {
		h.logger.Warn("fleet config: Tesla token expired",
			slog.String("user_id", userID),
			slog.Time("expired_at", tok.ExpiresAt),
		)
		h.writeError(w, http.StatusUnauthorized, "Tesla token expired — re-link your Tesla account")
		return TeslaToken{}, false
	}

	return tok, true
}

// handleTeslaTokenError maps Tesla token lookup errors to HTTP responses.
func (h *FleetConfigHandler) handleTeslaTokenError(w http.ResponseWriter, userID string, err error) {
	if errors.Is(err, sdk.ErrNotFound) {
		h.logger.Warn("fleet config: Tesla token not found",
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusUnauthorized, "Tesla account not linked — connect your Tesla account first")
		return
	}

	h.logger.Error("fleet config: Tesla token lookup failed",
		slog.String("user_id", userID),
		slog.String("error", err.Error()),
	)
	h.writeError(w, http.StatusInternalServerError, "internal error")
}

// handleVehicleLookupError maps vehicle lookup errors to HTTP responses.
func (h *FleetConfigHandler) handleVehicleLookupError(w http.ResponseWriter, vin string, err error) {
	if errors.Is(err, sdk.ErrNotFound) {
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
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
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

// fleetConfigResponse is the JSON body returned on a successful config push.
type fleetConfigResponse struct {
	Status string `json:"status"`
	VIN    string `json:"vin"`
}

// fleetConfigErrorResponse is the JSON body returned when a config push fails.
type fleetConfigErrorResponse struct {
	Error string `json:"error"`
}
