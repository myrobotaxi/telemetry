package telemetry

import (
	"fmt"
	"time"
)

// FleetConfigRequest is the body of POST /api/1/vehicles/{vin}/fleet_telemetry_config.
// Tesla expects "vins" and "config" at the top level.
type FleetConfigRequest struct {
	VINs   []string    `json:"vins"`
	Config FleetConfig `json:"config"`
}

// FleetConfig describes the telemetry server and field selection
// that the vehicle should use.
type FleetConfig struct {
	Hostname   string                 `json:"hostname"`
	Port       int                    `json:"port"`
	CA         *string                `json:"ca"`
	Fields     map[string]FieldConfig `json:"fields"`
	AlertTypes []string               `json:"alert_types,omitempty"`
	Exp        *int64                 `json:"exp,omitempty"`
}

// FieldConfig controls how often a field is emitted and, for spatial
// fields like Location, the minimum change threshold.
type FieldConfig struct {
	IntervalSeconds int      `json:"interval_seconds"`
	MinimumDelta    *float64 `json:"minimum_delta,omitempty"`
}

// FleetConfigResponse is the JSON returned by the Fleet API after
// a successful config push.
type FleetConfigResponse struct {
	Response FleetConfigResult `json:"response"`
}

// FleetConfigResult contains the outcome of a config push.
type FleetConfigResult struct {
	UpdatedVehicles int               `json:"updated_vehicles"`
	SkippedVehicles map[string]string `json:"skipped_vehicles"`
}

// FleetErrorsResponse wraps the list of telemetry errors returned
// by GET /api/1/vehicles/{vin}/fleet_telemetry_errors.
type FleetErrorsResponse struct {
	Response FleetErrorsList `json:"response"`
}

// FleetErrorsList contains individual telemetry errors.
type FleetErrorsList struct {
	Errors []FleetError `json:"errors"`
}

// FleetError describes a single telemetry connection error reported
// by the Fleet API.
type FleetError struct {
	Name      string    `json:"name"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// FleetAPIError is returned when the Fleet API responds with a
// non-success status code.
type FleetAPIError struct {
	StatusCode int
	Body       string
}

func (e *FleetAPIError) Error() string {
	return fmt.Sprintf("fleet API error %d: %s", e.StatusCode, e.Body)
}
