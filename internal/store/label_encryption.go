// Location-label encryption helpers (MYR-447).
//
// MYR-433 sealed the COORDINATES. It left the reverse-geocoded labels
// sitting in the next column over in plaintext — and "1600 Pennsylvania
// Ave NW, Washington DC" is a strictly more sensitive rendering of a
// position than "38.8977, -77.0365": it needs no map, no tooling and no
// interpretation to tell you where somebody was. This file is the string
// analogue of vehicle_gps_encryption.go and closes that gap for the eight
// label columns:
//
//	Vehicle."locationName"    Vehicle."locationAddress"
//	Vehicle."destinationName" Vehicle."destinationAddress"
//	Drive."startLocation"     Drive."startAddress"
//	Drive."endLocation"       Drive."endAddress"
//
// Naming convention is inherited from MYR-433: `<plaintext>Enc`, TEXT,
// nullable. NULL is the absent sentinel, which is what lets the geocode
// backfill's discovery predicate say `IS NULL`; AES-GCM ciphertext is
// nondeterministic, so `= ''` could never match a sealed value again.
//
// The empty string is absent at the encryption boundary too:
// cryptox.EncryptString("") == ("", nil) and DecryptString("") ==
// ("", nil). labelToEncColumn maps that to a NULL column write, so an
// empty label and a missing label are stored identically — which is
// correct, because neither carries a location.
//
// Failure posture mirrors resolveGPSPair: reads are FAIL-SOFT (a label
// that will not decrypt surfaces as "") but log at ERROR and increment
// telemetry_store_decrypt_failures_total, because a fail-soft read is
// otherwise indistinguishable from a car that simply has no address, and
// a key mistake would go unnoticed until a user complained. Writes are
// FAIL-CLOSED: an encrypt error aborts the write rather than silently
// dropping the label or, worse, leaving a stale one in place.

package store

import (
	"fmt"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// vehicleLabelColumns is the canonical iteration order for the four
// Vehicle label columns, named by their RETIRED plaintext column. The
// `*Enc` column name is always the plaintext name plus "Enc" — see
// plaintextToEncColumn, which this list must stay consistent with.
//
// Order matters only for readability of the generated SQL; every consumer
// keys by name.
var vehicleLabelColumns = []string{
	"locationName",
	"locationAddress",
	"destinationName",
	"destinationAddress",
}

// driveLabelColumns is the Drive-table counterpart. Kept separate rather
// than merged into one list because the two tables' write paths are
// entirely different (a dynamic UPDATE builder vs. fixed INSERT/UPDATE
// statements) and nothing iterates both.
var driveLabelColumns = []string{
	"startLocation",
	"startAddress",
	"endLocation",
	"endAddress",
}

// labelToEncString seals a label into base64-GCM ciphertext.
//
// Returns ("", nil) for the empty string — cryptox's absent sentinel,
// which every caller persists as a NULL column rather than an empty
// ciphertext. Distinguishing "no label" from "a label that happens to be
// empty" would buy nothing and cost a column value that decrypts to
// nothing.
func labelToEncString(plain string, enc cryptox.Encryptor) (string, error) {
	ct, err := enc.EncryptString(plain)
	if err != nil {
		return "", fmt.Errorf("encrypt label: %w", err)
	}
	return ct, nil
}

// labelToEncPtr is the fixed-statement counterpart of labelToEncString:
// it returns the *string pgx wants for a nullable TEXT parameter slot. A
// nil result writes NULL.
//
// Used by the Drive write paths, whose INSERT/UPDATE statements have a
// fixed parameter list and so cannot express "omit this column" the way
// the Vehicle dynamic UPDATE builder can.
func labelToEncPtr(plain string, enc cryptox.Encryptor) (*string, error) {
	ct, err := labelToEncString(plain, enc)
	if err != nil {
		return nil, err
	}
	if ct == "" {
		return nil, nil //nolint:nilnil // empty label: write NULL
	}
	return &ct, nil
}

// encStringToLabel is the read-path inverse: decrypt one `*Enc` column
// into the label it holds.
//
// Every failure mode collapses to the empty string, which consumers
// render as "no address known". That is deliberate and is the same
// bargain resolveGPSPair strikes: the only other option is falling back
// to the plaintext column, and a readable plaintext column is precisely
// the exposure this issue closes.
//
// encColumn is the `*Enc` column name and is used only for the log line
// and the metric label — it never carries a value. The decrypted label is
// NEVER logged: these columns are P1, "Log-safe: No".
func encStringToLabel(
	ct *string,
	enc cryptox.Encryptor,
	logger *slog.Logger,
	metrics Metrics,
	encColumn string,
) string {
	if enc == nil || ct == nil || *ct == "" {
		return ""
	}
	plain, err := enc.DecryptString(*ct)
	if err != nil {
		// ERROR, not Warn: this read is fail-soft, so without a loud
		// signal a key mistake presents as "this car has no address" and
		// nobody finds out until a user complains.
		if logger != nil {
			logger.Error("location label *Enc decrypt failed; surfacing no label",
				slog.String("column", encColumn),
				slog.String("error", err.Error()),
			)
		}
		if metrics != nil {
			metrics.IncDecryptFailure(encColumn)
		}
		return ""
	}
	return plain
}

// encStringToLabelPtr is encStringToLabel for the nullable Vehicle
// columns whose Go-side field is a *string (destinationName,
// destinationAddress). An absent or unreadable ciphertext yields nil, so
// the JSON surface emits `null` exactly as it did when the plaintext
// column was NULL.
func encStringToLabelPtr(
	ct *string,
	enc cryptox.Encryptor,
	logger *slog.Logger,
	metrics Metrics,
	encColumn string,
) *string {
	s := encStringToLabel(ct, enc, logger, metrics, encColumn)
	if s == "" {
		return nil
	}
	return &s
}
