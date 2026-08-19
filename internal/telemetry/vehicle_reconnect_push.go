package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// The Tesla half of §7.28's reconnect, split from the handler to keep both files
// inside the 300-line cap.
//
// Every status here is borrowed rather than invented, so a client that already
// handles the §7.11 link and the §7.9 command surface handles this one with no
// new branches:
//
//	401 auth_failed   — no Tesla account linked (fleet_config_errors.go's rule).
//	409 conflict      — Tesla ACCEPTED the request and SKIPPED the car, almost
//	                    always `missing_key`. The same 409 the §7.10 fleet-config
//	                    push returns for the same answer.
//	502 internal_error— Tesla or the proxy failed. The same 502 the fleet-config
//	                    push and the §7.23 complete-setup return.
//
// There is deliberately NO retry ladder. The FleetAPIClient already retries 429s
// and 5xx internally, and a second ladder here would turn a person tapping a
// button into a long-running request they cannot cancel.

// pushConfig re-installs the vehicle's fleet-telemetry config. Returns false
// after writing an error response.
func (h *VehicleReconnectHandler) pushConfig(
	ctx context.Context, w http.ResponseWriter, vehicleID, userID, vin string,
) bool {
	if h.fleet == nil {
		// No tesla-http-proxy configured. The route is mounted anyway (see the
		// handler doc) so that a proxy-less deployment answers honestly instead
		// of 404-ing an endpoint the client's disconnect notice links to.
		h.logger.Error("vehicle reconnect: no fleet-config path is wired",
			slog.String("vehicle_id", vehicleID))
		h.writeError(w, http.StatusBadGateway, wserrors.ErrCodeInternalError,
			"telemetry configuration is unavailable right now")
		return false
	}

	tok, err := h.tokens.Resolve(ctx, userID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			// The owner unlinked their Tesla account while suspended. There is
			// no grant to act under, and the honest next step is re-linking —
			// not this endpoint — so it wears the same 401 the fleet-config
			// push uses for the same condition.
			h.logger.Warn("vehicle reconnect: Tesla account not linked",
				slog.String("vehicle_id", vehicleID),
				slog.String("user_id", userID))
			h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed,
				"Tesla account not linked — connect your Tesla account first")
			return false
		}
		h.logger.Error("vehicle reconnect: Tesla token lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
			slog.String("error", err.Error()))
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return false
	}

	if err := h.fleet.PushForVIN(ctx, tok.AccessToken, vin); err != nil {
		h.writePushError(w, vehicleID, vin, err)
		return false
	}
	return true
}

// writePushError maps a failed config push onto the borrowed statuses above.
//
// THE SKIPPED-VEHICLE CASE IS THE ONE WORTH GETTING RIGHT. Tesla answers `200`
// with `skipped_vehicles: {vin: "missing_key"}` when the virtual key is not
// enrolled, and PushForVIN converts that into a *SkippedVehicleError. It is NOT
// a server fault and NOT retryable by tapping again — the owner has to re-pair
// the key in the Tesla app — so it must not wear the 502 that invites a retry
// loop. The reason string travels in the message because it is Tesla's own
// short label ("missing_key"), which is the only thing that tells a support
// conversation what actually happened.
func (h *VehicleReconnectHandler) writePushError(w http.ResponseWriter, vehicleID, vin string, err error) {
	var skipped *SkippedVehicleError
	if errors.As(err, &skipped) {
		h.logger.Warn("vehicle reconnect: Tesla skipped the vehicle",
			slog.String("vehicle_id", vehicleID),
			slog.String("vin", redactVIN(vin)),
			slog.String("reason", skipped.Reason))
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeInvalidRequest,
			fmt.Sprintf("vehicle skipped: %s", skipped.Reason))
		return
	}

	var apiErr *FleetAPIError
	if errors.As(err, &apiErr) {
		h.logger.Error("vehicle reconnect: fleet API rejected the push",
			slog.String("vehicle_id", vehicleID),
			slog.String("vin", redactVIN(vin)),
			slog.Int("status", apiErr.StatusCode))
		h.writeError(w, http.StatusBadGateway, wserrors.ErrCodeInternalError,
			"could not reconnect telemetry — try again in a moment")
		return
	}

	h.logger.Error("vehicle reconnect: config push failed",
		slog.String("vehicle_id", vehicleID),
		slog.String("vin", redactVIN(vin)),
		slog.String("error", err.Error()))
	h.writeError(w, http.StatusBadGateway, wserrors.ErrCodeInternalError,
		"could not reconnect telemetry — try again in a moment")
}
