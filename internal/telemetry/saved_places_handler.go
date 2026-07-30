package telemetry

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// SavedPlacesHandler serves the account-persisted saved-place surface
// (MYR-321, rest-api.md §7.20):
//
//	GET    /api/users/me/places          read both slots
//	PUT    /api/users/me/places/{kind}   set or replace one
//	DELETE /api/users/me/places/{kind}   forget one
//
// User-scoped like §7.6, §7.7 and §7.19: there is no vehicle in the path, so
// there is no ownership check to make — THE JWT SUBJECT IS THE RESOURCE. There
// is no userId in any path or body and there must never be one.
//
// ACCOUNT-SCOPED, NOT VEHICLE-SCOPED, and that is the whole point of the issue:
// a saved place belongs to a person, so it follows them across every car they
// own, every car they are a viewer of, and every device they sign in on. Before
// this, "Home" was a SwiftUI @State field that meant a different address on an
// iPhone than on an iPad and was forgotten on reinstall.
//
// NEVER VISIBLE TO A COUNTERPARTY. Sharing a car grants access to the CAR, not
// to the other person's address book, so these rows reach the account that saved
// them and nobody else — not a co-owner, not a viewer, not the owner of a car
// this person rides in.
//
// P1 THROUGHOUT, AND NEVER LOGGED. The coordinates are AES-256-GCM encrypted at
// rest (NFR-3.23, data-classification.md §1.17) and the label is log-redacted.
// No log line in this file carries a coordinate or a label — not on success,
// not on validation failure, and not inside an error envelope. A durable home
// coordinate is where somebody sleeps; leaking it into an operator's log would
// undo the column encryption three lines later.
type SavedPlacesHandler struct {
	auth     tokenValidator
	registry SavedPlacesRegistry
	logger   *slog.Logger
}

// NewSavedPlacesHandler wires the saved-place endpoints.
func NewSavedPlacesHandler(auth tokenValidator, registry SavedPlacesRegistry, logger *slog.Logger) *SavedPlacesHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &SavedPlacesHandler{auth: auth, registry: registry, logger: logger}
}

// ServeList handles GET /api/users/me/places.
//
// Returns BOTH kinds when set, and an EMPTY ARRAY when neither is — never null,
// never a 404, and never two placeholder rows with absent coordinates. An
// account that has saved nothing is the common case on day one, not an error.
func (h *SavedPlacesHandler) ServeList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}

	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	places, err := h.registry.ListSavedPlaces(r.Context(), userID)
	if err != nil {
		// The error is wrapped with the user id but NEVER with a coordinate;
		// the store layer's decrypt failures name the column, not the value.
		h.logger.Error("saved places: read failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	// Non-nil zero-length so the JSON is [] rather than null.
	body := savedPlacesListResponse{Places: make([]savedPlaceResponse, 0, len(places))}
	for _, p := range places {
		body.Places = append(body.Places, newSavedPlaceResponse(p))
	}
	h.writeJSON(w, http.StatusOK, body)
}

// ServePut handles PUT /api/users/me/places/{kind}.
//
// Answers 200 with the stored place — the echo, scanned back out of the
// database rather than reflected from the request, so what the client adopts is
// provably what was persisted.
//
// 200 ON BOTH FIRST WRITE AND REPLACE, never 201: the resource is the SLOT,
// which always exists in the URL space whether or not a row backs it, so a
// create and an update are indistinguishable to the caller and neither mints a
// new address.
func (h *SavedPlacesHandler) ServePut(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}

	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}
	kind, ok := h.pathKind(w, r)
	if !ok {
		return
	}
	place, ok := h.decode(w, r, kind)
	if !ok {
		return
	}

	stored, err := h.registry.UpsertSavedPlace(r.Context(), userID, place)
	if err != nil {
		h.logger.Error("saved places: write failed",
			slog.String("user_id", userID),
			slog.String("kind", kind),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	// The kind is P0 (which slot), the user id is an opaque cuid. The LABEL AND
	// COORDINATES ARE P1 AND ARE DELIBERATELY ABSENT from this line — logging
	// "saved place updated: home = 37.7955,-122.3937" would put a person's
	// house in the operator's log store and defeat the column encryption
	// entirely.
	h.logger.Info("saved place updated",
		slog.String("user_id", userID),
		slog.String("kind", kind),
	)
	h.writeJSON(w, http.StatusOK, newSavedPlaceResponse(stored))
}

// ServeDelete handles DELETE /api/users/me/places/{kind}.
//
// ALWAYS 204, whether or not a row was there. IDEMPOTENT BY DESIGN: a client
// retrying a dropped DELETE must not be told 404 for work it already completed,
// and "this slot is now empty" is true either way. A 404 here would also be a
// small oracle — it would tell a caller whether the account had ever set that
// slot, which is a fact about a person's habits and not one this endpoint owes
// anybody who can already read the list.
func (h *SavedPlacesHandler) ServeDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}

	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}
	kind, ok := h.pathKind(w, r)
	if !ok {
		return
	}

	removed, err := h.registry.DeleteSavedPlace(r.Context(), userID, kind)
	if err != nil {
		h.logger.Error("saved places: delete failed",
			slog.String("user_id", userID),
			slog.String("kind", kind),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.logger.Info("saved place deleted",
		slog.String("user_id", userID),
		slog.String("kind", kind),
		// Distinguishes a real delete from a no-op re-run in the log without
		// changing the response, which is 204 for both.
		slog.Bool("row_removed", removed),
	)
	w.WriteHeader(http.StatusNoContent)
}

// authUser resolves the bearer token to a user id, writing a 401 on failure.
func (h *SavedPlacesHandler) authUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return "", false
	}
	userID, err := h.auth.ValidateToken(r.Context(), token)
	if err != nil {
		h.logger.Warn("saved places: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return "", false
	}
	return userID, true
}

// pathKind reads and validates the {kind} path segment.
//
// Case-SENSITIVE and lowercase: 'Home' is a 400, not a synonym. Accepting
// variants would let two spellings of one slot reach an upsert whose conflict
// target is the exact bytes — and a person would end up with two homes, one of
// which they could no longer see.
func (h *SavedPlacesHandler) pathKind(w http.ResponseWriter, r *http.Request) (string, bool) {
	kind := r.PathValue("kind")
	if !isValidSavedPlaceKind(kind) {
		// The rejected value is NOT echoed back: it is attacker-controlled and
		// this envelope is not a place to reflect input.
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"kind must be one of: home, work")
		return "", false
	}
	return kind, true
}

func (h *SavedPlacesHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

func (h *SavedPlacesHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
