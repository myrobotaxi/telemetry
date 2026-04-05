package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
)

// vinResolverAdapter adapts store.VehicleRepo (returns Vehicle) to the
// ws.VINResolver interface (returns vehicleID string).
type vinResolverAdapter struct {
	repo *store.VehicleRepo
}

func (a *vinResolverAdapter) GetByVIN(ctx context.Context, vin string) (string, error) {
	v, err := a.repo.GetByVIN(ctx, vin)
	if err != nil {
		return "", fmt.Errorf("resolve VIN: %w", err)
	}
	return v.ID, nil
}

// vehicleOwnerAdapter adapts store.VehicleRepo to the
// telemetry.VehicleOwnerLookup interface (returns owning user ID).
type vehicleOwnerAdapter struct {
	repo *store.VehicleRepo
}

func (a *vehicleOwnerAdapter) GetVehicleOwner(ctx context.Context, vin string) (string, error) {
	v, err := a.repo.GetByVIN(ctx, vin)
	if err != nil {
		return "", fmt.Errorf("resolve vehicle owner: %w", err)
	}
	return v.UserID, nil
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
