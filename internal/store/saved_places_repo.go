package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// go_saved_places (migration 0023, MYR-321) holds at most two rows per person:
// where they live and where they work. The rest-api.md §7.20 endpoints are the
// only reader and writer; nothing else in the service consults it.
//
// Encryption contract (NFR-3.23, data-classification.md §1.17): lat/lng are P1
// GPS data stored ENCRYPT-ONLY — the table has no plaintext coordinate columns,
// so unlike the MYR-63 Vehicle dual-write path there is no fallback and the
// Encryptor is MANDATORY at construction. This mirrors RideRequestRepo exactly,
// including the lossless float↔string codec in vehicle_gps_encryption.go, so a
// cross-repo reader decrypts these columns identically to a ride's pickup.
//
// Encrypt failures FAIL THE WRITE (a saved place without coordinates is not a
// saved place) and decrypt failures FAIL THE READ (there is no plaintext column
// to degrade to, and surfacing a partial coordinate would send somebody to the
// wrong address). Neither ever falls back.
//
// `label` is P1 log-redacted but NOT app-level encrypted — the same tier split
// go_ride_requests makes between pickup_lat_enc and pickup_label.

// SavedPlaceKind is which slot a row occupies. Closed set, mirrored by the
// migration's CHECK constraint and by the contract enum
// (saved-places.schema.json $defs.SavedPlaceKind). Peers, not tiers: unlike
// SharePermission there is no ordering here.
type SavedPlaceKind string

const (
	// SavedPlaceHome is where the person lives.
	SavedPlaceHome SavedPlaceKind = "home"
	// SavedPlaceWork is where the person works.
	SavedPlaceWork SavedPlaceKind = "work"
)

// ValidSavedPlaceKinds is the canonical set, in the order the list read returns
// them (ORDER BY kind puts home before work). Declared once so the handler's
// path validation, the repo and the tests cannot disagree with the CHECK.
var ValidSavedPlaceKinds = []SavedPlaceKind{SavedPlaceHome, SavedPlaceWork}

// IsValidSavedPlaceKind reports whether s names a slot. Case-SENSITIVE and
// lowercase: 'Home' is not a synonym, it is a bad request. Matching loosely
// here would let two spellings of one slot reach an upsert whose conflict
// target is the exact bytes, and the person would end up with two homes.
func IsValidSavedPlaceKind(s string) bool {
	for _, k := range ValidSavedPlaceKinds {
		if string(k) == s {
			return true
		}
	}
	return false
}

// SavedPlace is one stored Home or Work row, coordinates already decrypted.
// Every field is populated on a read: the table's columns are all NOT NULL, so
// there is no half-set row to represent and no pointer to nil-check.
type SavedPlace struct {
	Kind      SavedPlaceKind
	Label     string
	Latitude  float64
	Longitude float64
}

// SavedPlacesRepo is the go_saved_places repository. Coordinates cross the
// boundary as plaintext float64 and are stored as base64 AES-256-GCM
// ciphertext; the repo is the encrypt/decrypt boundary, exactly as
// RideRequestRepo is for ride coordinates.
type SavedPlacesRepo struct {
	pool      *pgxpool.Pool
	encryptor cryptox.Encryptor
}

// NewSavedPlacesRepo constructs the repo. The Encryptor is REQUIRED —
// go_saved_places stores GPS coordinates encrypt-only, so a nil Encryptor would
// mean writing a person's home address in the clear. Fail at construction
// rather than at the first write (mirrors NewRideRequestRepo).
func NewSavedPlacesRepo(pool *pgxpool.Pool, encryptor cryptox.Encryptor) *SavedPlacesRepo {
	if encryptor == nil {
		panic("store.NewSavedPlacesRepo: encryptor must not be nil")
	}
	return &SavedPlacesRepo{pool: pool, encryptor: encryptor}
}

// ListForUser returns every saved place the account has set, home before work.
//
// A person with none gets an EMPTY, NON-NIL slice — never an error and never a
// nil map to range over. That is the state every account is in until somebody
// saves something, so the no-rows branch is the COMMON path, not an edge case.
// The result is SPARSE by construction: a kind that was never set, or was
// deleted, is simply absent, so callers must look a kind up rather than index.
func (r *SavedPlacesRepo) ListForUser(ctx context.Context, userID string) ([]SavedPlace, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("store.ListForUser: empty user id")
	}

	rows, err := r.pool.Query(ctx, querySavedPlacesForUser, userID)
	if err != nil {
		return nil, fmt.Errorf("store.ListForUser(user=%s): %w", userID, err)
	}
	defer rows.Close()

	// Non-nil zero-length: the JSON projection must render [] and not null.
	places := make([]SavedPlace, 0, len(ValidSavedPlaceKinds))
	for rows.Next() {
		place, err := r.scanSavedPlace(rows)
		if err != nil {
			return nil, fmt.Errorf("store.ListForUser(user=%s): %w", userID, err)
		}
		places = append(places, place)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListForUser(user=%s): iterate: %w", userID, err)
	}
	return places, nil
}

// Upsert writes one whole place and returns it as stored — the echo §7.20
// answers with. RETURNING rather than a write followed by a second read: a
// concurrent write between the two would make the client adopt a value that was
// never stored.
//
// A WHOLE-OBJECT upsert, deliberately unlike PushPrefsRepo.UpdatePrefs. Five
// independent switches can honestly be written one at a time; a label and the
// coordinate it describes are ONE fact, and a partial write would let a client
// keep the old label on a new pin — storing "Home" at an address that is not
// home. There is no COALESCE here for that reason.
//
// Idempotent: re-sending an identical place is a no-op that returns the same
// row, so a retry after a dropped response is safe.
func (r *SavedPlacesRepo) Upsert(ctx context.Context, userID string, place SavedPlace) (SavedPlace, error) {
	if strings.TrimSpace(userID) == "" {
		return SavedPlace{}, fmt.Errorf("store.Upsert: empty user id")
	}
	if !IsValidSavedPlaceKind(string(place.Kind)) {
		// Belt and braces behind the handler's own validation: reaching the
		// CHECK constraint would answer 500 where the truth is a 400.
		return SavedPlace{}, fmt.Errorf("store.Upsert(user=%s): invalid kind %q", userID, place.Kind)
	}

	latEnc, lngEnc, err := r.encryptCoords(place.Latitude, place.Longitude)
	if err != nil {
		return SavedPlace{}, fmt.Errorf("store.Upsert(user=%s, kind=%s): %w", userID, place.Kind, err)
	}

	row := r.pool.QueryRow(ctx, queryUpsertSavedPlace,
		userID, string(place.Kind), latEnc, lngEnc, place.Label)
	stored, err := r.scanSavedPlace(row)
	if err != nil {
		return SavedPlace{}, fmt.Errorf("store.Upsert(user=%s, kind=%s): %w", userID, place.Kind, err)
	}
	return stored, nil
}

// Delete forgets one saved place. Returns whether a row was actually removed;
// deleting a kind that was never set is NOT an error, which is what lets the
// endpoint answer 204 either way. §7.20 is idempotent by design: a client that
// retries a dropped DELETE must not be told 404 for work it already completed.
func (r *SavedPlacesRepo) Delete(ctx context.Context, userID string, kind SavedPlaceKind) (bool, error) {
	if strings.TrimSpace(userID) == "" {
		return false, fmt.Errorf("store.Delete: empty user id")
	}
	if !IsValidSavedPlaceKind(string(kind)) {
		return false, fmt.Errorf("store.Delete(user=%s): invalid kind %q", userID, kind)
	}

	tag, err := r.pool.Exec(ctx, queryDeleteSavedPlace, userID, string(kind))
	if err != nil {
		return false, fmt.Errorf("store.Delete(user=%s, kind=%s): %w", userID, kind, err)
	}
	return tag.RowsAffected() > 0, nil
}
