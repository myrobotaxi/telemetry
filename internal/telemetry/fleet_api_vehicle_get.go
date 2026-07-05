package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
)

// GetTelemetryConfig retrieves a vehicle's current fleet telemetry config
// state from the Fleet API — chiefly the "synced" flag (whether the vehicle
// has received and applied a config). The token must be a valid OAuth2
// Bearer token for the fleet owner.
func (c *FleetAPIClient) GetTelemetryConfig(
	ctx context.Context,
	token string,
	vin string,
) (*FleetConfigStatusResponse, error) {
	if token == "" {
		return nil, fmt.Errorf("GetTelemetryConfig: auth token is required")
	}
	if len(vin) != vinLength {
		return nil, fmt.Errorf("GetTelemetryConfig: invalid VIN %q (must be 17 characters)", redactVIN(vin))
	}

	url := c.baseURL + "/api/1/vehicles/" + neturl.PathEscape(vin) + "/fleet_telemetry_config"

	c.logger.Debug("fetching telemetry config status", slog.String("vin", redactVIN(vin)))

	respBody, err := c.doWithRetry(ctx, http.MethodGet, url, token, nil)
	if err != nil {
		return nil, fmt.Errorf("GetTelemetryConfig(%s): %w", redactVIN(vin), err)
	}

	var result FleetConfigStatusResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("GetTelemetryConfig(%s): decode response: %w", redactVIN(vin), err)
	}

	return &result, nil
}

// GetTelemetryErrors retrieves recent telemetry connection errors for a
// vehicle from the Fleet API. Useful for diagnosing why a vehicle is
// not sending telemetry.
func (c *FleetAPIClient) GetTelemetryErrors(
	ctx context.Context,
	token string,
	vin string,
) (*FleetErrorsResponse, error) {
	if token == "" {
		return nil, fmt.Errorf("GetTelemetryErrors: auth token is required")
	}
	if len(vin) != vinLength {
		return nil, fmt.Errorf("GetTelemetryErrors: invalid VIN %q (must be 17 characters)", redactVIN(vin))
	}

	url := c.baseURL + "/api/1/vehicles/" + neturl.PathEscape(vin) + "/fleet_telemetry_errors"

	c.logger.Debug("fetching telemetry errors",
		slog.String("vin", redactVIN(vin)),
	)

	respBody, err := c.doWithRetry(ctx, http.MethodGet, url, token, nil)
	if err != nil {
		return nil, fmt.Errorf("GetTelemetryErrors(%s): %w", redactVIN(vin), err)
	}

	var result FleetErrorsResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("GetTelemetryErrors(%s): decode response: %w", redactVIN(vin), err)
	}

	c.logger.Debug("telemetry errors retrieved",
		slog.String("vin", redactVIN(vin)),
		slog.Int("count", len(result.Response.Errors)),
	)

	return &result, nil
}
