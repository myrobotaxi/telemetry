package telemetry

import (
	"net/http"
	"time"
)

// fleetConfigStatusResponse is the GET /api/fleet-config/{vin} body. Synced
// comes straight from Tesla. The expiry fields are populated only when Tesla
// echoes the config's `exp` (undocumented but often present); when it does
// not, ExpiresAt is empty and DaysRemaining is nil, and the UI falls back to
// the Synced flag to convey "streaming vs not".
type fleetConfigStatusResponse struct {
	VIN           string `json:"vin"`
	Synced        bool   `json:"synced"`
	Hostname      string `json:"hostname,omitempty"`
	Port          int    `json:"port,omitempty"`
	ExpiresAt     string `json:"expiresAt,omitempty"` // RFC3339; empty if exp unknown
	Expired       bool   `json:"expired"`
	DaysRemaining *int   `json:"daysRemaining,omitempty"` // nil if expired or exp unknown
}

// handleStatus handles GET /api/fleet-config/{vin} — reports the vehicle's
// current telemetry-config state (Tesla's synced flag plus best-effort
// expiry) so the UI can show whether a re-push is needed.
func (h *FleetConfigHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	vin, teslaTok, ok := h.authorize(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	status, err := h.fleet.GetTelemetryConfig(ctx, teslaTok.AccessToken, vin)
	if err != nil {
		h.handleFleetAPIError(w, vin, err)
		return
	}

	resp := fleetConfigStatusResponse{
		VIN:    redactVIN(vin),
		Synced: status.Response.Synced,
	}
	if cfg := status.Response.Config; cfg != nil {
		resp.Hostname = cfg.Hostname
		resp.Port = cfg.Port
		if cfg.Exp != nil {
			expiresAt := time.Unix(*cfg.Exp, 0).UTC()
			remaining := time.Until(expiresAt)
			resp.ExpiresAt = expiresAt.Format(time.RFC3339)
			resp.Expired = remaining <= 0
			// DaysRemaining is only meaningful while the config is still
			// valid; leave it nil once expired so the UI branches on
			// `expired` instead of rendering a negative countdown. The
			// int() truncates toward zero (30.7d → 30), which reads as
			// "days fully remaining".
			if !resp.Expired {
				days := int(remaining.Hours() / 24)
				resp.DaysRemaining = &days
			}
		}
	}

	h.writeJSON(w, http.StatusOK, resp)
}
