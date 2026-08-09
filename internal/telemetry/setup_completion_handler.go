// POST /api/tesla/vehicles/{vehicleId}/complete-setup — the owner-only,
// idempotent, rate-limited trigger for MYR-505's zero-touch setup completion
// (rest-api.md §7.23).
//
// Ownership semantics are the §7.12 / §7.14 / §7.15 convention verbatim: an
// unknown vehicleId is `404 not_found`, indistinguishable from an
// ownership-filtered one so the endpoint never leaks existence, and a real
// mismatch is `403 vehicle_not_owned`. Neither touches Tesla.
//
// IDEMPOTENCE HERE MEANS "BOUNDED EFFECT ON A REAL CAR", not "the same bytes
// back". Three independent mechanisms, each closing a different door:
//
//   - The STREAMING SHORT-CIRCUIT makes the common repeat — a client calling
//     again after success — free: no wake, no probe, no push.
//   - The PER-VEHICLE COOLDOWN is longer than the probe budget, so two requests
//     for one car cannot overlap and a stuck client cannot loop wakes.
//   - The PAIRING-EPOCH BUDGET (MYR-489, shared with the reconciler) means that
//     even across cooldowns, the delete-then-create against the car happens at
//     most once per epoch.
//
// The cooldown is consumed BEFORE the sequence runs and is NOT refunded on
// failure — a client retrying an unreachable car must back off rather than
// re-waking it every second, exactly as §7.15 argues for refresh.

package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// setupCompleter is the consumer-site seam over *SetupCompleter so the handler
// is unit-testable with a fake — NO live Tesla call ever fires from a test.
type setupCompleter interface {
	Complete(ctx context.Context, row VehicleSnapshotRow) (SetupState, error)
}

// SetupCompletionHandler serves the §7.23 route.
type SetupCompletionHandler struct {
	auth      tokenValidator
	vehicles  VehicleSnapshotReader
	completer setupCompleter
	cooldown  *vehicleCooldown
	logger    *slog.Logger
}

// NewSetupCompletionHandler wires the handler.
func NewSetupCompletionHandler(
	auth tokenValidator,
	vehicles VehicleSnapshotReader,
	completer setupCompleter,
	logger *slog.Logger,
) *SetupCompletionHandler {
	return &SetupCompletionHandler{
		auth:      auth,
		vehicles:  vehicles,
		completer: completer,
		cooldown:  newVehicleCooldown(defaultSetupCooldown, defaultSetupCooldownBurst),
		logger:    logger,
	}
}

// setupCompletionResponse is the success body.
//
// DELIBERATELY MINIMAL. It carries the vehicle it is about and the ONE thing
// the client renders. Everything a caller might also want — whether we woke the
// car, whether this call or an earlier one did the push, how many probes it
// took — is server bookkeeping that would become contract the moment it was
// emitted, and none of it changes what the owner is told.
type setupCompletionResponse struct {
	VehicleID  string     `json:"vehicleId"`
	SetupState SetupState `json:"setupState"`
}

// ServeHTTP routes POST only.
func (h *SetupCompletionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}
	h.handle(w, r)
}

func (h *SetupCompletionHandler) handle(w http.ResponseWriter, r *http.Request) {
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
		h.logger.Warn("complete-setup: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	row, ok := h.authorizeVehicle(ctx, w, vehicleID, userID)
	if !ok {
		return
	}

	// Keyed by VIN, not vehicleId: the resource being protected is the CAR and
	// Tesla's per-vehicle rate limits, both of which are VIN-shaped.
	if !h.cooldown.allow(row.VIN) {
		w.Header().Set("Retry-After", "60")
		h.writeError(w, http.StatusTooManyRequests, wserrors.ErrCodeRateLimited,
			"setup completion is already running or ran within the last minute for this vehicle")
		return
	}

	state, err := h.completer.Complete(ctx, row)
	if err != nil {
		h.writeCompletionError(w, row, err)
		return
	}

	h.writeJSON(w, http.StatusOK, setupCompletionResponse{VehicleID: row.ID, SetupState: state})
}

// authorizeVehicle loads the vehicle and enforces owner-only access, matching
// the §7.12 / §7.14 / §7.15 convention.
func (h *SetupCompletionHandler) authorizeVehicle(
	ctx context.Context, w http.ResponseWriter, vehicleID, userID string,
) (VehicleSnapshotRow, bool) {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return VehicleSnapshotRow{}, false
		}
		h.logger.Error("complete-setup: lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()))
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return VehicleSnapshotRow{}, false
	}
	if row.UserID != userID {
		h.logger.Warn("complete-setup: ownership mismatch",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID))
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
		return VehicleSnapshotRow{}, false
	}
	return row, true
}

// writeCompletionError maps a sentinel outcome onto the typed REST envelope.
//
// Every branch here is a case where NO setup state could honestly be claimed,
// which is why none of them returns a 200 with a state attached. The client
// renders a retry affordance from the code, not a setup card.
func (h *SetupCompletionHandler) writeCompletionError(w http.ResponseWriter, row VehicleSnapshotRow, err error) {
	vin := redactVIN(row.VIN)
	switch {
	case errors.Is(err, ErrSetupVehicleUnreachable):
		h.logger.Info("complete-setup: vehicle unreachable",
			slog.String("vehicle_id", row.ID), slog.String("vin", vin))
		h.writeError(w, http.StatusServiceUnavailable, wserrors.ErrCodeVehicleAsleep,
			"your car did not come online — try again in a moment")

	case errors.Is(err, ErrSetupPushUnavailable):
		// A deployment fact, not a car fact: this process has no signing proxy
		// (the self-serve-onboarding.md §5 safety guard), so it cannot push
		// config to a real car at all.
		h.logger.Error("complete-setup: no signing proxy configured; cannot push config",
			slog.String("vehicle_id", row.ID), slog.String("vin", vin))
		h.writeError(w, http.StatusServiceUnavailable, wserrors.ErrCodeServiceUnavailable,
			"vehicle configuration is temporarily unavailable")

	case errors.Is(err, ErrSetupProbeUnavailable):
		h.logger.Warn("complete-setup: could not read pairing status from Tesla",
			slog.String("vehicle_id", row.ID), slog.String("vin", vin),
			slog.String("error", redactedErrorText(err)))
		h.writeError(w, http.StatusBadGateway, wserrors.ErrCodeCommandFailed,
			"could not check your car's pairing status — try again")

	case errors.Is(err, ErrSetupPushFailed):
		// forceRepush has already logged the specifics, including the loud
		// UNCONFIGURED event, and scheduled the reconciler to repair it.
		h.logger.Warn("complete-setup: forced config re-push failed",
			slog.String("vehicle_id", row.ID), slog.String("vin", vin))
		h.writeError(w, http.StatusBadGateway, wserrors.ErrCodeCommandFailed,
			"could not finish configuring your car — we will keep trying")

	default:
		h.logger.Error("complete-setup failed",
			slog.String("vehicle_id", row.ID), slog.String("vin", vin),
			slog.String("error", redactedErrorText(err)))
		h.writeError(w, http.StatusBadGateway, wserrors.ErrCodeCommandFailed, "setup completion failed upstream")
	}
}

// writeJSON / writeError mirror the sibling §7.15 handler (rest-api.md §4.1
// typed error envelope).
func (h *SetupCompletionHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

func (h *SetupCompletionHandler) writeError(
	w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string,
) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
