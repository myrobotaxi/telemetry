package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
)

// DeleteTelemetryConfig removes a vehicle's fleet telemetry configuration
// via the Fleet API — the inverse of PushTelemetryConfig. After the config
// is removed the vehicle stops streaming to us once it re-reads its config /
// reconnects (not guaranteed instantaneous for an already-open stream; our
// receiver rejects the deleted VIN immediately regardless — see the
// vehicle_deleted dispatcher).
//
// It is a near-verbatim copy of GetTelemetryConfig, NOT of
// PushTelemetryConfig: a DELETE carries no config body to sign, so the
// tesla-http-proxy's handleFleetTelemetryConfig (which reads+signs a
// {vins, config} body on POST) does not apply — the request falls through
// to the proxy's plain forward and reaches Tesla unmodified with the bearer
// token, exactly like the per-VIN GET status read we already ship. Same
// client, same BaseURL (cfg.Proxy().URL), same per-VIN path, no JWS.
//
// The token must be a valid OAuth2 Bearer token for the fleet owner.
// Callers treat any error (incl. a 404 for an already-deleted config) as
// best-effort / non-fatal: the authoritative teardown is the local DB
// transaction, and a stale config self-expires at Tesla while our receiver
// rejects the stream regardless (docs/architecture/car-offboarding.md §5.2).
func (c *FleetAPIClient) DeleteTelemetryConfig(
	ctx context.Context,
	token string,
	vin string,
) error {
	if token == "" {
		return fmt.Errorf("DeleteTelemetryConfig: auth token is required")
	}
	if len(vin) != vinLength {
		return fmt.Errorf("DeleteTelemetryConfig: invalid VIN %q (must be 17 characters)", redactVIN(vin))
	}

	url := c.baseURL + "/api/1/vehicles/" + neturl.PathEscape(vin) + "/fleet_telemetry_config"

	c.logger.Debug("deleting telemetry config", slog.String("vin", redactVIN(vin)))

	// The response body carries no information we act on (success or a Tesla
	// error the caller already treats as non-fatal), so it is discarded.
	if _, err := c.doWithRetry(ctx, http.MethodDelete, url, token, nil); err != nil {
		return fmt.Errorf("DeleteTelemetryConfig(%s): %w", redactVIN(vin), err)
	}

	c.logger.Info("telemetry config deleted", slog.String("vin", redactVIN(vin)))
	return nil
}
