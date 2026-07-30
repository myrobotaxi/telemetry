// Body decoding and validation for the saved-places surface (MYR-321,
// rest-api.md §7.20). Split from saved_places_handler.go so the handler file
// stays focused on the HTTP verbs and stays under the 300-line cap.

package telemetry

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// savedPlaceKinds is the closed set of slots, mirroring the migration 0023
// CHECK constraint and the contract enum (saved-places.schema.json
// $defs.SavedPlaceKind). Declared here rather than imported from the store so
// internal/telemetry keeps its no-store-import rule; the migration test is what
// pins the two lists together.
var savedPlaceKinds = []string{"home", "work"}

// savedPlaceLabelMaxRunes caps the display line. Enforced on the RUNE count,
// not the byte length, so a 200-character address in a non-Latin script is
// accepted rather than rejected at 67 characters — the database CHECK uses
// char_length(), which counts characters too, so the two agree.
const savedPlaceLabelMaxRunes = 200

// isValidSavedPlaceKind reports whether s names a slot. Case-sensitive; see
// SavedPlacesHandler.pathKind for why.
func isValidSavedPlaceKind(s string) bool {
	for _, k := range savedPlaceKinds {
		if k == s {
			return true
		}
	}
	return false
}

// decode parses and validates the PUT body against the kind named by the path.
//
// Every failure is a 400 `invalid_request` with a message that names the
// offending FIELD but NEVER its value: the label and coordinates are P1, and an
// error envelope that echoed them would leak a person's address to whatever
// logs or reports the 400 — including a client crash reporter.
func (h *SavedPlacesHandler) decode(w http.ResponseWriter, r *http.Request, pathKind string) (SavedPlaceData, bool) {
	// Strict decode (unknown keys are a 400), matching §7.14 / §7.17 / §7.19.
	// It matters here because the body is a WHOLE-object upsert: a typo'd or
	// renamed key would otherwise decode to a nil pointer and be reported as a
	// missing required field, which is the right rejection by luck rather than
	// by design — and a client sending `lat` instead of `latitude` deserves to
	// be told its key is unknown, not that it omitted one it thinks it sent.
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var body savedPlaceRequest
	if err := dec.Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return SavedPlaceData{}, false
	}

	// The body's `kind` is optional and redundant with the path. When present
	// it MUST agree: a body that could redirect the write would let a stale
	// client overwrite Home while its URL said Work, and the path is what
	// authorizes and routes the call.
	if body.Kind != nil && *body.Kind != pathKind {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"body kind must match the path kind")
		return SavedPlaceData{}, false
	}

	label, ok := h.validateLabel(w, body.Label)
	if !ok {
		return SavedPlaceData{}, false
	}
	latitude, ok := h.validateCoord(w, body.Latitude, "latitude", 90)
	if !ok {
		return SavedPlaceData{}, false
	}
	longitude, ok := h.validateCoord(w, body.Longitude, "longitude", 180)
	if !ok {
		return SavedPlaceData{}, false
	}

	return SavedPlaceData{
		Kind:      pathKind,
		Label:     label,
		Latitude:  latitude,
		Longitude: longitude,
	}, true
}

// validateLabel enforces presence, non-blankness and the rune cap.
//
// REQUIRED ON EVERY WRITE, including one that only moves the pin: this is an
// upsert, so an omitted label means "I did not send a label", never "keep the
// one you have". Storing the old label against new coordinates is precisely the
// failure a whole-object write exists to prevent.
//
// The stored value is TRIMMED of surrounding whitespace, so " Home " and "Home"
// are the same label rather than two that render identically and compare
// unequal in a client's cache.
func (h *SavedPlacesHandler) validateLabel(w http.ResponseWriter, raw *string) (string, bool) {
	if raw == nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "label is required")
		return "", false
	}
	label := strings.TrimSpace(*raw)
	if label == "" {
		// Whitespace-only is blank. A label that renders as nothing would give
		// the person an unlabelled row they cannot tell apart from the other.
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "label must not be blank")
		return "", false
	}
	if utf8.RuneCountInString(label) > savedPlaceLabelMaxRunes {
		// REJECTED, never truncated: silently storing a prefix of somebody's
		// address would show them a place they did not save.
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"label must be at most 200 characters")
		return "", false
	}
	return label, true
}

// validateCoord enforces presence, finiteness and the WGS-84 range for one
// axis. limit is 90 for latitude and 180 for longitude.
//
// A POINTER RATHER THAN A PLAIN float64 is what makes "omitted" expressible at
// all: 0 is a real coordinate — the equator and the prime meridian both cross
// inhabited land — so on a plain float64 a missing key and a deliberate 0
// decode identically, and a client that forgot to send a latitude would have a
// place silently saved off the coast of Ghana.
//
// The finiteness check is not theoretical. encoding/json rejects the bare
// tokens NaN and Infinity, but a value can still arrive non-finite from an
// overflowing literal like 1e400, which decodes to +Inf. Both are caught here
// rather than at the database, where the encrypt step would happily seal
// "+Inf" into ciphertext that no reader can turn back into a place.
func (h *SavedPlacesHandler) validateCoord(w http.ResponseWriter, raw *float64, field string, limit float64) (float64, bool) {
	if raw == nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, field+" is required")
		return 0, false
	}
	v := *raw
	if math.IsNaN(v) || math.IsInf(v, 0) {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, field+" must be a finite number")
		return 0, false
	}
	if v < -limit || v > limit {
		// The out-of-range VALUE is not echoed: it is P1 by association (a
		// caller's near-miss coordinate is still a location) and reflecting
		// input into an error envelope is a habit worth not having.
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, field+" is out of range")
		return 0, false
	}
	return v, true
}
