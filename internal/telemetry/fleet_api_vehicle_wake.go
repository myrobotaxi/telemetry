// `wake_up` on the DIRECT Fleet API — the first step of the MYR-505 zero-touch
// setup completion.
//
// WHY A SECOND WAKE PATH EXISTS. The server already wakes cars, through
// internal/commands' bounded wake+retry budget (§7.9, and the §7.15 refresh
// endpoint reuses it). That path is proxy-preferred because the thing it is
// clearing the way for is a SIGNED command. MYR-505 is clearing the way for a
// fleet-telemetry CONFIG PUSH, and it runs at the one moment in a car's life
// when the virtual key may not be enrolled yet — so a wake that needed the
// signer would be unavailable in exactly the situation it exists for.
//
// `wake_up` NEEDS NO VIRTUAL KEY. It is one of the handful of Fleet API
// endpoints Tesla exempts from command signing, precisely so an unpaired
// vehicle can still be brought online. It is therefore an ordinary
// authenticated call on the READER client, alongside GetVehicle and
// GetTelemetryConfig, and MUST NOT be sent to the tesla-http-proxy.
//
// WHAT THE RESPONSE MEANS, AND WHAT IT DOES NOT. Tesla answers with the vehicle
// object as it stands AT THE MOMENT OF THE CALL, which for a sleeping car is
// `state: "asleep"` — the wake is asynchronous and typically completes seconds
// later. So a caller must read a successful return as "Tesla ACCEPTED the
// wake", never as "the car is now online". MYR-505 treats it exactly that way:
// acceptance is what licenses the config push, and the returned state is
// reported but never used as a gate. Reading it as a gate would make the
// endpoint fail for every genuinely sleeping car — which is all of them.

package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
)

// WakeVehicle asks Tesla to wake the vehicle (POST /api/1/vehicles/{vin}/wake_up).
//
// This is an UNSIGNED authenticated call — `wake_up` is exempt from command
// signing — and MUST target the direct Fleet API base URL, NOT the signing
// tesla-http-proxy. The token must be a valid OAuth2 Bearer token for the
// vehicle's owner.
//
// A nil error means Tesla ACCEPTED the wake, not that the car is awake; the
// returned state is the pre-wake one for a sleeping car. See the file header.
func (c *FleetAPIClient) WakeVehicle(
	ctx context.Context,
	token string,
	vin string,
) (*FleetVehicleState, error) {
	if token == "" {
		return nil, fmt.Errorf("WakeVehicle: auth token is required")
	}
	if len(vin) != vinLength {
		return nil, fmt.Errorf("WakeVehicle: invalid VIN %q (must be 17 characters)", redactVIN(vin))
	}

	url := c.baseURL + "/api/1/vehicles/" + neturl.PathEscape(vin) + "/wake_up"

	c.logger.Debug("waking vehicle", slog.String("vin", redactVIN(vin)))

	// No body: Tesla's wake_up takes none, and doWithRetry sends a nil reader
	// for a nil body so the request carries no stray Content-Type either.
	respBody, err := c.doWithRetry(ctx, http.MethodPost, url, token, nil)
	if err != nil {
		return nil, fmt.Errorf("WakeVehicle(%s): %w", redactVIN(vin), err)
	}

	// The body is the same per-vehicle object GetVehicle decodes. A decode
	// failure is NOT fatal to the caller's purpose — the wake was accepted, and
	// the state is advisory — but it is still an error here rather than a
	// silent zero value, so a caller that DOES care cannot mistake a broken
	// response for `state: ""`.
	var result fleetVehicleGetResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("WakeVehicle(%s): decode response: %w", redactVIN(vin), err)
	}

	return &result.Response, nil
}
