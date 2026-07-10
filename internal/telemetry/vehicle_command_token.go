package telemetry

import (
	"context"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

const (
	// defaultCommandCooldown is the minimum spacing between commands to the
	// same vehicle; defaultCommandBurst allows a small burst so a UI that
	// fires lock+climate together isn't rejected. Per-vehicle, in-memory.
	defaultCommandCooldown = 2 * time.Second
	defaultCommandBurst    = 2
)

// vehicleCooldown is a per-vehicle command rate limiter (token bucket). It
// throttles commands to a single vehicle without capping unrelated
// vehicles, protecting both the car and the Fleet API from command floods.
type vehicleCooldown struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	every    time.Duration
	burst    int
}

func newVehicleCooldown(every time.Duration, burst int) *vehicleCooldown {
	return &vehicleCooldown{
		limiters: make(map[string]*rate.Limiter),
		every:    every,
		burst:    burst,
	}
}

// allow reports whether a command to vehicleID is permitted now, consuming
// a token if so.
func (c *vehicleCooldown) allow(vehicleID string) bool {
	c.mu.Lock()
	lim, ok := c.limiters[vehicleID]
	if !ok {
		lim = rate.NewLimiter(rate.Every(c.every), c.burst)
		c.limiters[vehicleID] = lim
	}
	c.mu.Unlock()
	return lim.Allow()
}

// resolveTeslaToken fetches the caller's Tesla OAuth token, refreshing it if
// expired (when a refresher is configured). Mirrors the fleet-config
// handler's token path so both surfaces share the same refresh semantics.
// On failure it writes the error response and returns ok=false.
func (h *VehicleCommandHandler) resolveTeslaToken(ctx context.Context, w http.ResponseWriter, userID string) (TeslaToken, bool) {
	tok, err := h.tokens.GetTeslaToken(ctx, userID)
	if err != nil {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "Tesla account not linked")
		return TeslaToken{}, false
	}

	if tok.ExpiresAt.IsZero() || !tok.ExpiresAt.Before(time.Now()) {
		return tok, true
	}

	if h.refresher == nil || tok.RefreshToken == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "Tesla token expired — re-link your Tesla account")
		return TeslaToken{}, false
	}

	refreshed, err := h.refresher.Refresh(ctx, tok.RefreshToken)
	if err != nil {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "Tesla token expired — re-link your Tesla account")
		return TeslaToken{}, false
	}

	expiresAt := refreshed.ExpiresAt()
	if h.updater != nil {
		if uErr := h.updater.UpdateTeslaToken(ctx, userID, refreshed.AccessToken, refreshed.RefreshToken, expiresAt.Unix()); uErr != nil {
			h.logger.Error("vehicle command: failed to persist refreshed token")
		}
	}

	return TeslaToken{
		AccessToken:  refreshed.AccessToken,
		RefreshToken: refreshed.RefreshToken,
		ExpiresAt:    expiresAt,
	}, true
}
