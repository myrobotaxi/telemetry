package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
)

const defaultFleetTelemetryPort = 443

func runFleetConfig(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("fleet-config requires a subcommand (show | push)")
	}
	switch args[0] {
	case "show":
		return runFleetConfigShow(args[1:])
	case "push":
		return runFleetConfigPush(ctx, args[1:])
	default:
		return fmt.Errorf("unknown fleet-config subcommand %q", args[0])
	}
}

func runFleetConfigShow(args []string) error {
	fs := flag.NewFlagSet("fleet-config show", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return writeJSON(os.Stdout, telemetry.DefaultFieldConfig())
}

// fleetPushOutput mirrors the server's fleet config endpoint but is kept
// local to the CLI — there is no shared schema contract for this view.
type fleetPushOutput struct {
	VIN             string            `json:"vin"`
	UserID          string            `json:"userId"`
	Refreshed       bool              `json:"tokenRefreshed"`
	UpdatedVehicles int               `json:"updatedVehicles"`
	SkippedVehicles map[string]string `json:"skippedVehicles,omitempty"`
}

func runFleetConfigPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fleet-config push", flag.ContinueOnError)
	vin := fs.String("vin", "", "17-character Tesla VIN")
	userID := fs.String("user-id", "", "MyRoboTaxi user id (owner of the VIN)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireFlag("vin", *vin); err != nil {
		return err
	}
	if err := requireFlag("user-id", *userID); err != nil {
		return err
	}
	if len(*vin) != 17 {
		return fmt.Errorf("invalid --vin: must be 17 characters, got %d", len(*vin))
	}

	endpoint, err := loadEndpointConfig()
	if err != nil {
		return err
	}
	proxyURL := os.Getenv("TESLA_PROXY_URL")
	if proxyURL == "" {
		return fmt.Errorf("TESLA_PROXY_URL is required for fleet-config push")
	}

	logger := newLogger()
	db, err := openDB(ctx, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	accountRepo, err := newAccountRepo(db)
	if err != nil {
		return err
	}
	vehicleRepo := store.NewVehicleRepo(db.Pool(), store.NoopMetrics{})

	if err := verifyVINOwnership(ctx, vehicleRepo, *vin, *userID); err != nil {
		return err
	}

	token, refreshed, err := resolveTeslaToken(ctx, logger, accountRepo, *userID)
	if err != nil {
		return err
	}

	resp, err := pushFleetConfig(ctx, logger, proxyURL, endpoint, *vin, token)
	if err != nil {
		return err
	}

	return writeJSON(os.Stdout, fleetPushOutput{
		VIN:             *vin,
		UserID:          *userID,
		Refreshed:       refreshed,
		UpdatedVehicles: resp.Response.UpdatedVehicles,
		SkippedVehicles: resp.Response.SkippedVehicles,
	})
}

// verifyVINOwnership fails fast if the VIN is not registered to the given
// user. Prevents pushing fleet config for vehicles the operator does not own.
func verifyVINOwnership(ctx context.Context, repo *store.VehicleRepo, vin, userID string) error {
	v, err := repo.GetByVIN(ctx, vin)
	if err != nil {
		return fmt.Errorf("lookup vehicle: %w", err)
	}
	if v.UserID != userID {
		return fmt.Errorf("vehicle owner mismatch: VIN belongs to user %q, not %q", v.UserID, userID)
	}
	return nil
}

// pushFleetConfig constructs the FleetConfigRequest and calls the tesla
// Fleet API via the proxy.
func pushFleetConfig(
	ctx context.Context,
	logger *slog.Logger,
	proxyURL string,
	endpoint telemetry.EndpointConfig,
	vin, token string,
) (*telemetry.FleetConfigResponse, error) {
	client := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL:    proxyURL,
		HTTPClient: proxyHTTPClient(proxyURL, logger),
	}, logger.With(slog.String("component", "fleet-api")))

	expTime := time.Now().Add(350 * 24 * time.Hour).Unix()
	var ca *string
	if endpoint.CA != "" {
		ca = &endpoint.CA
	}
	req := telemetry.FleetConfigRequest{
		VINs: []string{vin},
		Config: telemetry.FleetConfig{
			Hostname:   endpoint.Hostname,
			Port:       endpoint.Port,
			CA:         ca,
			Fields:     telemetry.DefaultFieldConfig(),
			AlertTypes: []string{"service"},
			Exp:        &expTime,
		},
	}
	resp, err := client.PushTelemetryConfig(ctx, token, req)
	if err != nil {
		return nil, fmt.Errorf("push fleet config: %w", err)
	}
	return resp, nil
}

// loadEndpointConfig reads the Fleet Telemetry endpoint coordinates from
// the environment, applying the same default port (443) as the server.
func loadEndpointConfig() (telemetry.EndpointConfig, error) {
	hostname := os.Getenv("FLEET_TELEMETRY_HOSTNAME")
	if hostname == "" {
		return telemetry.EndpointConfig{}, fmt.Errorf("FLEET_TELEMETRY_HOSTNAME is required for fleet-config push")
	}
	port := defaultFleetTelemetryPort
	if v := os.Getenv("FLEET_TELEMETRY_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p <= 0 {
			return telemetry.EndpointConfig{}, fmt.Errorf("invalid FLEET_TELEMETRY_PORT %q", v)
		}
		port = p
	}
	return telemetry.EndpointConfig{
		Hostname: hostname,
		Port:     port,
		CA:       os.Getenv("FLEET_TELEMETRY_CA"),
	}, nil
}

// resolveTeslaToken reads the Tesla token from the DB and, if it is
// expired or missing credentials, attempts to refresh it.
func resolveTeslaToken(
	ctx context.Context,
	logger *slog.Logger,
	accountRepo *store.AccountRepo,
	userID string,
) (accessToken string, didRefresh bool, err error) {
	tok, err := accountRepo.GetTeslaToken(ctx, userID)
	if err != nil {
		return "", false, fmt.Errorf("read tesla token: %w", err)
	}
	expiresAt := tokenExpiry(tok.ExpiresAt)
	if !shouldRefresh(expiresAt) {
		return tok.AccessToken, false, nil
	}
	refreshed, err := refreshToken(ctx, logger, accountRepo, userID, tok.RefreshToken)
	if err != nil {
		if errors.Is(err, errRefreshSkipped) {
			return tok.AccessToken, false, nil
		}
		return "", false, err
	}
	return refreshed.AccessToken, true, nil
}

// proxyHTTPClient mirrors the server's tesla-http-proxy handling: when the
// proxy URL is on loopback, certificate verification is skipped because
// the proxy uses a self-signed cert. Non-loopback URLs use the default
// HTTP client (verified TLS).
func proxyHTTPClient(proxyURL string, logger *slog.Logger) *http.Client {
	u, err := url.Parse(proxyURL)
	if err != nil {
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
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //#nosec G402 -- loopback only; guard above ensures non-loopback uses verified TLS
			},
		},
	}
}
