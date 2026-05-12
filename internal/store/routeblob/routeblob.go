// Package routeblob is the byte-compatible Go counterpart to
// `../my-robo-taxi/src/lib/route-blob-encryption.ts` (MYR-64 Phase 1).
// It centralises the dual-write/dual-read primitives for the two large
// P1-classified route polylines covered by the rollout
// (data-classification.md §1.4, NFR-3.23):
//
//   - Vehicle.navRouteCoordinatesEnc — Tesla's planned navigation
//     polyline (the "where the car is going" destination route, member
//     of the navigation atomic group). Plaintext column type: `Json?`.
//   - Drive.routePointsEnc           — recorded drive route polyline
//     (the historical breadcrumb trail of a completed drive). The
//     plaintext column is non-nullable `Json` defaulting to `[]`.
//
// # Wire format
//
// Plaintext is `json.Marshal`'d at the encryption boundary and the
// resulting UTF-8 bytes are sealed by `cryptox.Encryptor.EncryptString`.
// The decrypt boundary `json.Unmarshal`s the result back into the typed
// shape. The TS side performs the equivalent JSON.stringify/JSON.parse
// pair so the same ciphertext blob round-trips through either runtime.
//
// # Empty-value semantics
//
// An empty slice / nil input is treated as "absent". The encryption path
// returns `("", nil)` so the caller can write `NULL` into the *Enc
// column (matching the TS helper's `null` sentinel). The decrypt path
// returns `(nil, nil)` for an empty-string ciphertext so the caller
// short-circuits to plaintext.
//
// # Failure semantics
//
// Decrypt or `json.Unmarshal` failures return the wrapped error so the
// caller (vehicle_repo / drive_repo) can log a `Warn` and fall back to
// the plaintext column. Route blobs are 100KB+; corruption MUST NOT 500
// the live nav-route view. The fallback policy lives in the caller —
// this package only decrypts.

package routeblob

import (
	"encoding/json"
	"fmt"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// NavRouteCoordinate is the [lng, lat] tuple stored in
// `Vehicle.navRouteCoordinates` per vehicle-state-schema.md §3 (Mapbox
// / GeoJSON ordering). The Go telemetry server is the only writer in
// production; the TS app reads via `vehicle-mappers.ts`.
type NavRouteCoordinate = [2]float64

// RoutePoint is one element of `Drive.routePoints`. The shape mirrors
// store.RoutePointRecord but is owned here so this package has no
// dependency on the parent store. The JSON tags MUST match the parent
// shape exactly so a row written by the parent is readable by this
// helper and vice versa.
type RoutePoint struct {
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lng"`
	Speed     float64 `json:"speed"`
	Heading   float64 `json:"heading"`
	Timestamp string  `json:"timestamp"`
}

// EncryptNavRoute marshals coords to JSON and seals the bytes under
// the Encryptor's active write key. Returns `("", nil)` for nil/empty
// — the caller writes NULL into the *Enc column rather than an
// encrypted empty array.
//
// Errors only escape on a JSON marshal failure (extremely unlikely for
// a [2]float64 array) or a cryptographic failure inside the encryptor.
// Both are propagated unchanged so the caller can log + bump a counter
// without dropping the underlying telemetry.
func EncryptNavRoute(coords []NavRouteCoordinate, enc cryptox.Encryptor) (string, error) {
	if len(coords) == 0 {
		return "", nil
	}
	b, err := json.Marshal(coords)
	if err != nil {
		return "", fmt.Errorf("routeblob.EncryptNavRoute: marshal: %w", err)
	}
	ct, err := enc.EncryptString(string(b))
	if err != nil {
		return "", fmt.Errorf("routeblob.EncryptNavRoute: encrypt: %w", err)
	}
	return ct, nil
}

// DecryptNavRoute opens the ciphertext and unmarshals it into a slice
// of [lng, lat] tuples. Empty-string ciphertext is treated as "absent"
// and returns `(nil, nil)` so the caller can short-circuit to the
// plaintext column.
//
// A non-array decoded shape is reported as an error rather than
// silently returning `nil` so the caller's fallback path runs.
func DecryptNavRoute(ciphertext string, enc cryptox.Encryptor) ([]NavRouteCoordinate, error) {
	if ciphertext == "" {
		return nil, nil
	}
	plain, err := enc.DecryptString(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("routeblob.DecryptNavRoute: decrypt: %w", err)
	}
	if plain == "" {
		return nil, nil
	}
	var out []NavRouteCoordinate
	if err := json.Unmarshal([]byte(plain), &out); err != nil {
		return nil, fmt.Errorf("routeblob.DecryptNavRoute: unmarshal: %w", err)
	}
	return out, nil
}

// EncryptRoutePoints is the Drive.routePoints analog of
// EncryptNavRoute. Same empty-input semantics: nil/empty returns
// `("", nil)` so the caller writes NULL into the shadow column.
//
// We encode RoutePoint{} explicitly so the JSON tags are preserved
// (lat, lng, speed, heading, timestamp). Callers passing a
// `[]store.RoutePointRecord` should convert via FromRoutePointRecords
// — the fields are identical but the package owns its own type to
// avoid a circular dependency on internal/store.
func EncryptRoutePoints(points []RoutePoint, enc cryptox.Encryptor) (string, error) {
	if len(points) == 0 {
		return "", nil
	}
	b, err := json.Marshal(points)
	if err != nil {
		return "", fmt.Errorf("routeblob.EncryptRoutePoints: marshal: %w", err)
	}
	ct, err := enc.EncryptString(string(b))
	if err != nil {
		return "", fmt.Errorf("routeblob.EncryptRoutePoints: encrypt: %w", err)
	}
	return ct, nil
}

// DecryptRoutePoints opens the ciphertext and unmarshals it into a
// slice of RoutePoint. Empty input returns `(nil, nil)`. Non-array
// decoded shape returns an error so the caller falls back.
func DecryptRoutePoints(ciphertext string, enc cryptox.Encryptor) ([]RoutePoint, error) {
	if ciphertext == "" {
		return nil, nil
	}
	plain, err := enc.DecryptString(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("routeblob.DecryptRoutePoints: decrypt: %w", err)
	}
	if plain == "" {
		return nil, nil
	}
	var out []RoutePoint
	if err := json.Unmarshal([]byte(plain), &out); err != nil {
		return nil, fmt.Errorf("routeblob.DecryptRoutePoints: unmarshal: %w", err)
	}
	return out, nil
}

// EncryptJSONBytes is the low-level helper used by the Vehicle write
// path: the navRouteCoordinates column is already a marshaled JSON
// array (`json.RawMessage`), so we encrypt the existing bytes rather
// than round-tripping through `[]NavRouteCoordinate`. Returns `("",
// nil)` when raw is empty / `null` / `[]` (TS-compatible "absent"
// sentinels), matching readNavRouteCoordinates' fallback semantics on
// the JS side.
//
// The caller is responsible for passing valid JSON; we don't re-parse.
func EncryptJSONBytes(raw []byte, enc cryptox.Encryptor) (string, error) {
	if isEmptyJSON(raw) {
		return "", nil
	}
	ct, err := enc.EncryptString(string(raw))
	if err != nil {
		return "", fmt.Errorf("routeblob.EncryptJSONBytes: encrypt: %w", err)
	}
	return ct, nil
}

// DecryptJSONBytes is the low-level inverse of EncryptJSONBytes:
// returns the decrypted JSON bytes verbatim so the caller can stash
// them in a `json.RawMessage` (matching the existing Vehicle struct
// shape). Empty input returns `(nil, nil)`.
//
// Callers MUST validate the decrypted bytes parse to the expected
// shape before exposing them to consumers (the per-pair JSON.Unmarshal
// guard inside DecryptNavRoute is an example).
func DecryptJSONBytes(ciphertext string, enc cryptox.Encryptor) ([]byte, error) {
	if ciphertext == "" {
		return nil, nil
	}
	plain, err := enc.DecryptString(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("routeblob.DecryptJSONBytes: decrypt: %w", err)
	}
	if plain == "" {
		return nil, nil
	}
	return []byte(plain), nil
}

// isEmptyJSON returns true when raw is empty or one of the absent
// sentinels (`null`, `[]`). Whitespace-only blobs are also treated as
// absent — the Tesla pipeline never produces them but defensive
// callers that pre-trim their input shouldn't have to special-case the
// result here.
func isEmptyJSON(raw []byte) bool {
	if len(raw) == 0 {
		return true
	}
	// Trim ASCII whitespace without pulling in unicode tables.
	start, end := 0, len(raw)
	for start < end && isJSONSpace(raw[start]) {
		start++
	}
	for end > start && isJSONSpace(raw[end-1]) {
		end--
	}
	trimmed := raw[start:end]
	if len(trimmed) == 0 {
		return true
	}
	if string(trimmed) == "null" || string(trimmed) == "[]" {
		return true
	}
	return false
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
