package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
)

// debugFieldsMinTokenLen is the minimum length required for
// DEBUG_FIELDS_TOKEN when the endpoint is enabled in non-dev mode.
// 32 chars ≈ 192 bits of entropy for a base64-encoded random token;
// anything shorter is likely a typo or a test fixture and should be
// rejected at startup rather than shipped to prod.
const debugFieldsMinTokenLen = 32

// debugFieldsGate describes how /api/debug/fields should be mounted at
// startup. It is derived from the --dev flag and the DEBUG_FIELDS_TOKEN
// env var by resolveDebugFieldsGate.
type debugFieldsGate struct {
	// Enabled is true when the endpoint should be mounted.
	Enabled bool
	// Token is the shared secret the client must present; empty means
	// auth is skipped (only valid under --dev).
	Token string
	// Reason is a short human string ("dev mode" or "token set") used
	// in the startup log to tell operators which gate is active.
	Reason string
}

// errDebugFieldsTokenTooShort is returned when a non-dev operator sets
// DEBUG_FIELDS_TOKEN to a value shorter than debugFieldsMinTokenLen.
var errDebugFieldsTokenTooShort = errors.New("DEBUG_FIELDS_TOKEN must be at least 32 chars in non-dev mode")

// resolveDebugFieldsGate folds the --dev flag and DEBUG_FIELDS_TOKEN
// value into a single decision about whether to mount the debug-fields
// endpoint and how to authenticate clients. Rules:
//
//   - --dev:              endpoint mounted, token optional
//   - no --dev + no token: endpoint NOT mounted
//   - no --dev + token:   endpoint mounted, token required; fails if token < 32 chars
func resolveDebugFieldsGate(devMode bool, token string) (debugFieldsGate, error) {
	switch {
	case devMode && token != "":
		return debugFieldsGate{Enabled: true, Token: token, Reason: "dev mode + DEBUG_FIELDS_TOKEN"}, nil
	case devMode:
		return debugFieldsGate{Enabled: true, Reason: "dev mode (no token required)"}, nil
	case token == "":
		return debugFieldsGate{}, nil
	case len(token) < debugFieldsMinTokenLen:
		return debugFieldsGate{}, fmt.Errorf("%w (got %d chars)", errDebugFieldsTokenTooShort, len(token))
	default:
		return debugFieldsGate{Enabled: true, Token: token, Reason: "DEBUG_FIELDS_TOKEN set"}, nil
	}
}

// devLocalhostOriginPatterns is the localhost-friendly fallback used when
// --dev is set and the operator has not configured an explicit origin
// allow-list. Hostname-only patterns match any scheme (http/ws), and the
// "localhost:*" / "127.0.0.1:*" forms admit any port (Next.js typically
// dials from :3000 or :3001 during local development).
var devLocalhostOriginPatterns = []string{
	"localhost",
	"localhost:*",
	"127.0.0.1",
	"127.0.0.1:*",
	"[::1]",
	"[::1]:*",
}

// resolveWSOriginPatterns folds the operator-configured allow-list and
// the --dev flag into the final OriginPatterns slice passed to the
// coder/websocket Accept call. Rules per NFR-3.22 / MYR-17:
//
//   - configured AllowedOrigins takes precedence in every mode.
//   - empty config + --dev: localhost defaults so a dev workstation
//     can dial without ceremony (still fails-closed for any other
//     cross-origin host).
//   - empty config + production: empty slice. coder/websocket's
//     authenticateOrigin permits same-origin and empty-Origin requests
//     and rejects all cross-origin dials with HTTP 403. A loud Warn
//     reminds the operator to set websocket.allowed_origins or
//     WEBSOCKET_ALLOWED_ORIGINS rather than rely on this fallback.
func resolveWSOriginPatterns(configured []string, devMode bool, logger *slog.Logger) []string {
	if len(configured) > 0 {
		logger.Info("websocket allowed origins configured",
			slog.Int("count", len(configured)),
			slog.Any("patterns", configured),
		)
		return configured
	}
	if devMode {
		logger.Warn("websocket allowed origins not configured; --dev fallback admits localhost only",
			slog.Any("patterns", devLocalhostOriginPatterns),
		)
		// Return a copy so callers cannot mutate the package-level slice.
		out := make([]string, len(devLocalhostOriginPatterns))
		copy(out, devLocalhostOriginPatterns)
		return out
	}
	logger.Warn("websocket allowed origins not configured in production mode — all cross-origin connections will be rejected with HTTP 403; set websocket.allowed_origins or WEBSOCKET_ALLOWED_ORIGINS to admit a browser app")
	return nil
}

// vinResolverAdapter adapts store.VINCache to the ws.VINResolver interface
// (returns vehicleID string). Backing the WS broadcaster path with the cache
// avoids fetching the full Vehicle row (including the heavy navRouteCoordinates
// JSON) on every telemetry frame — the slim two-column query runs once per VIN
// for the lifetime of the process.
type vinResolverAdapter struct {
	cache *store.VINCache
}

func (a *vinResolverAdapter) GetByVIN(ctx context.Context, vin string) (string, error) {
	id, err := a.cache.ResolveID(ctx, vin)
	if err != nil {
		return "", fmt.Errorf("resolve VIN: %w", err)
	}
	return id, nil
}

// vehicleOwnerAdapter adapts store.VINCache to the
// telemetry.VehicleOwnerLookup interface (returns owning user ID). Shares
// the same cache instance as vinResolverAdapter, so a single DB lookup per
// VIN serves both ID and owner resolution for the lifetime of the process.
type vehicleOwnerAdapter struct {
	cache *store.VINCache
}

func (a *vehicleOwnerAdapter) GetVehicleOwner(ctx context.Context, vin string) (string, error) {
	userID, err := a.cache.ResolveOwner(ctx, vin)
	if err != nil {
		return "", fmt.Errorf("resolve vehicle owner: %w", err)
	}
	return userID, nil
}

// teslaTokenAdapter adapts store.AccountRepo to the
// telemetry.TeslaTokenProvider interface, converting the store-layer
// TeslaOAuthToken (Unix epoch) to the telemetry-layer TeslaToken (time.Time).
type teslaTokenAdapter struct {
	repo *store.AccountRepo
}

func (a *teslaTokenAdapter) GetTeslaToken(ctx context.Context, userID string) (telemetry.TeslaToken, error) {
	dbTok, err := a.repo.GetTeslaToken(ctx, userID)
	if err != nil {
		return telemetry.TeslaToken{}, err
	}

	var expiresAt time.Time
	if dbTok.ExpiresAt != nil {
		expiresAt = time.Unix(*dbTok.ExpiresAt, 0)
	}

	return telemetry.TeslaToken{
		AccessToken:  dbTok.AccessToken,
		RefreshToken: dbTok.RefreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}

// teslaTokenUpdaterAdapter adapts store.AccountRepo to the
// telemetry.TeslaTokenUpdater interface for persisting refreshed tokens.
type teslaTokenUpdaterAdapter struct {
	repo *store.AccountRepo
}

func (a *teslaTokenUpdaterAdapter) UpdateTeslaToken(ctx context.Context, userID, accessToken, refreshToken string, expiresAt int64) error {
	return a.repo.UpdateTeslaToken(ctx, userID, accessToken, refreshToken, expiresAt)
}

// proxyTimeout matches the default Fleet API timeout for consistency.
const proxyTimeout = 30 * time.Second

// proxyHTTPClient returns an *http.Client suitable for calling the
// tesla-http-proxy sidecar. When the proxy URL is a loopback address
// (127.0.0.1, localhost, or ::1), TLS certificate verification is
// skipped because the proxy uses a self-signed cert and traffic never
// leaves the machine. This is the standard pattern for tesla-http-proxy
// sidecar deployments (TeslaMate, Home Assistant, etc.).
//
// For non-loopback URLs, returns nil (FleetAPIClient uses its default client).
func proxyHTTPClient(proxyURL string, logger *slog.Logger) *http.Client {
	u, err := url.Parse(proxyURL)
	if err != nil {
		logger.Warn("invalid proxy URL — using default HTTP client",
			slog.String("proxy_url", proxyURL),
			slog.String("error", err.Error()),
		)
		return nil
	}

	host := u.Hostname()
	ip := net.ParseIP(host)
	isLoopback := host == "localhost" || (ip != nil && ip.IsLoopback())

	if !isLoopback {
		return nil
	}

	logger.Info("proxy on loopback — skipping TLS verification",
		slog.String("proxy_url", proxyURL),
	)

	return &http.Client{
		Timeout: proxyTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //#nosec G402 -- loopback only; guard above ensures non-loopback uses verified TLS
			},
		},
	}
}


// vehicleAuthorizerAdapter adapts store.VINCache to the
// telemetry.VehicleAuthorizer interface (data-lifecycle.md §3.5,
// MYR-73). The receiver consults this on every inbound mTLS upgrade
// to reject frames for VINs whose Vehicle row has been deleted. A
// cache miss → DB fetch → cache fill (or cached miss for unknown
// VINs); subsequent calls are O(1).
type vehicleAuthorizerAdapter struct {
	cache *store.VINCache
}

// IsAuthorized returns true when the VIN has a matching Vehicle row.
// errors.Is(err, store.ErrVehicleNotFound) is treated as the
// authoritative "not authorized" signal and reported as (false, nil)
// so the receiver can branch on the boolean alone. Other errors
// (transient DB outages) propagate so the receiver fails open per
// the contract.
func (a *vehicleAuthorizerAdapter) IsAuthorized(ctx context.Context, vin string) (bool, error) {
	if vin == "" {
		return false, nil
	}
	_, err := a.cache.ResolveID(ctx, vin)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, store.ErrVehicleNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("vehicleAuthorizerAdapter.IsAuthorized(vin redacted): %w", err)
}
