package push

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// The notification-preference endpoints (rest-api.md §7.19):
//
//	GET /api/users/me/push-prefs   read this account's five switches
//	PUT /api/users/me/push-prefs   change some of them
//
// User-scoped like §7.7 and §7.17: there is no vehicle in the path, so there is
// no ownership check to make — the JWT subject IS the resource.
//
// NOTE ON SCOPE: these are DELIVERY preferences, not authorization. Switching a
// category off stops this service waking a phone; it does not restrict what the
// account may read over REST or the telemetry socket, and it is not a security
// control. A category that is off is a silence, not a denial.

// PrefsRegistry is the persistence seam for the two endpoints. Satisfied by an
// adapter over *store.PushPrefsRepo (the same cmd/-level adaptation DeviceStore
// gets, so internal/push imports no store package).
type PrefsRegistry interface {
	// PrefsForUser returns the stored preferences, or DefaultPrefs for an
	// account that has never written any. A missing row is not an error.
	PrefsForUser(ctx context.Context, userID string) (Prefs, error)
	// UpdatePrefs applies a PARTIAL update and returns the whole set as
	// stored — the echo the client adopts.
	UpdatePrefs(ctx context.Context, userID string, update PrefsUpdate) (Prefs, error)
}

// PrefsUpdate is a partial write: a nil field means "leave this category
// alone", never "switch it off". See prefsRequest for why the wire is partial.
type PrefsUpdate struct {
	RideLifecycle    *bool
	DriveStarted     *bool
	DriveCompleted   *bool
	ChargingComplete *bool
	ViewerJoined     *bool
}

// isEmpty reports whether the update would change nothing. A legal request —
// §7.19 defines `{}` as a plain read-after-write — but worth distinguishing in
// the log line so an operator can tell a no-op client from a working one.
func (u PrefsUpdate) isEmpty() bool {
	return u.RideLifecycle == nil &&
		u.DriveStarted == nil &&
		u.DriveCompleted == nil &&
		u.ChargingComplete == nil &&
		u.ViewerJoined == nil
}

// PrefsHandler serves the notification-preference surface.
type PrefsHandler struct {
	auth     tokenValidator
	registry PrefsRegistry
	logger   *slog.Logger
}

// NewPrefsHandler wires the preference endpoints.
func NewPrefsHandler(auth tokenValidator, registry PrefsRegistry, logger *slog.Logger) *PrefsHandler {
	return &PrefsHandler{auth: auth, registry: registry, logger: logger}
}

// prefsResponse is the §7.19 body, emitted by BOTH verbs.
//
// Every field is a plain bool and ALL FIVE ARE ALWAYS PRESENT. No `omitempty`
// anywhere, deliberately: `omitempty` on a bool drops `false`, which is the
// interesting half of every one of these values, and a client reading an absent
// key as "unknown" (or as its own default of true) would render a switch in the
// opposite position to the one the person just chose.
type prefsResponse struct {
	RideLifecycle    bool `json:"rideLifecycle"`
	DriveStarted     bool `json:"driveStarted"`
	DriveCompleted   bool `json:"driveCompleted"`
	ChargingComplete bool `json:"chargingComplete"`
	ViewerJoined     bool `json:"viewerJoined"`
}

// newPrefsResponse projects the domain type onto the wire shape.
//
// A struct CONVERSION rather than a field-by-field literal, and the conversion
// is the guard: Go permits it only while the two shapes have identical field
// names and types (tags are ignored), so adding a sixth category to Prefs and
// forgetting the response fails to COMPILE here. A literal would have compiled
// and quietly omitted the new key.
//
// The guard covers ADDITIONS and RENAMES but NOT REORDERINGS: field order is
// part of struct identity, so the two declarations must stay in the same order
// — and because all five fields are the same type, swapping two of them stays
// compilable and silently mislabels both switches. Keep the field order in
// Prefs, prefsResponse and prefsRequest identical; the per-category tests are
// what would actually catch a swap.
func newPrefsResponse(p Prefs) prefsResponse {
	return prefsResponse(p)
}

// prefsRequest is the PUT body: the SAME five keys, each OPTIONAL.
//
// Partial on purpose. A settings screen changes one switch at a time, and a
// whole-object PUT would make a second phone — or a stale screen — write back
// four values its user never touched. With pointers, an omitted key means
// "leave alone" and only the switch that actually moved travels.
//
// The pointers are also what makes `false` expressible at all: on a plain bool,
// an omitted key and an explicit `false` decode identically, which for a
// preference API means the off position can never be sent.
type prefsRequest struct {
	RideLifecycle    *bool `json:"rideLifecycle"`
	DriveStarted     *bool `json:"driveStarted"`
	DriveCompleted   *bool `json:"driveCompleted"`
	ChargingComplete *bool `json:"chargingComplete"`
	ViewerJoined     *bool `json:"viewerJoined"`
}

// toUpdate projects the decoded body onto the store's partial-update shape.
// Same conversion-as-guard reasoning as newPrefsResponse.
func (b prefsRequest) toUpdate() PrefsUpdate {
	return PrefsUpdate(b)
}

// ServeGet handles GET /api/users/me/push-prefs.
func (h *PrefsHandler) ServeGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}

	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	prefs, err := h.registry.PrefsForUser(r.Context(), userID)
	if err != nil {
		h.logger.Error("push prefs: read failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.writeJSON(w, http.StatusOK, newPrefsResponse(prefs))
}

// ServePut handles PUT /api/users/me/push-prefs.
//
// Answers with the WHOLE set as stored — the echo. The client adopts it rather
// than the booleans it sent, which is the only way an optimistic switch can
// settle on the truth when another device changed a different category in the
// same moment.
func (h *PrefsHandler) ServePut(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}

	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}
	update, ok := h.decode(w, r)
	if !ok {
		return
	}

	prefs, err := h.registry.UpdatePrefs(r.Context(), userID, update)
	if err != nil {
		h.logger.Error("push prefs: write failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	// P0 throughout — an opaque cuid and five booleans about delivery. Unlike
	// the sibling device endpoints there is nothing here to redact.
	h.logger.Info("push prefs updated",
		slog.String("user_id", userID),
		slog.Bool("no_op", update.isEmpty()),
		slog.Bool("ride_lifecycle", prefs.RideLifecycle),
		slog.Bool("drive_started", prefs.DriveStarted),
		slog.Bool("drive_completed", prefs.DriveCompleted),
		slog.Bool("charging_complete", prefs.ChargingComplete),
		slog.Bool("viewer_joined", prefs.ViewerJoined),
	)
	h.writeJSON(w, http.StatusOK, newPrefsResponse(prefs))
}

// decode parses the partial PUT body.
func (h *PrefsHandler) decode(w http.ResponseWriter, r *http.Request) (PrefsUpdate, bool) {
	// Strict decode (unknown keys are a 400), matching the sibling handlers. It
	// matters more here than anywhere: on a partial body every key is optional,
	// so a typo'd or renamed category would otherwise be accepted with a 200
	// and change NOTHING — the client would show the switch in its new position
	// and the notification would keep arriving. That is the MYR-349 lie again,
	// one layer down.
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var body prefsRequest
	if err := dec.Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return PrefsUpdate{}, false
	}
	return body.toUpdate(), true
}

// authUser resolves the bearer token to a user id, writing a 401 on failure.
func (h *PrefsHandler) authUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := bearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return "", false
	}
	userID, err := h.auth.ValidateToken(r.Context(), token)
	if err != nil {
		h.logger.Warn("push prefs: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return "", false
	}
	return userID, true
}

func (h *PrefsHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

func (h *PrefsHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
