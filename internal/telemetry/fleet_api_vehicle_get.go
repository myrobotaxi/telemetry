package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
)

// FleetVehicleState is the subset of the Fleet API per-vehicle object
// (GET /api/1/vehicles/{vin}) that MyRoboTaxi reads for the in-service
// status derivation (MYR-259). Tesla's REST vehicle object is the
// authoritative source of the `in_service` bool — it is NOT present in the
// fleet-telemetry stream except via the pushed ServiceMode field (159).
type FleetVehicleState struct {
	VIN string `json:"vin"`
	// State is Tesla's connectivity state: "online", "asleep", "offline".
	State string `json:"state"`
	// InService is true while the vehicle is flagged in service by Tesla
	// (e.g. a service center or tech has it in service mode).
	InService bool `json:"in_service"`
}

// fleetVehicleGetResponse mirrors GET /api/1/vehicles/{vin}.
type fleetVehicleGetResponse struct {
	Response FleetVehicleState `json:"response"`
}

// GetVehicle reads a single vehicle's REST object from the Fleet API
// (GET /api/1/vehicles/{vin}) and returns the state + in_service flag
// (MYR-259). This is an UNSIGNED authenticated read — the same auth style
// as ListVehicles / GetTelemetryConfig — and MUST target the direct Fleet
// API base URL, NOT the signing tesla-http-proxy. The token must be a
// valid OAuth2 Bearer token for the vehicle's owner.
func (c *FleetAPIClient) GetVehicle(
	ctx context.Context,
	token string,
	vin string,
) (*FleetVehicleState, error) {
	if token == "" {
		return nil, fmt.Errorf("GetVehicle: auth token is required")
	}
	if len(vin) != vinLength {
		return nil, fmt.Errorf("GetVehicle: invalid VIN %q (must be 17 characters)", redactVIN(vin))
	}

	url := c.baseURL + "/api/1/vehicles/" + neturl.PathEscape(vin)

	c.logger.Debug("fetching vehicle rest object", slog.String("vin", redactVIN(vin)))

	respBody, err := c.doWithRetry(ctx, http.MethodGet, url, token, nil)
	if err != nil {
		return nil, fmt.Errorf("GetVehicle(%s): %w", redactVIN(vin), err)
	}

	var result fleetVehicleGetResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("GetVehicle(%s): decode response: %w", redactVIN(vin), err)
	}

	return &result.Response, nil
}

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
