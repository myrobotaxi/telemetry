package telemetry

import "context"

// The wire + seam types for the saved-places surface (MYR-321, rest-api.md
// §7.20). Shapes mirror contracts saved-places.schema.json exactly.

// SavedPlaceData is one saved place as it crosses the store seam — coordinates
// already decrypted. Mirrors store.SavedPlace field for field; the cmd adapter
// converts, so internal/telemetry imports no store package.
type SavedPlaceData struct {
	Kind      string
	Label     string
	Latitude  float64
	Longitude float64
}

// SavedPlacesRegistry is the persistence seam for the three endpoints.
// Satisfied by an adapter over *store.SavedPlacesRepo.
type SavedPlacesRegistry interface {
	// ListSavedPlaces returns every slot the account has set — 0, 1 or 2 rows,
	// home before work. An account with none gets an empty slice, NOT an error:
	// "never saved anything" is the state every account starts in.
	ListSavedPlaces(ctx context.Context, userID string) ([]SavedPlaceData, error)
	// UpsertSavedPlace writes one WHOLE place and returns it as stored — the
	// echo the client adopts. Not a partial update; see savedPlaceRequest.
	UpsertSavedPlace(ctx context.Context, userID string, place SavedPlaceData) (SavedPlaceData, error)
	// DeleteSavedPlace forgets one slot. The bool reports whether a row was
	// actually removed; deleting a slot that was never set is NOT an error,
	// which is what lets the endpoint answer 204 either way.
	DeleteSavedPlace(ctx context.Context, userID, kind string) (bool, error)
}

// savedPlaceResponse is the §7.20 single-place body — the PUT echo, and the
// element type of the list envelope.
//
// EVERY FIELD IS ALWAYS PRESENT and none carries `omitempty`, deliberately.
// `omitempty` on a float64 drops 0, and 0 is a real coordinate (the equator and
// the prime meridian both pass through inhabited places); a client reading an
// absent key as "unknown" would render a saved place with no pin. The same
// reasoning §7.19 gives for its booleans, with a sharper failure mode.
type savedPlaceResponse struct {
	Kind      string  `json:"kind"`
	Label     string  `json:"label"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// newSavedPlaceResponse projects the domain type onto the wire shape.
//
// A struct CONVERSION rather than a field-by-field literal, and the conversion
// is the guard: Go permits it only while the two shapes have identical field
// names and types (tags are ignored), so adding a field to SavedPlaceData and
// forgetting the response fails to COMPILE here. Same technique as
// newPrefsResponse, and the same caveat — it catches additions and renames but
// NOT a reordering of two same-typed fields, so keep Latitude before Longitude
// in both declarations. Swapping them would compile and silently mirror every
// saved place across the globe; the handler tests are what would catch it.
func newSavedPlaceResponse(p SavedPlaceData) savedPlaceResponse {
	return savedPlaceResponse(p)
}

// savedPlacesListResponse is the §7.20 GET envelope.
//
// Keyed on `places`, deliberately NOT the `items` + nextCursor + hasMore
// envelope of the cursor-paginated lists: this set is bounded BY THE KIND ENUM
// at two rows, so there is nothing to paginate and an SDK pagination helper
// must not mistake it for a page (contracts SavedPlacesResponse).
type savedPlacesListResponse struct {
	Places []savedPlaceResponse `json:"places"`
}

// savedPlaceRequest is the PUT body: the whole place, minus the kind the path
// already names.
//
// WHOLE-OBJECT, NOT PARTIAL — the opposite of prefsRequest, and the difference
// is the shape of the data rather than a style choice. Five notification
// switches are genuinely independent, so a partial write lets two phones change
// two categories without clobbering each other. A label and the coordinate it
// describes are ONE fact: a partial write there would let a client move the pin
// while keeping a label that no longer describes it, storing "1 Ferry Building"
// at an address three miles away.
//
// The pointers therefore mean something different here than they do on
// prefsRequest. They are not an "omitted means leave alone" signal — they exist
// only so the handler can tell an OMITTED key from an explicit zero and reject
// the former, since `latitude: 0` is a legitimate coordinate that a plain
// float64 would make indistinguishable from a missing one.
type savedPlaceRequest struct {
	// Kind is OPTIONAL and redundant with the path segment, which is what
	// actually routes the write. Present only so a client holding a whole
	// SavedPlace can post it back without stripping a field. When present it
	// MUST equal the path (validated in decode) — a body that could redirect
	// the write would let a stale client overwrite Home while its URL said
	// Work.
	Kind      *string  `json:"kind"`
	Label     *string  `json:"label"`
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
}
