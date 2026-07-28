package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"time"
)

// ServiceData is the Fleet API service-visit object
// (GET /api/1/vehicles/{vin}/service_data), read while a vehicle is in service
// to learn when it is expected back (MYR-316).
//
// EVERY FIELD IS NULLABLE, and that is the normal case rather than an edge
// case: Tesla returns an all-null body for a service visit with no appointment
// record attached — verified live against a real in-service car. An absent
// `service_etc` is therefore NOT an error, NOT a fetch failure, and NOT a claim
// that the car is back; it simply means Tesla has no estimate to give, and the
// owner-entered fallback takes over.
//
// Every field is a pointer so "absent" and "zero" stay distinguishable, per the
// same rule the vehicle_data decode follows.
type ServiceData struct {
	// ServiceStatus is Tesla's own visit-status string (e.g. "in_service").
	// Read for observability only — Vehicle.status is derived from the
	// authoritative in_service flag on GET /api/1/vehicles/{vin}, not from
	// here, so this never drives a status transition.
	ServiceStatus *string `json:"service_status"`
	// ServiceETC is the estimated time of completion for the current visit.
	// This is the value that becomes serviceEstimatedEndAt when non-null.
	ServiceETC *time.Time `json:"service_etc"`
	// ServiceVisitNumber is Tesla's opaque visit reference.
	ServiceVisitNumber *string `json:"service_visit_number"`
	// StatusID is Tesla's numeric status code for the visit.
	StatusID *int `json:"status_id"`
}

// serviceDataResponse mirrors GET /api/1/vehicles/{vin}/service_data.
type serviceDataResponse struct {
	Response ServiceData `json:"response"`
}

// GetServiceData reads a vehicle's current service-visit record from the Fleet
// API (MYR-316). Like GetVehicle this is an UNSIGNED authenticated read that
// MUST target the direct Fleet API base URL, not the signing tesla-http-proxy.
//
// Callers MUST treat both a nil ServiceETC and an error as "no Tesla estimate"
// and fall back to the owner-entered value — this endpoint is advisory, and a
// service visit is not blocked on our ability to read it.
func (c *FleetAPIClient) GetServiceData(
	ctx context.Context,
	token string,
	vin string,
) (*ServiceData, error) {
	if token == "" {
		return nil, fmt.Errorf("GetServiceData: auth token is required")
	}
	if len(vin) != vinLength {
		return nil, fmt.Errorf("GetServiceData: invalid VIN %q (must be 17 characters)", redactVIN(vin))
	}

	url := c.baseURL + "/api/1/vehicles/" + neturl.PathEscape(vin) + "/service_data"

	c.logger.Debug("fetching vehicle service_data", slog.String("vin", redactVIN(vin)))

	respBody, err := c.doWithRetry(ctx, http.MethodGet, url, token, nil)
	if err != nil {
		return nil, fmt.Errorf("GetServiceData(%s): %w", redactVIN(vin), err)
	}

	var result serviceDataResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		// Never include respBody: the service record can name a service centre
		// and carry a visit reference tied to the owner.
		return nil, fmt.Errorf("GetServiceData(%s): decode response: %w", redactVIN(vin), err)
	}

	return &result.Response, nil
}
