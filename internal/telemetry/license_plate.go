package telemetry

import (
	"errors"
	"strings"
)

// License-plate normalization + validation (MYR-286).
//
// Split out of vehicle_plate_handler.go so the rule has one home and both the
// handler and its tests reference the same definition. The ORDER matters and is
// part of the contract (rest-api.md §7.14,
// docs/contracts/schemas/vehicle-state.schema.json): normalize FIRST, then
// validate the NORMALIZED value. "  abc 1234  " is a valid submission because
// it normalizes to "ABC 1234"; validating the raw input would reject it on the
// lowercase letters and count the surrounding spaces against the length cap.
//
// Because normalization always precedes storage, every value the read paths
// return is already normalized — the schema tells consumers not to re-normalize
// or re-validate before display.

// maxLicensePlateLen is the post-normalization character cap, matching
// `maxLength: 10` in the vehicle-state / vehicle-summary schemas. US plates top
// out at 8 characters; 10 leaves headroom without letting the field become a
// free-text note.
const maxLicensePlateLen = 10

// ErrPlateTooLong / ErrPlateCharset are the two validation failures, both
// surfaced as `400 invalid_request`. Their messages describe the RULE and
// deliberately never echo the submitted value, which is P1
// (data-classification.md §1.3) and would otherwise leak into the error
// envelope and any client-side log of it.
var (
	ErrPlateTooLong = errors.New("plate must be at most 10 characters after normalization")
	ErrPlateCharset = errors.New("plate may contain only letters, digits, spaces, and hyphens")
)

// NormalizeLicensePlate applies the canonical write-side normalization:
// trim leading/trailing whitespace, then uppercase. Nothing else — interior
// spacing is preserved verbatim, because "ABC 1234" and "ABC1234" are different
// plates in some jurisdictions and collapsing them would silently rewrite the
// owner's answer.
//
// Uppercasing is ASCII-safe here: any character that survives validation is in
// [A-Z0-9 -], and strings.ToUpper leaves those untouched. A non-ASCII input is
// uppercased harmlessly and then rejected by ValidateLicensePlate.
func NormalizeLicensePlate(plate string) string {
	return strings.ToUpper(strings.TrimSpace(plate))
}

// ValidateLicensePlate checks an ALREADY-NORMALIZED plate against the wire
// contract: at most maxLicensePlateLen characters and drawn only from
// `^[A-Z0-9 -]*$`.
//
// The empty string is VALID and means "clear the plate" — the pattern is
// deliberately `*` (not `+`) so a clear is an ordinary write rather than a
// separate DELETE verb.
//
// Length is measured in bytes, which is exact here: every character the charset
// permits is single-byte ASCII, so a multi-byte input is rejected on charset
// grounds before byte-vs-rune length could ever disagree.
func ValidateLicensePlate(plate string) error {
	if len(plate) > maxLicensePlateLen {
		return ErrPlateTooLong
	}
	for i := 0; i < len(plate); i++ {
		if !isLicensePlateByte(plate[i]) {
			return ErrPlateCharset
		}
	}
	return nil
}

// isLicensePlateByte reports whether b is a member of the `[A-Z0-9 -]` charset.
// Hand-rolled rather than regexp so the hot path allocates nothing and the
// charset is readable at the point of enforcement.
func isLicensePlateByte(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == ' ' || b == '-':
		return true
	default:
		return false
	}
}
