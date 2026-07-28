package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// defaultRefreshCooldown is the minimum spacing between owner-triggered
// refreshes of the SAME vehicle. Sized well above the wake+read round trip so a
// user mashing the refresh button cannot queue overlapping wakes, and well
// below the MYR-300 freshness window so a genuinely stale car can always be
// refreshed again on the next screen visit.
//
// Burst is 1: unlike the command cooldown (which allows 2 so a UI firing
// lock+climate together isn't rejected) there is no legitimate reason to
// refresh one car twice at once.
const (
	defaultRefreshCooldown = 60 * time.Second
	defaultRefreshBurst    = 1
)

// vehicleRefresher is the consumer-site seam over *VehicleRefresher so the
// handler is unit-testable with a fake — NO live Tesla call ever fires from a
// test.
type vehicleRefresher interface {
	LastStreamAt(vin string) (time.Time, bool)
	Refresh(ctx context.Context, userID, vin string) (RefreshResult, error)
}

// VehicleRefreshHandler serves POST /api/tesla/vehicles/{vehicleId}/refresh —
// the owner-only "bring this car up to date now" endpoint (MYR-315,
// rest-api.md §7.15).
//
// It exists because a car that is asleep or merely quiet stops sending frames,
// and the app has no other way to ask for current state: the WebSocket carries
// only what the car volunteers. Every other surface is passive.
//
// Ownership mirrors the sibling §7.12 teardown and §7.14 plate endpoints
// exactly: an unknown vehicleId is `404 not_found` (indistinguishable from
// ownership-filtered — never leak existence), a real ownership mismatch is
// `403 vehicle_not_owned`. Neither touches Tesla.
//
// Cooldown semantics. The per-vehicle limiter is IN-MEMORY and per-process, so
// it resets on restart and is not shared across replicas — a deliberate
// trade-off: the backstop that actually protects Tesla is the wake budget and
// the single-read shape, and a durable counter would cost a DB round trip on
// every tap for a limit whose only job is to damp a stuck finger. The limiter
// is consumed only on the path that ACTUALLY dials Tesla; a `fresh`
// short-circuit is free and unlimited.
type VehicleRefreshHandler struct {
	auth      tokenValidator
	vehicles  VehicleSnapshotReader
	refresher vehicleRefresher
	cooldown  *vehicleCooldown
	logger    *slog.Logger
}

// NewVehicleRefreshHandler wires the refresh handler.
func NewVehicleRefreshHandler(
	auth tokenValidator,
	vehicles VehicleSnapshotReader,
	refresher vehicleRefresher,
	logger *slog.Logger,
) *VehicleRefreshHandler {
	return &VehicleRefreshHandler{
		auth:      auth,
		vehicles:  vehicles,
		refresher: refresher,
		cooldown:  newVehicleCooldown(defaultRefreshCooldown, defaultRefreshBurst),
		logger:    logger,
	}
}

// vehicleRefreshResponse is the success body. `lastUpdated` is RFC 3339 UTC —
// for a `fresh` result it is the arrival time of the last live frame, for a
// `refreshed` result the completion time of the REST read.
type vehicleRefreshResponse struct {
	Status      string `json:"status"`
	LastUpdated string `json:"lastUpdated"`
}

// ServeHTTP routes POST only.
func (h *VehicleRefreshHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}
	h.handle(w, r)
}

// handle authenticates the caller, verifies ownership, then applies the
// freshness short-circuit → cooldown → wake+read ladder.
func (h *VehicleRefreshHandler) handle(w http.ResponseWriter, r *http.Request) {
	vehicleID := strings.TrimSpace(r.PathValue("vehicleId"))
	if vehicleID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing vehicleId")
		return
	}

	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return
	}

	ctx := r.Context()

	userID, err := h.auth.ValidateToken(ctx, token)
	if err != nil {
		h.logger.Warn("vehicle refresh: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	row, ok := h.authorizeVehicle(ctx, w, vehicleID, userID)
	if !ok {
		return
	}

	// Freshness first, BEFORE the cooldown: a fresh answer costs nothing
	// upstream, so rate-limiting it would only punish a user whose car is
	// working perfectly.
	if at, fresh := h.refresher.LastStreamAt(row.VIN); fresh {
		h.writeJSON(w, http.StatusOK, vehicleRefreshResponse{
			Status:      RefreshStatusFresh,
			LastUpdated: at.UTC().Format(time.RFC3339),
		})
		return
	}

	if !h.cooldown.allow(row.VIN) {
		w.Header().Set("Retry-After", "60")
		h.writeError(w, http.StatusTooManyRequests, wserrors.ErrCodeRateLimited,
			"refresh cooldown: this vehicle was refreshed within the last minute")
		return
	}

	res, err := h.refresher.Refresh(ctx, row.UserID, row.VIN)
	if err != nil {
		h.writeRefreshError(w, row.VIN, err)
		return
	}

	h.writeJSON(w, http.StatusOK, vehicleRefreshResponse{
		Status:      res.Status,
		LastUpdated: res.LastUpdated.UTC().Format(time.RFC3339),
	})
}

// authorizeVehicle loads the vehicle and enforces owner-only access, matching
// the §7.12 / §7.14 convention: unknown vehicle → 404 (never leak existence),
// ownership mismatch → 403.
func (h *VehicleRefreshHandler) authorizeVehicle(ctx context.Context, w http.ResponseWriter, vehicleID, userID string) (VehicleSnapshotRow, bool) {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return VehicleSnapshotRow{}, false
		}
		h.logger.Error("vehicle refresh: lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return VehicleSnapshotRow{}, false
	}
	if row.UserID != userID {
		h.logger.Warn("vehicle refresh: ownership mismatch",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
		return VehicleSnapshotRow{}, false
	}
	return row, true
}

// writeRefreshError maps a refresh failure onto the typed REST envelope.
//
// A *commands.CommandError already carries its status + code (notably the
// 503 vehicle_asleep the wake budget produces), so it passes through unchanged
// — the same translation VehicleCommandHandler does.
//
// A vehicle_data read that fails with Tesla's 408 means the car dropped back to
// sleep between the wake probe and the read; that is the same user-visible
// condition as an exhausted wake budget, so it reports the same transient
// 503 vehicle_asleep rather than a misleading 502.
func (h *VehicleRefreshHandler) writeRefreshError(w http.ResponseWriter, vin string, err error) {
	var cmdErr *commands.CommandError
	switch {
	case errors.As(err, &cmdErr):
		h.logger.Info("vehicle refresh rejected",
			slog.String("vin", redactVIN(vin)),
			slog.String("code", string(cmdErr.Code)),
		)
		h.writeError(w, cmdErr.Status, cmdErr.Code, cmdErr.Message)

	case errors.Is(err, ErrTeslaTokenUnavailable):
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "Tesla account not linked")

	case errors.Is(err, ErrTeslaTokenExpired):
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed,
			"Tesla token expired — re-link your Tesla account")

	case errors.Is(err, ErrVehicleDataRead) && isFleetStatus(err, http.StatusRequestTimeout):
		h.logger.Info("vehicle refresh: car slept between wake and read",
			slog.String("vin", redactVIN(vin)))
		h.writeError(w, http.StatusServiceUnavailable, wserrors.ErrCodeVehicleAsleep,
			"vehicle asleep or offline after wake retries")

	default:
		h.logger.Error("vehicle refresh failed",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusBadGateway, wserrors.ErrCodeCommandFailed, "vehicle refresh failed upstream")
	}
}

// isFleetStatus reports whether err wraps a Fleet API error with the given
// HTTP status.
func isFleetStatus(err error, status int) bool {
	var apiErr *FleetAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

// writeJSON / writeError mirror the sibling §7.12 / §7.14 handlers
// (rest-api.md §4.1 typed error envelope).
func (h *VehicleRefreshHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

func (h *VehicleRefreshHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
