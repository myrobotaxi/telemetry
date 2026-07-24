package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// FleetVehicle is the minimal per-vehicle identity the owner-onboarding sync
// needs from the Fleet API vehicle list (MYR-257). The Fleet API returns many
// more fields; only these are consumed to seed the Prisma "Vehicle" identity
// columns — live values are filled later by the streaming pipeline.
type FleetVehicle struct {
	// ID is Tesla's numeric vehicle id, serialized as a JSON number; captured
	// as a string for the "Vehicle"."teslaVehicleId" column.
	ID          json.Number `json:"id"`
	VIN         string      `json:"vin"`
	DisplayName string      `json:"display_name"`
	// AccessType is the caller's access level for this vehicle — "OWNER" or
	// "DRIVER" (shared driver). Only OWNER vehicles may be provisioned to the
	// caller; a shared driver must never have someone else's car attached to
	// their account (MYR-257 review finding 3).
	AccessType string `json:"access_type"`
}

// IsOwner reports whether the caller is the vehicle's owner (not a shared
// driver). Tesla returns access_type "OWNER" for owned vehicles; any other
// value (including empty, which older Fleet responses may omit) is treated as
// non-owner and is NOT provisioned.
func (v FleetVehicle) IsOwner() bool {
	return strings.EqualFold(strings.TrimSpace(v.AccessType), "OWNER")
}

// fleetVehicleListResponse mirrors GET /api/1/vehicles.
type fleetVehicleListResponse struct {
	Response []FleetVehicle `json:"response"`
	Count    int            `json:"count"`
}

// ListVehicles returns the vehicles owned by the bearer token's Tesla account
// (GET /api/1/vehicles). Used by the in-app link flow to seed "Vehicle" rows
// server-side so a new owner never needs the web app. VINs are redacted in logs.
func (c *FleetAPIClient) ListVehicles(ctx context.Context, token string) ([]FleetVehicle, error) {
	if token == "" {
		return nil, fmt.Errorf("ListVehicles: auth token is required")
	}

	url := c.baseURL + "/api/1/vehicles"
	respBody, err := c.doWithRetry(ctx, http.MethodGet, url, token, nil)
	if err != nil {
		return nil, fmt.Errorf("ListVehicles: %w", err)
	}

	var result fleetVehicleListResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("ListVehicles: decode response: %w", err)
	}

	c.logger.Debug("fleet vehicle list retrieved", slog.Int("count", len(result.Response)))
	return result.Response, nil
}
