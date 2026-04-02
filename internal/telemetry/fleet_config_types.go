package telemetry

import (
	"context"
	"net/http"
	"strings"
	"time"
)

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

// TeslaTokenRefresher refreshes an expired Tesla OAuth token using a
// stored refresh_token. Implementations should call Tesla's OAuth2 endpoint.
type TeslaTokenRefresher interface {
	Refresh(ctx context.Context, refreshToken string) (TeslaRefreshedToken, error)
}

// TeslaTokenUpdater persists a refreshed Tesla token set to the database.
type TeslaTokenUpdater interface {
	UpdateTeslaToken(ctx context.Context, userID, accessToken, refreshToken string, expiresAt int64) error
}

// TeslaToken holds a Tesla OAuth2 access token with its expiry.
type TeslaToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time // zero value means no expiry info
}

// EndpointConfig describes the telemetry server that vehicles
// should connect to after fleet config is pushed.
type EndpointConfig struct {
	Hostname string
	Port     int
	CA       string // PEM-encoded CA cert
}

// FleetConfigOption configures optional dependencies on FleetConfigHandler.
type FleetConfigOption func(*FleetConfigHandler)

// WithTokenRefresher enables automatic Tesla token refresh when a token is
// expired. Both refresher and updater must be provided for auto-refresh to
// work. The updater persists the refreshed token to the database.
func WithTokenRefresher(refresher TeslaTokenRefresher, updater TeslaTokenUpdater) FleetConfigOption {
	return func(h *FleetConfigHandler) {
		h.refresher = refresher
		h.updater = updater
	}
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
