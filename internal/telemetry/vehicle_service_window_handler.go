package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// OwnerServiceWindowWriter persists the OWNER-ENTERED expected service end for
// a vehicle. A nil expectedEndAt clears it. Satisfied by
// *store.VehicleRepo.SetServiceExpectedEndAt.
//
// Implementations MUST leave the Tesla-sourced `service_etc` column untouched —
// the two sources are separate columns precisely so an owner's answer cannot
// overwrite Tesla's, nor be overwritten by it.
type OwnerServiceWindowWriter interface {
	SetServiceExpectedEndAt(ctx context.Context, vehicleID string, expectedEndAt *time.Time) error
}

// VehicleServiceWindowHandler serves
// PUT /api/tesla/vehicles/{vehicleId}/service-window — the owner-entered
// "expected back" fallback behind `serviceEstimatedEndAt` (MYR-316,
// rest-api.md §7.16).
//
// Why an owner-entered value is needed at all: Tesla's `service_data` is
// frequently ALL NULL. A visit booked by phone, a walk-in, or any visit with no
// appointment record attached yields no `service_etc`, and that is the common
// case rather than the exception. Without a fallback the whole service window —
// the "Estimated completion" line and, more importantly, the scheduler bound
// that stops an owner accepting a reservation for the middle of a service visit
// — would simply not exist for those cars.
//
// The value is a FALLBACK, never an override: emission is
// COALESCE(tesla, owner), so the moment Tesla produces an estimate it wins, and
// if Tesla later withdraws it the owner's answer takes over again. Both live in
// their own column so neither erases the other.
//
// Ownership mirrors the sibling §7.12 / §7.14 / §7.15 endpoints exactly:
// unknown vehicleId → `404 not_found` (indistinguishable from
// ownership-filtered — never leak existence), real mismatch →
// `403 vehicle_not_owned`. Neither writes anything.
type VehicleServiceWindowHandler struct {
	auth     tokenValidator
	vehicles VehicleSnapshotReader
	windows  OwnerServiceWindowWriter
	now      func() time.Time
	logger   *slog.Logger
}

// VehicleServiceWindowOption configures optional handler dependencies.
type VehicleServiceWindowOption func(*VehicleServiceWindowHandler)

// withServiceWindowClock injects a clock so the future-instant validation is
// deterministic in tests.
func withServiceWindowClock(now func() time.Time) VehicleServiceWindowOption {
	return func(h *VehicleServiceWindowHandler) {
		if now != nil {
			h.now = now
		}
	}
}

// NewVehicleServiceWindowHandler wires the owner service-window handler.
func NewVehicleServiceWindowHandler(
	auth tokenValidator,
	vehicles VehicleSnapshotReader,
	windows OwnerServiceWindowWriter,
	logger *slog.Logger,
	opts ...VehicleServiceWindowOption,
) *VehicleServiceWindowHandler {
	h := &VehicleServiceWindowHandler{
		auth:     auth,
		vehicles: vehicles,
		windows:  windows,
		now:      time.Now,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// vehicleServiceWindowRequest is the request body:
// `{"expectedEndAt": "2026-07-29T18:00:00Z"}`.
//
// A POINTER so the three cases stay distinguishable at the JSON layer: an
// absent key, an explicit null, and a string. The first two both CLEAR — an
// owner retracting an estimate should not have to guess which spelling we
// wanted.
type vehicleServiceWindowRequest struct {
	ExpectedEndAt *string `json:"expectedEndAt"`
}

// vehicleServiceWindowResponse echoes the stored value so a client can adopt it
// without a re-read — there is no WebSocket delta for this field.
type vehicleServiceWindowResponse struct {
	VehicleID string `json:"vehicleId"`
	// ExpectedEndAt is the stored owner value in RFC 3339 UTC, or null when
	// cleared. NOTE this is the OWNER column, not the emitted
	// serviceEstimatedEndAt — Tesla's estimate may still take precedence on the
	// next read of §7.0 / §7.1.
	ExpectedEndAt *string `json:"expectedEndAt"`
}

// ServeHTTP routes PUT only.
func (h *VehicleServiceWindowHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}
	h.handle(w, r)
}

// handle authenticates the caller, validates the instant, verifies ownership,
// and writes.
func (h *VehicleServiceWindowHandler) handle(w http.ResponseWriter, r *http.Request) {
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
		h.logger.Warn("service window: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	expectedEndAt, ok := h.decodeExpectedEndAt(w, r)
	if !ok {
		return
	}

	if !h.verifyOwnership(ctx, w, vehicleID, userID) {
		return
	}

	if err := h.windows.SetServiceExpectedEndAt(ctx, vehicleID, expectedEndAt); err != nil {
		h.logger.Error("service window: write failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.logger.Info("service window updated",
		slog.String("vehicle_id", vehicleID),
		slog.String("user_id", userID),
		slog.Bool("cleared", expectedEndAt == nil),
	)

	h.writeJSON(w, http.StatusOK, vehicleServiceWindowResponse{
		VehicleID:     vehicleID,
		ExpectedEndAt: formatInstantOrNil(expectedEndAt),
	})
}

// decodeExpectedEndAt parses and validates the body. Returns ok=false after
// writing a 400.
//
// An empty request body is accepted as a clear, matching the absent-key and
// explicit-null spellings — a client that "unsets" the field by sending nothing
// at all is expressing the same intent.
func (h *VehicleServiceWindowHandler) decodeExpectedEndAt(w http.ResponseWriter, r *http.Request) (*time.Time, bool) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var body vehicleServiceWindowRequest
	if err := dec.Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, true // empty body clears
		}
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return nil, false
	}

	// Absent key, explicit null, or an empty/whitespace string all clear.
	if body.ExpectedEndAt == nil || strings.TrimSpace(*body.ExpectedEndAt) == "" {
		return nil, true
	}

	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(*body.ExpectedEndAt))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"expectedEndAt must be an RFC 3339 date-time")
		return nil, false
	}

	// Must be in the FUTURE. A past instant is either a typo or a stale form
	// submission, and storing one would emit a service window that claims the
	// car is already back — worse than emitting nothing, because the scheduler
	// bound would then permit rides it should refuse.
	utc := ts.UTC()
	if !utc.After(h.now()) {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"expectedEndAt must be in the future")
		return nil, false
	}

	return &utc, true
}

// verifyOwnership resolves vehicleId → owner and enforces that the caller owns
// it. Matches the §7.12 / §7.14 convention exactly: unknown vehicle → 404
// (never leak existence), ownership mismatch → 403.
//
// This check is LOAD-BEARING in a way the plate endpoint's is not: the
// service-window columns live on the Go-owned side table, which has no userId
// column, so there is no owner-scoped SQL predicate to fall back on.
func (h *VehicleServiceWindowHandler) verifyOwnership(ctx context.Context, w http.ResponseWriter, vehicleID, userID string) bool {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return false
		}
		h.logger.Error("service window: lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return false
	}
	if row.UserID != userID {
		h.logger.Warn("service window: ownership mismatch",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
		return false
	}
	return true
}

// writeJSON / writeError mirror the sibling §7.12 / §7.14 / §7.15 handlers
// (rest-api.md §4.1 typed error envelope).
func (h *VehicleServiceWindowHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

func (h *VehicleServiceWindowHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
