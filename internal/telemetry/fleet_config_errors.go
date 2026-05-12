package telemetry

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// handleTeslaTokenError maps Tesla token lookup errors to HTTP responses.
func (h *FleetConfigHandler) handleTeslaTokenError(w http.ResponseWriter, userID string, err error) {
	if errors.Is(err, sdk.ErrNotFound) {
		h.logger.Warn("fleet config: Tesla token not found",
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "Tesla account not linked — connect your Tesla account first")
		return
	}

	h.logger.Error("fleet config: Tesla token lookup failed",
		slog.String("user_id", userID),
		slog.String("error", err.Error()),
	)
	h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
}

// handleVehicleLookupError maps vehicle lookup errors to HTTP responses.
func (h *FleetConfigHandler) handleVehicleLookupError(w http.ResponseWriter, vin string, err error) {
	if errors.Is(err, sdk.ErrNotFound) {
		h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
		return
	}

	h.logger.Error("fleet config: vehicle lookup failed",
		slog.String("vin", redactVIN(vin)),
		slog.String("error", err.Error()),
	)
	h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
}

// handleFleetAPIError maps Fleet API errors to HTTP responses.
func (h *FleetConfigHandler) handleFleetAPIError(w http.ResponseWriter, vin string, err error) {
	var apiErr *FleetAPIError
	if errors.As(err, &apiErr) {
		h.logger.Error("fleet config: proxy error",
			slog.String("vin", redactVIN(vin)),
			slog.Int("status", apiErr.StatusCode),
			slog.String("body", apiErr.Body),
		)
		if apiErr.StatusCode >= 500 {
			h.writeError(w, http.StatusBadGateway, wserrors.ErrCodeInternalError, "fleet API error")
			return
		}
		h.writeError(w, http.StatusBadGateway, wserrors.ErrCodeInternalError, fmt.Sprintf("fleet API rejected request: %s", apiErr.Body))
		return
	}

	h.logger.Error("fleet config: push failed",
		slog.String("vin", redactVIN(vin)),
		slog.String("error", err.Error()),
	)
	h.writeError(w, http.StatusBadGateway, wserrors.ErrCodeInternalError, "failed to reach fleet API proxy")
}
