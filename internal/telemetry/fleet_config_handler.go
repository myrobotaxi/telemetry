package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// vinLength is the standard length of a Vehicle Identification Number.
const vinLength = 17

// FleetConfigHandler handles POST /api/fleet-config/{vin} requests. It
// validates the caller's JWT, verifies vehicle ownership, and pushes a
// telemetry configuration to the vehicle via the Fleet API proxy.
type FleetConfigHandler struct {
	auth      tokenValidator
	vehicles  VehicleOwnerLookup
	tokens    TeslaTokenProvider
	refresher TeslaTokenRefresher // nil disables auto-refresh
	updater   TeslaTokenUpdater   // nil disables DB updates after refresh
	fleet     *FleetAPIClient
	endpoint  EndpointConfig
	logger    *slog.Logger
}

// NewFleetConfigHandler creates a handler that pushes fleet telemetry config
// for a single vehicle. The endpoint describes the telemetry server that the
// vehicle should connect to after configuration. The tokens provider is used
// to fetch the user's Tesla OAuth token for authenticating with the Fleet API.
// The refresher and updater are optional — pass nil to disable auto-refresh.
func NewFleetConfigHandler(
	auth tokenValidator,
	vehicles VehicleOwnerLookup,
	tokens TeslaTokenProvider,
	fleet *FleetAPIClient,
	endpoint EndpointConfig,
	logger *slog.Logger,
	opts ...FleetConfigOption,
) *FleetConfigHandler {
	h := &FleetConfigHandler{
		auth:     auth,
		vehicles: vehicles,
		tokens:   tokens,
		fleet:    fleet,
		endpoint: endpoint,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
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

	var ca *string
	if h.endpoint.CA != "" {
		ca = &h.endpoint.CA
	}

	// Tesla requires exp between ~31 and ~360 days from now.
	expTime := time.Now().Add(350 * 24 * time.Hour).Unix()

	req := FleetConfigRequest{
		VINs: []string{vin},
		Config: FleetConfig{
			Hostname:   h.endpoint.Hostname,
			Port:       h.endpoint.Port,
			CA:         ca,
			Fields:     DefaultFieldConfig(),
			AlertTypes: []string{"service"},
			Exp:        &expTime,
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
// expiry. If the token is expired and a refresh_token is available, it
// attempts to refresh the token automatically. Returns the token and true
// on success. On failure it writes an HTTP error response and returns false.
func (h *FleetConfigHandler) resolveTeslaToken(ctx context.Context, w http.ResponseWriter, userID string) (TeslaToken, bool) {
	tok, err := h.tokens.GetTeslaToken(ctx, userID)
	if err != nil {
		h.handleTeslaTokenError(w, userID, err)
		return TeslaToken{}, false
	}

	if tok.ExpiresAt.IsZero() || !tok.ExpiresAt.Before(time.Now()) {
		return tok, true
	}

	// Token is expired — attempt auto-refresh if possible.
	if refreshed, ok := h.tryRefreshToken(ctx, w, userID, tok); ok {
		return refreshed, true
	}
	return TeslaToken{}, false
}

// tryRefreshToken attempts to refresh an expired Tesla token. Returns the
// refreshed token and true on success. On failure it writes an HTTP error
// and returns false.
func (h *FleetConfigHandler) tryRefreshToken(ctx context.Context, w http.ResponseWriter, userID string, tok TeslaToken) (TeslaToken, bool) {
	if h.refresher == nil || tok.RefreshToken == "" {
		h.logger.Warn("fleet config: Tesla token expired, no refresh available",
			slog.String("user_id", userID),
			slog.Time("expired_at", tok.ExpiresAt),
			slog.Bool("has_refresher", h.refresher != nil),
			slog.Bool("has_refresh_token", tok.RefreshToken != ""),
		)
		h.writeError(w, http.StatusUnauthorized,
			"Tesla token expired — re-link your Tesla account")
		return TeslaToken{}, false
	}

	h.logger.Info("fleet config: refreshing expired Tesla token",
		slog.String("user_id", userID),
		slog.Time("expired_at", tok.ExpiresAt),
	)

	refreshed, err := h.refresher.Refresh(ctx, tok.RefreshToken)
	if err != nil {
		h.logger.Error("fleet config: Tesla token refresh failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusUnauthorized,
			"Tesla token expired — re-link your Tesla account")
		return TeslaToken{}, false
	}

	// Persist the refreshed token if an updater is available.
	if h.updater != nil {
		expiresAt := refreshed.ExpiresAt()
		if err := h.updater.UpdateTeslaToken(ctx, userID,
			refreshed.AccessToken, refreshed.RefreshToken, expiresAt.Unix()); err != nil {
			h.logger.Error("fleet config: failed to persist refreshed token",
				slog.String("user_id", userID),
				slog.String("error", err.Error()),
			)
			// Continue with the refreshed token even if persistence fails.
		}
	}

	return TeslaToken{
		AccessToken:  refreshed.AccessToken,
		RefreshToken: refreshed.RefreshToken,
		ExpiresAt:    refreshed.ExpiresAt(),
	}, true
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
