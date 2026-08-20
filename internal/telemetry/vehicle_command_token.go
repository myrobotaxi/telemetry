package telemetry

import (
	"context"
	"errors"
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
// expired (when a refresher is configured). Runs the SHARED on-demand path
// (teslaTokenRefresh), so this surface and the fleet-config handler cannot
// differ on whether a refresh is serialized. On failure it writes the error
// response and returns ok=false.
func (h *VehicleCommandHandler) resolveTeslaToken(ctx context.Context, w http.ResponseWriter, userID string) (TeslaToken, bool) {
	tok, err := teslaTokenRefresh{
		tokens:    h.tokens,
		refresher: h.refresher,
		updater:   h.updater,
		rotator:   h.rotator,
		logger:    h.logger,
	}.resolve(ctx, userID)
	switch {
	case errors.Is(err, ErrTeslaTokenUnavailable):
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "Tesla account not linked")
		return TeslaToken{}, false
	case err != nil:
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "Tesla token expired — re-link your Tesla account")
		return TeslaToken{}, false
	}
	return tok, true
}
