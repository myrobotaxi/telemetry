// `fleet_status` — Tesla's authoritative answer to "is our virtual key
// enrolled on this car?", added for the MYR-505 zero-touch setup completion.
//
// WHY THIS AND NOT AN INFERENCE. Every pairing signal the server had before
// this was INDIRECT and arrived only as a side effect of doing something else:
// Tesla's `missing_key` skip on a config push (MYR-448), or a signed command
// that applied (MYR-489). Both are proof when they happen, and neither can be
// ASKED. MYR-505 needs to ask — the owner has just come back from the Tesla app
// and is watching a progress card — so it needs the one Fleet API call whose
// entire job is to answer the question.
//
// IT IS A TESLA-SIDE RECORD, NOT A CAR QUERY, and that property is what makes
// the whole endpoint honest. `fleet_status` reports what Tesla's fleet database
// knows about key enrollment; it does not wake, dial, or otherwise depend on the
// car being reachable. So a car that never comes out of sleep still yields a
// TRUE answer about its pairing, which is why MYR-505 never has to collapse
// "we could not reach it" into "you have not paired" — those are different
// answers and this call keeps them separate.
//
// UNSIGNED AND THEREFORE A READER CALL. It is a POST only because it carries a
// VIN list; there is no command to sign and no virtual key involved, so it MUST
// target the direct Fleet API base URL and NOT the tesla-http-proxy — the same
// split GetTelemetryConfig and GetVehicle document. Sending it to the proxy
// would ask the signer to sign a request that has no vehicle command in it.

package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// FleetStatus is the subset of Tesla's `fleet_status` response body that
// MyRoboTaxi reads: the pairing partition of the requested VINs.
//
// Tesla also returns a `vehicle_info` map (firmware version, telemetry version,
// key count). It is deliberately NOT modelled — nothing in MYR-505 acts on it,
// and every field it carries is another thing that can echo a full VIN into a
// decode error.
type FleetStatus struct {
	// KeyPairedVINs lists the requested VINs on which the application's
	// virtual key IS enrolled.
	KeyPairedVINs []string `json:"key_paired_vins"`
	// UnpairedVINs lists the requested VINs on which it is NOT.
	UnpairedVINs []string `json:"unpaired_vins"`
}

// FleetStatusResponse mirrors POST /api/1/vehicles/fleet_status.
type FleetStatusResponse struct {
	Response FleetStatus `json:"response"`
}

// KeyPaired reports whether vin appears in the paired partition.
//
// POSITIVE MEMBERSHIP ONLY, deliberately. The alternative — "not in
// unpaired_vins" — would read a VIN Tesla omitted from BOTH lists (an unknown
// VIN, a VIN the token cannot see, a response we half-decoded) as paired, and
// the consequence of a false positive here is a delete-then-create against a
// real customer car. A false negative merely leaves the owner on the
// `awaiting_virtual_key` card they were already looking at.
func (r *FleetStatusResponse) KeyPaired(vin string) bool {
	if r == nil {
		return false
	}
	for _, v := range r.Response.KeyPairedVINs {
		if v == vin {
			return true
		}
	}
	return false
}

// fleetStatusRequest is the request body: a VIN list.
type fleetStatusRequest struct {
	VINs []string `json:"vins"`
}

// GetFleetStatus asks Tesla which of the given VINs have the application's
// virtual key enrolled.
//
// This is an UNSIGNED authenticated call and MUST target the direct Fleet API
// base URL, NOT the signing tesla-http-proxy (see the file header). The token
// must be a valid OAuth2 Bearer token for the vehicles' owner.
func (c *FleetAPIClient) GetFleetStatus(
	ctx context.Context,
	token string,
	vins []string,
) (*FleetStatusResponse, error) {
	if token == "" {
		return nil, fmt.Errorf("GetFleetStatus: auth token is required")
	}
	if len(vins) == 0 {
		return nil, fmt.Errorf("GetFleetStatus: no VINs provided")
	}
	for _, vin := range vins {
		if len(vin) != vinLength {
			return nil, fmt.Errorf("GetFleetStatus: invalid VIN %q (must be 17 characters)", redactVIN(vin))
		}
	}

	body, err := json.Marshal(fleetStatusRequest{VINs: vins})
	if err != nil {
		return nil, fmt.Errorf("GetFleetStatus: marshal request: %w", err)
	}

	url := c.baseURL + "/api/1/vehicles/fleet_status"

	c.logger.Debug("fetching fleet status",
		slog.String("vin", redactVIN(vins[0])),
		slog.Int("vin_count", len(vins)))

	respBody, err := c.doWithRetry(ctx, http.MethodPost, url, token, body)
	if err != nil {
		return nil, fmt.Errorf("GetFleetStatus(%s): %w", redactVIN(vins[0]), err)
	}

	var result FleetStatusResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("GetFleetStatus(%s): decode response: %w", redactVIN(vins[0]), err)
	}

	return &result, nil
}
