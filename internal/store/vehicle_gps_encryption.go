// Vehicle-GPS encryption helpers for the MYR-63 dual-write rollout
// (NFR-3.23). Mirrors the byte-compatible TS helper at
// ../my-robo-taxi/src/lib/vehicle-gps-encryption.ts so writes from
// either side decrypt cleanly on the other.
//
// Atomic-pair semantics: latitude and longitude are persisted as separate
// columns but consumed as a synchronized pair (vehicle-state-schema.md
// §3.3 GPS predicates). A half-pair *Enc — one column populated with
// ciphertext while its mate is NULL — is treated as corrupt: read paths
// fall back to plaintext for the entire pair, write paths skip the
// ciphertext write rather than emit a corrupt row. Either choice
// preserves the invariant.
//
// Float ↔ string conversion: Go's strconv.FormatFloat with -1 precision
// + ParseFloat is the lossless round-trip for finite IEEE-754 doubles
// and matches the TS side's String(x)/Number(s) byte-for-byte.

package store

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// gpsPair is one of the three latitude/longitude column pairs the
// Vehicle table tracks. Naming the pairs lets the helpers iterate
// instead of repeating logic per column.
type gpsPair struct {
	lat string // plaintext column name on the Go-side struct, also the SDK field
	lng string
}

// gpsPairs is the canonical iteration order used by every read/write
// path. Mirrors PAIRS in vehicle-gps-encryption.ts.
var gpsPairs = []gpsPair{
	{lat: "latitude", lng: "longitude"},
	{lat: "destinationLatitude", lng: "destinationLongitude"},
	{lat: "originLatitude", lng: "originLongitude"},
}

// encryptedGPSPair holds the *Enc ciphertext (or empty when the half is
// absent) for one (lat, lng) pair — used by the write path to load the
// values into a VehicleUpdate before flush.
type encryptedGPSPair struct {
	latEnc *string
	lngEnc *string
}

// floatToEncString converts a *float64 to base64-GCM ciphertext via the
// configured Encryptor. Returns ("", nil) for nil — callers interpret
// that as "leave the *Enc column NULL".
//
// strconv.FormatFloat with prec=-1 produces the shortest decimal that
// round-trips exactly through ParseFloat — the same semantics the TS
// side gets from String(x). Avoid %g/%v which choose presentation, not
// round-trip, precision.
func floatToEncString(v *float64, enc cryptox.Encryptor) (string, error) {
	if v == nil {
		return "", nil
	}
	plain := strconv.FormatFloat(*v, 'g', -1, 64)
	ct, err := enc.EncryptString(plain)
	if err != nil {
		return "", fmt.Errorf("encrypt: %w", err)
	}
	return ct, nil
}

// encStringToFloat is the read-path inverse: decrypt a ciphertext, parse
// as float64. Returns (nil, nil) for empty / unparseable input — both
// states are treated as "absent" by the caller. nilnil is intentional
// here: nil pointer + nil error means "no value to surface" and matches
// the TS `Number.isFinite` guard. Only genuine cryptographic failures
// (auth-tag mismatch, version unknown) escalate to a non-nil error.
//
//nolint:nilnil // see comment: (nil,nil) is the absent-value sentinel.
func encStringToFloat(ciphertext *string, enc cryptox.Encryptor) (*float64, error) {
	if ciphertext == nil || *ciphertext == "" {
		return nil, nil
	}
	plain, err := enc.DecryptString(*ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	if plain == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(plain, 64)
	if err != nil {
		return nil, fmt.Errorf("parse %q as float: %w", plain, err)
	}
	return &f, nil
}

// resolveGPSPair implements the read-path preference rule for one
// (lat, lng) pair: ciphertext wins when BOTH halves are present;
// half-pair *Enc is corrupt and forces a plaintext fallback for the
// entire pair (the TS side's identical guard).
//
// Returns the resolved (lat, lng) pointers — nil when the column is
// truly absent. The boolean return distinguishes "fell back to
// plaintext after a half-pair was detected" from a clean read so
// callers can warn at most once per row.
func resolveGPSPair(
	latEncCT, lngEncCT *string,
	latPT, lngPT *float64,
	enc cryptox.Encryptor,
	logger *slog.Logger,
	pair gpsPair,
) (lat, lng *float64) {
	latPresent := latEncCT != nil && *latEncCT != ""
	lngPresent := lngEncCT != nil && *lngEncCT != ""

	if latPresent != lngPresent {
		// Half-pair: the row is corrupt — ignore both *Enc halves and
		// fall back to plaintext. This is the read-side mirror of
		// buildEncryptedVehicleGPSWrite's input check.
		if logger != nil {
			logger.Warn("vehicle GPS half-pair *Enc detected; falling back to plaintext",
				slog.String("pair", pair.lat+"/"+pair.lng),
				slog.Bool("lat_enc_present", latPresent),
				slog.Bool("lng_enc_present", lngPresent),
			)
		}
		return latPT, lngPT
	}

	if !latPresent {
		// Neither half encrypted — legacy plaintext row. Pre-rollout
		// path; expected during the dual-write window.
		return latPT, lngPT
	}

	latRes, latErr := encStringToFloat(latEncCT, enc)
	lngRes, lngErr := encStringToFloat(lngEncCT, enc)
	if latErr != nil || lngErr != nil {
		// Decrypt-or-parse failure on a fully populated *Enc pair.
		// Treat the pair as corrupt: fall back to plaintext rather
		// than surface partial GPS to consumers. Logged at Warn so the
		// failure shows up in operator dashboards without crashing the
		// read path.
		if logger != nil {
			logger.Warn("vehicle GPS *Enc decrypt failed; falling back to plaintext",
				slog.String("pair", pair.lat+"/"+pair.lng),
				slog.Any("lat_err", latErr),
				slog.Any("lng_err", lngErr),
			)
		}
		return latPT, lngPT
	}
	return latRes, lngRes
}

// buildEncryptedGPSPair encrypts a (lat, lng) input pair into the matching
// *Enc ciphertexts, enforcing the atomic-pair input invariant.
//
// Returns (encryptedGPSPair{}, false) for half-pair input — the caller
// MUST NOT touch the *Enc columns in that case. Logged at Warn so the
// regression is visible.
//
// nil/nil input is a no-op (returns hasWrite=false): both columns are
// left untouched. Both-non-nil yields a populated pair the caller can
// dual-write alongside the plaintext UPDATE.
func buildEncryptedGPSPair(
	lat, lng *float64,
	enc cryptox.Encryptor,
	logger *slog.Logger,
	pair gpsPair,
) (encryptedGPSPair, bool, error) {
	latProvided := lat != nil
	lngProvided := lng != nil

	if !latProvided && !lngProvided {
		return encryptedGPSPair{}, false, nil
	}
	if latProvided != lngProvided {
		if logger != nil {
			logger.Warn("vehicle GPS half-pair input; *Enc columns left untouched",
				slog.String("pair", pair.lat+"/"+pair.lng),
				slog.Bool("lat_provided", latProvided),
				slog.Bool("lng_provided", lngProvided),
			)
		}
		return encryptedGPSPair{}, false, nil
	}

	latCT, err := floatToEncString(lat, enc)
	if err != nil {
		return encryptedGPSPair{}, false, fmt.Errorf("encrypt %s: %w", pair.lat, err)
	}
	lngCT, err := floatToEncString(lng, enc)
	if err != nil {
		return encryptedGPSPair{}, false, fmt.Errorf("encrypt %s: %w", pair.lng, err)
	}
	return encryptedGPSPair{latEnc: &latCT, lngEnc: &lngCT}, true, nil
}
