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

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
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

// parseFleetConfigPushInput validates the subcommand's arguments and
// resolves the operator handle.
//
// Split out of runFleetConfigPush purely to keep that function under the
// cyclomatic budget once MYR-447 added the audit path. Every branch here is
// an argument check, so grouping them leaves the push flow readable as a
// sequence of steps rather than a wall of guards.
func parseFleetConfigPushInput(vin, userID string) (string, error) {
	if err := requireFlag("vin", vin); err != nil {
		return "", err
	}
	if err := requireFlag("user-id", userID); err != nil {
		return "", err
	}
	if len(vin) != 17 {
		return "", fmt.Errorf("invalid --vin: must be 17 characters, got %d", len(vin))
	}
	return requireOperator()
}

func runFleetConfigPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fleet-config push", flag.ContinueOnError)
	vin := fs.String("vin", "", "17-character Tesla VIN")
	userID := fs.String("user-id", "", "MyRoboTaxi user id (owner of the VIN)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	operator, err := parseFleetConfigPushInput(*vin, *userID)
	if err != nil {
		return err
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
	vehicleRepo, err := newVehicleRepo(db, logger)
	if err != nil {
		return err
	}

	// MYR-447. Two audit rows, not one, and the split is forced by which
	// subject is KNOWABLE at which moment.
	//
	// The vehicle lookup is keyed on the VIN alone, so the operator's
	// --user-id is an unverified claim until the row comes back. Writing a
	// single row against that claim before the lookup — which is what this
	// did first — records the wrong data subject whenever the two disagree:
	// the operator names user A, the row decrypted belongs to user B, and B
	// (whose location columns were actually read) has no audit trail of it.
	// That is precisely the accountability the feature is sold on, failing
	// in the case it most matters: a wrong or malicious --user-id.
	//
	// So the vehicle-row decrypt is audited against the OWNER the lookup
	// resolves — after the read, before the value is used or the command
	// proceeds. Nothing is printed or transmitted in between; this matches
	// `ops fields snapshot`, which is VIN-keyed for the same reason and
	// audits at the same point.
	auditor := newOperatorAuditor(db)
	owner, err := auditedVehicleOwner(ctx, auditor, vehicleRepo, operator, *vin)
	if err != nil {
		return err
	}
	if owner != *userID {
		return fmt.Errorf("vehicle owner mismatch: VIN belongs to user %q, not %q", owner, *userID)
	}

	// The token decrypt IS knowable in advance — the credentials belong to
	// the account being acted on by definition — so this one stays
	// strictly fail-closed: written BEFORE the resolve that decrypts the
	// tokens and transmits them to Tesla. Not printed, but "not printed"
	// is not "not accessed".
	if err := auditor.RecordDecrypt(ctx, store.OperatorAccess{
		Operator:   operator,
		Command:    "ops fleet-config push",
		UserID:     *userID,
		TargetType: store.OperatorTargetUser,
		TargetID:   *userID,
		Fields:     teslaTokenAuditFields,
	}); err != nil {
		return fmt.Errorf("record operator decrypt: %w", err)
	}

	token, refreshed, err := resolveTeslaToken(ctx, logger, accountRepo, *userID)
	if err != nil {
		return err
	}

	resp, err := pushFleetConfig(ctx, logger, proxyURL, endpoint, *vin, token)
	if err != nil {
		return err
	}

	if err := writeJSON(os.Stdout, fleetPushOutput{
		VIN:             *vin,
		UserID:          *userID,
		Refreshed:       refreshed,
		UpdatedVehicles: resp.Response.UpdatedVehicles,
		SkippedVehicles: resp.Response.SkippedVehicles,
	}); err != nil {
		return err
	}

	// A skip is Tesla saying "200, and I did nothing" — most often because the
	// owner has not paired the virtual key. Reporting that as success is the
	// MYR-448 bug in CLI form: an operator runs the command, sees no error, and
	// believes the car is configured. Exit non-zero so scripts and humans both
	// notice. The JSON above is still written first so the reason is visible.
	return telemetry.SkipErrorFor(resp, *vin)
}

// verifyVINOwnership fails fast if the VIN is not registered to the given
// user. Prevents pushing fleet config for vehicles the operator does not own.
// auditedVehicleOwner resolves a VIN to its owning user id and records the
// decrypt that resolving it performs.
//
// GetByVIN decrypts the row's whole location surface — coordinates, the
// geocoded labels MYR-447 sealed, the nav polyline — so the read is an
// operator decrypt whether or not the caller goes on to use it. The audit
// row is written against the owner the lookup returned rather than against
// anything the operator asserted, and before the id is handed back, so a
// caller cannot act on the result without the access having been recorded.
//
// It returns the owner rather than a bool so the caller can compare and
// report the mismatch itself; folding the comparison in here would mean
// this function decided policy for every caller.
func auditedVehicleOwner(
	ctx context.Context,
	auditor *store.OperatorAuditor,
	repo *store.VehicleRepo,
	operator, vin string,
) (string, error) {
	v, err := repo.GetByVIN(ctx, vin)
	if err != nil {
		return "", fmt.Errorf("lookup vehicle: %w", err)
	}
	if err := auditor.RecordDecrypt(ctx, store.OperatorAccess{
		Operator:   operator,
		Command:    "ops fleet-config push",
		UserID:     v.UserID,
		TargetType: store.OperatorTargetVehicle,
		TargetID:   v.ID,
		Fields:     vehicleRowAuditFields,
	}); err != nil {
		return "", fmt.Errorf("record operator decrypt: %w", err)
	}
	return v.UserID, nil
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
