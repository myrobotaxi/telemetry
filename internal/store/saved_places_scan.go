// SavedPlacesRepo scan + coordinate-crypto helpers (MYR-321). Split from
// saved_places_repo.go so the accessor file stays focused on the public
// surface, mirroring the ride_request_repo.go / ride_request_scan.go split.
//
// Reuses the lossless float<->string codec from vehicle_gps_encryption.go
// (strconv round-trip, byte-compatible with the TS helpers) so a cross-repo
// reader decrypts these columns identically to a ride's pickup coordinates.

package store

import (
	"fmt"

	"github.com/jackc/pgx/v5"
)

// encryptCoords encrypts a place's coordinate pair for the *_enc columns.
//
// Unlike the Vehicle dual-write path there is no plaintext fallback column, so
// ANY encrypt failure fails the write — storing a place with one axis missing,
// or with a coordinate in the clear, are both worse than storing nothing.
func (r *SavedPlacesRepo) encryptCoords(latitude, longitude float64) (latEnc, lngEnc string, err error) {
	lat := latitude
	lng := longitude
	latEnc, err = floatToEncString(&lat, r.encryptor)
	if err != nil {
		return "", "", fmt.Errorf("encrypt latitude: %w", err)
	}
	lngEnc, err = floatToEncString(&lng, r.encryptor)
	if err != nil {
		return "", "", fmt.Errorf("encrypt longitude: %w", err)
	}
	return latEnc, lngEnc, nil
}

// decryptCoord decrypts one required coordinate column.
//
// A NULL/empty or non-numeric result is CORRUPTION, not an absent value: the
// column is NOT NULL and encrypt-only, so there is no legitimate empty state.
// It escalates to an error rather than the (nil, nil) absent sentinel the
// Vehicle fallback path tolerates — same posture as
// RideRequestRepo.decryptCoord, and for a sharper reason here, since silently
// surfacing a zero coordinate would route somebody to Null Island.
func (r *SavedPlacesRepo) decryptCoord(column, ciphertext string) (float64, error) {
	v, err := encStringToFloat(&ciphertext, r.encryptor)
	if err != nil {
		return 0, fmt.Errorf("decrypt %s: %w", column, err)
	}
	if v == nil {
		return 0, fmt.Errorf("decrypt %s: corrupt or empty ciphertext", column)
	}
	return *v, nil
}

// scanSavedPlace scans one savedPlaceColumns row and decrypts the coordinate
// ciphertexts into the returned record. Shared by the list read and both
// writers' RETURNING clauses, which is what keeps a stored row and an echoed
// row provably the same shape: the echo is not built from the request, it is
// scanned back out of the database through this exact path.
func (r *SavedPlacesRepo) scanSavedPlace(row pgx.Row) (SavedPlace, error) {
	var (
		place  SavedPlace
		kind   string
		latEnc string
		lngEnc string
	)
	if err := row.Scan(&kind, &latEnc, &lngEnc, &place.Label); err != nil {
		return SavedPlace{}, fmt.Errorf("scan saved place: %w", err)
	}

	place.Kind = SavedPlaceKind(kind)

	lat, err := r.decryptCoord("lat_enc", latEnc)
	if err != nil {
		return SavedPlace{}, err
	}
	lng, err := r.decryptCoord("lng_enc", lngEnc)
	if err != nil {
		return SavedPlace{}, err
	}
	place.Latitude = lat
	place.Longitude = lng
	return place, nil
}
