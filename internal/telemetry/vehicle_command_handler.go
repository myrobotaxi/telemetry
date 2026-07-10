package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// maxCommandBody caps the JSON parameter body a command request may carry.
const maxCommandBody = 4 << 10 // 4 KiB — command params are tiny

// commandExecutor is the consumer-site seam over internal/commands.Executor
// so the handler can be unit-tested with a fake.
type commandExecutor interface {
	Execute(ctx context.Context, req commands.Request) (commands.Result, error)
}

// VehicleCommandHandler serves POST /api/vehicles/{vehicleId}/command/{name}:
// the owner-only Tesla vehicle-command surface (MYR-180, rest-api.md §7.9).
// It validates the caller JWT, enforces vehicle ownership, applies a
// per-vehicle command cooldown, resolves the owner's (auto-refreshed) Tesla
// token, and dispatches through the command Executor.
type VehicleCommandHandler struct {
	auth      tokenValidator
	vehicles  VehicleSnapshotReader
	tokens    TeslaTokenProvider
	refresher TeslaTokenRefresher // nil disables auto-refresh
	updater   TeslaTokenUpdater   // nil disables DB persistence of refresh
	executor  commandExecutor
	cooldown  *vehicleCooldown
	logger    *slog.Logger
}

// VehicleCommandOption configures optional dependencies.
type VehicleCommandOption func(*VehicleCommandHandler)

// WithCommandTokenRefresher enables Tesla token auto-refresh, mirroring the
// fleet-config handler's refresh path.
func WithCommandTokenRefresher(refresher TeslaTokenRefresher, updater TeslaTokenUpdater) VehicleCommandOption {
	return func(h *VehicleCommandHandler) {
		h.refresher = refresher
		h.updater = updater
	}
}

// NewVehicleCommandHandler constructs the handler.
func NewVehicleCommandHandler(
	auth tokenValidator,
	vehicles VehicleSnapshotReader,
	tokens TeslaTokenProvider,
	executor commandExecutor,
	logger *slog.Logger,
	opts ...VehicleCommandOption,
) *VehicleCommandHandler {
	h := &VehicleCommandHandler{
		auth:     auth,
		vehicles: vehicles,
		tokens:   tokens,
		executor: executor,
		cooldown: newVehicleCooldown(defaultCommandCooldown, defaultCommandBurst),
		logger:   logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ServeHTTP handles POST /api/vehicles/{vehicleId}/command/{name}.
func (h *VehicleCommandHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}

	vehicleID := r.PathValue("vehicleId")
	name := r.PathValue("name")
	if vehicleID == "" || name == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing vehicleId or command name")
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
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	row, ok := h.authorizeVehicle(ctx, w, vehicleID, userID)
	if !ok {
		return
	}

	if !h.cooldown.allow(vehicleID) {
		w.Header().Set("Retry-After", "2")
		h.writeError(w, http.StatusTooManyRequests, wserrors.ErrCodeRateLimited, "command cooldown: too many commands for this vehicle")
		return
	}

	params, ok := h.decodeParams(w, r)
	if !ok {
		return
	}

	teslaTok, ok := h.resolveTeslaToken(ctx, w, userID)
	if !ok {
		return
	}

	res, err := h.executor.Execute(ctx, commands.Request{
		VIN:         row.VIN,
		Command:     name,
		Params:      params,
		AccessToken: teslaTok.AccessToken,
		Scopes:      commands.ParseScopes(teslaTok.AccessToken),
	})
	if err != nil {
		h.writeCommandError(w, name, row.VIN, err)
		return
	}

	h.logger.Info("vehicle command applied",
		slog.String("vin", redactVIN(row.VIN)),
		slog.String("command", res.Command),
	)
	h.writeJSON(w, http.StatusOK, vehicleCommandResponse{
		Status:  "applied",
		Command: res.Command,
		VIN:     redactVIN(row.VIN),
	})
}

// authorizeVehicle loads the vehicle and enforces owner-only access.
func (h *VehicleCommandHandler) authorizeVehicle(ctx context.Context, w http.ResponseWriter, vehicleID, userID string) (VehicleSnapshotRow, bool) {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return VehicleSnapshotRow{}, false
		}
		h.logger.Error("vehicle command: vehicle lookup failed", slog.String("error", err.Error()))
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return VehicleSnapshotRow{}, false
	}
	if row.UserID != userID {
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
		return VehicleSnapshotRow{}, false
	}
	return row, true
}

// decodeParams reads the optional JSON parameter body. An empty body yields
// nil params (parameterless commands). Malformed JSON is a 400.
func (h *VehicleCommandHandler) decodeParams(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCommandBody))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "could not read request body")
		return nil, false
	}
	if len(body) == 0 {
		return nil, true
	}
	var params map[string]any
	if err := json.Unmarshal(body, &params); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed JSON body")
		return nil, false
	}
	return params, true
}

// writeCommandError translates a *commands.CommandError into the REST error
// envelope. Any non-typed error collapses to a 500.
func (h *VehicleCommandHandler) writeCommandError(w http.ResponseWriter, name, vin string, err error) {
	var cmdErr *commands.CommandError
	if errors.As(err, &cmdErr) {
		h.logger.Info("vehicle command rejected",
			slog.String("vin", redactVIN(vin)),
			slog.String("command", name),
			slog.String("code", string(cmdErr.Code)),
		)
		h.writeError(w, cmdErr.Status, cmdErr.Code, cmdErr.Message)
		return
	}
	h.logger.Error("vehicle command: unexpected error", slog.String("error", err.Error()))
	h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
}

func (h *VehicleCommandHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("vehicle command: writeJSON encode failed", slog.String("error", err.Error()))
	}
}

func (h *VehicleCommandHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}

// vehicleCommandResponse is the success body. VIN is redacted to last-4.
type vehicleCommandResponse struct {
	Status  string `json:"status"`
	Command string `json:"command"`
	VIN     string `json:"vin"`
}
