package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// VehicleReaddHandler serves POST /api/tesla/vehicles/{teslaVehicleId}/re-add —
// the owner "Add this car back" deliberate re-add endpoint (MYR-262). It is the
// sanctioned un-trap for the MYR-261 removed-vehicle tombstone: a car the owner
// deliberately removed gets a go_removed_vehicles tombstone, and the passive
// post-link bulk sync (ownerStreamHook.AfterLink -> UpsertOwnedVehicle) skips
// any tombstoned VIN so an incidental Tesla re-link can NEVER resurrect it. That
// tombstone-wins default made a removed car a permanent trap; this endpoint is
// the ONLY runtime path that clears a tombstone.
//
// The deliberate-vs-passive seam is exactly one call: this handler clears the
// tombstone (RemovedVehicleRegistry.ClearTombstone) BEFORE provisioning, whereas
// the passive AfterLink sync never clears one. Everything else (list -> owner
// filter -> UpsertOwnedVehicle -> stream-config push) is the shared provisioning
// path, so once the tombstone is gone the car returns through the same code the
// passive sync uses.
//
// Ownership is fail-closed at two layers, neither trusting the HTTP path param:
//   - ClearTombstone is scoped `WHERE user_id = <caller> AND tesla_vehicle_id =
//     <id>`, so a caller can only ever clear their OWN tombstone — never another
//     user's.
//   - The best-effort provision lists the CALLER's Fleet-API vehicles and
//     provisions only an OWNER-access match (UpsertOwnedVehicle is itself
//     owner-scoped and cross-user-safe), so a caller can never re-add a car they
//     do not own even if they guess another owner's teslaVehicleId.
//
// The path key is the Tesla vehicle id (NOT a Prisma cuid like the §7.12
// teardown): at re-add time the local "Vehicle" row has been deleted, so the
// tombstone's (user_id, tesla_vehicle_id) composite is the only stable handle
// for a removed car.
//
// Idempotent: clearing an absent tombstone is a clean no-op (wasTombstoned=false)
// and the response is still 200 — the post-state is "no tombstone blocks this
// car" either way.
type VehicleReaddHandler struct {
	auth        tokenValidator
	clearer     TombstoneClearer
	provisioner VehicleReaddProvisioner // nil disables the inline re-provision (tests / no proxy)
	logger      *slog.Logger
}

// TombstoneClearer clears the removed-vehicle tombstone for a specific
// (userID, teslaVehicleID) the caller owns and reports whether one existed.
// Satisfied by *store.RemovedVehicleRegistry. It MUST scope the delete to the
// caller's own tombstone (WHERE user_id = caller) so a re-add can never clear
// another user's tombstone, and be idempotent (an absent tombstone is a clean
// no-op success, no audit row).
type TombstoneClearer interface {
	ClearTombstone(ctx context.Context, userID, teslaVehicleID string) (bool, error)
}

// VehicleReaddProvisioner best-effort re-provisions a single owned car after its
// tombstone is cleared: resolve the owner's Tesla token, list their Fleet-API
// vehicles, and (only for an OWNER-access match) seed the "Vehicle" row + push
// the stream config. Returns whether the car was provisioned inline. It is
// nil-able (unwired in tests / when the proxy is absent) and best-effort — a
// failure never fails the re-add, because clearing the tombstone is the durable
// un-trap and the next Tesla link's passive sync will then provision the car.
type VehicleReaddProvisioner interface {
	ProvisionReaddedVehicle(ctx context.Context, userID, teslaVehicleID string) (provisioned bool, err error)
}

// NewVehicleReaddHandler wires the re-add handler. provisioner may be nil (no
// proxy configured / tests) — the tombstone is still cleared and the inline
// re-provision is skipped (reported as provisioned=false).
func NewVehicleReaddHandler(
	auth tokenValidator,
	clearer TombstoneClearer,
	provisioner VehicleReaddProvisioner,
	logger *slog.Logger,
) *VehicleReaddHandler {
	return &VehicleReaddHandler{
		auth:        auth,
		clearer:     clearer,
		provisioner: provisioner,
		logger:      logger,
	}
}

// vehicleReaddResponse is the honest post-re-add body (rest-api.md §7.13).
type vehicleReaddResponse struct {
	// Readded is authoritative: after this call no tombstone blocks the car, so
	// it is eligible to return (true on both a real clear and an idempotent
	// no-op, mirroring the teardown's `removed`).
	Readded bool `json:"readded"`
	// WasTombstoned reports whether a tombstone was actually cleared by this call
	// (false when the car had no tombstone — a clean idempotent no-op).
	WasTombstoned bool `json:"wasTombstoned"`
	// Provisioned reports whether the car was re-provisioned inline. False means
	// the un-trap succeeded but the car will be provisioned by the next Tesla
	// link's passive sync (e.g. the owner's tokens were cleared on a last-vehicle
	// removal and must be re-linked first).
	Provisioned bool `json:"provisioned"`
}

// ServeHTTP routes POST only.
func (h *VehicleReaddHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}
	h.handle(w, r)
}

// handle authenticates the caller, clears the tombstone for the caller's own
// teslaVehicleId, then best-effort re-provisions the car.
func (h *VehicleReaddHandler) handle(w http.ResponseWriter, r *http.Request) {
	teslaVehicleID := strings.TrimSpace(r.PathValue("teslaVehicleId"))
	if teslaVehicleID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing teslaVehicleId")
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
		h.logger.Warn("vehicle re-add: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	// Un-trap: clear the caller's OWN tombstone (owner-scoped at the SQL layer).
	// This is the single call that distinguishes a deliberate re-add from the
	// passive AfterLink sync — the sync NEVER clears a tombstone.
	cleared, err := h.clearer.ClearTombstone(ctx, userID, teslaVehicleID)
	if err != nil {
		h.logger.Error("vehicle re-add: clear tombstone failed",
			slog.String("user_id", userID),
			slog.String("tesla_vehicle_id", teslaVehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	// Best-effort inline re-provision (owner-filtered against the caller's fleet).
	// Never fails the re-add: the tombstone clear above is the durable un-trap.
	provisioned := false
	if h.provisioner != nil {
		provisioned, err = h.provisioner.ProvisionReaddedVehicle(ctx, userID, teslaVehicleID)
		if err != nil {
			h.logger.Warn("vehicle re-add: inline provision failed (tombstone cleared; retriable via re-link)",
				slog.String("user_id", userID),
				slog.String("tesla_vehicle_id", teslaVehicleID),
				slog.String("error", err.Error()),
			)
		}
	}

	h.logger.Info("vehicle re-add complete",
		slog.String("user_id", userID),
		slog.String("tesla_vehicle_id", teslaVehicleID),
		slog.Bool("was_tombstoned", cleared),
		slog.Bool("provisioned", provisioned),
	)

	h.writeJSON(w, http.StatusOK, vehicleReaddResponse{
		Readded:       true,
		WasTombstoned: cleared,
		Provisioned:   provisioned,
	})
}

// writeJSON / writeError mirror the VehicleTeardownHandler helpers (rest-api.md
// §4.1 typed error envelope).
func (h *VehicleReaddHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

func (h *VehicleReaddHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
