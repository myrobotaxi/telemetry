package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Handler tests for the saved-places surface (MYR-321, rest-api.md §7.20).

const savedPlacesUserID = "cusr_places_0001"

// --- fakes -----------------------------------------------------------------

// fakeSavedPlacesRegistry is the store seam. It keeps the rows in a map keyed
// by kind so the upsert's "one row per kind" invariant is modelled rather than
// asserted about a slice.
type fakeSavedPlacesRegistry struct {
	rows map[string]SavedPlaceData

	listErr   error
	upsertErr error
	deleteErr error

	// lastUpsert records what actually reached the store, so the tests can
	// prove the handler passed the PATH kind and the TRIMMED label through.
	lastUpsert *SavedPlaceData
	deleted    []string
}

func newFakeSavedPlaces() *fakeSavedPlacesRegistry {
	return &fakeSavedPlacesRegistry{rows: map[string]SavedPlaceData{}}
}

func (f *fakeSavedPlacesRegistry) ListSavedPlaces(_ context.Context, _ string) ([]SavedPlaceData, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Home before work, matching ORDER BY kind.
	out := make([]SavedPlaceData, 0, 2)
	for _, k := range []string{"home", "work"} {
		if row, ok := f.rows[k]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeSavedPlacesRegistry) UpsertSavedPlace(_ context.Context, _ string, place SavedPlaceData) (SavedPlaceData, error) {
	if f.upsertErr != nil {
		return SavedPlaceData{}, f.upsertErr
	}
	captured := place
	f.lastUpsert = &captured
	f.rows[place.Kind] = place
	return place, nil
}

func (f *fakeSavedPlacesRegistry) DeleteSavedPlace(_ context.Context, _, kind string) (bool, error) {
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	f.deleted = append(f.deleted, kind)
	_, existed := f.rows[kind]
	delete(f.rows, kind)
	return existed, nil
}

// --- harness ---------------------------------------------------------------

// newSavedPlacesServer mounts the three routes on a real ServeMux so that
// r.PathValue("kind") is populated the way it is in production. Building the
// request by hand would leave the path value empty and make every kind look
// invalid — the test would pass for the wrong reason.
func newSavedPlacesServer(t *testing.T, reg SavedPlacesRegistry) *http.ServeMux {
	t.Helper()
	h := NewSavedPlacesHandler(&stubTokenValidator{userID: savedPlacesUserID}, reg, discardLogger())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/users/me/places", h.ServeList)
	mux.HandleFunc("PUT /api/users/me/places/{kind}", h.ServePut)
	mux.HandleFunc("DELETE /api/users/me/places/{kind}", h.ServeDelete)
	return mux
}

func savedPlacesCall(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(context.Background(), method, path, nil)
	} else {
		r = httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func decodeSavedPlace(t *testing.T, w *httptest.ResponseRecorder) savedPlaceResponse {
	t.Helper()
	var got savedPlaceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode place response: %v (body=%s)", err, w.Body.String())
	}
	return got
}

func decodeSavedPlacesList(t *testing.T, w *httptest.ResponseRecorder) savedPlacesListResponse {
	t.Helper()
	var got savedPlacesListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list response: %v (body=%s)", err, w.Body.String())
	}
	return got
}

const validPlaceBody = `{"label":"1 Ferry Building","latitude":37.7955,"longitude":-122.3937}`

// --- tests -----------------------------------------------------------------

// A brand-new account has saved nothing. That is the COMMON case on day one,
// so it must be a 200 with an empty ARRAY — not a 404, not null, and not two
// placeholder rows with absent coordinates.
func TestSavedPlaces_EmptyAccountGetsEmptyArrayNotNull(t *testing.T) {
	mux := newSavedPlacesServer(t, newFakeSavedPlaces())

	w := savedPlacesCall(t, mux, http.MethodGet, "/api/users/me/places", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// Assert on the RAW BYTES: `{"places":null}` unmarshals into a nil slice
	// that len() reports as 0, so a typed check alone would not catch the bug
	// this test exists for.
	if body := strings.TrimSpace(w.Body.String()); body != `{"places":[]}` {
		t.Fatalf("body = %s, want {\"places\":[]}", body)
	}
}

// The envelope is keyed on `places`, is sparse, and never grows a pagination
// cursor — this set is bounded by the kind enum at two rows.
func TestSavedPlaces_ListEnvelopeIsKeyedOnPlacesAndSparse(t *testing.T) {
	reg := newFakeSavedPlaces()
	reg.rows["work"] = SavedPlaceData{Kind: "work", Label: "HQ", Latitude: 37.3947, Longitude: -122.1503}

	mux := newSavedPlacesServer(t, reg)
	w := savedPlacesCall(t, mux, http.MethodGet, "/api/users/me/places", "")

	got := decodeSavedPlacesList(t, w)
	if len(got.Places) != 1 {
		t.Fatalf("places = %d, want 1 (sparse: home was never set)", len(got.Places))
	}
	if got.Places[0].Kind != "work" {
		t.Fatalf("kind = %q, want work", got.Places[0].Kind)
	}
	// No `items`, no cursor, no hasMore — an SDK pagination helper must not be
	// able to mistake this for a page.
	for _, forbidden := range []string{`"items"`, `"nextCursor"`, `"hasMore"`} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Errorf("envelope must not carry %s: %s", forbidden, w.Body.String())
		}
	}
}

// Both kinds set: home before work, and every field present on every row.
func TestSavedPlaces_ListReturnsBothKindsHomeFirst(t *testing.T) {
	reg := newFakeSavedPlaces()
	reg.rows["home"] = SavedPlaceData{Kind: "home", Label: "Casa", Latitude: 37.7955, Longitude: -122.3937}
	reg.rows["work"] = SavedPlaceData{Kind: "work", Label: "HQ", Latitude: 37.3947, Longitude: -122.1503}

	mux := newSavedPlacesServer(t, reg)
	got := decodeSavedPlacesList(t, savedPlacesCall(t, mux, http.MethodGet, "/api/users/me/places", ""))

	if len(got.Places) != 2 {
		t.Fatalf("places = %d, want 2", len(got.Places))
	}
	if got.Places[0].Kind != "home" || got.Places[1].Kind != "work" {
		t.Fatalf("order = %q,%q — want home,work", got.Places[0].Kind, got.Places[1].Kind)
	}
	if got.Places[0].Label != "Casa" || got.Places[0].Latitude != 37.7955 {
		t.Fatalf("home row = %+v", got.Places[0])
	}
}

// PUT echoes the stored place and is a 200 on FIRST write as well as on
// replace: the resource is the SLOT, which always exists in the URL space, so
// a create and an update are indistinguishable to the caller.
func TestSavedPlaces_PutEchoesStoredPlaceAnd200sOnCreateAndReplace(t *testing.T) {
	reg := newFakeSavedPlaces()
	mux := newSavedPlacesServer(t, reg)

	first := savedPlacesCall(t, mux, http.MethodPut, "/api/users/me/places/home", validPlaceBody)
	if first.Code != http.StatusOK {
		t.Fatalf("first write status = %d, want 200 (never 201)", first.Code)
	}
	got := decodeSavedPlace(t, first)
	if got.Kind != "home" || got.Label != "1 Ferry Building" ||
		got.Latitude != 37.7955 || got.Longitude != -122.3937 {
		t.Fatalf("echo = %+v", got)
	}

	// Replace: same slot, different place. Still 200, and a WHOLE replacement.
	second := savedPlacesCall(t, mux, http.MethodPut, "/api/users/me/places/home",
		`{"label":"New House","latitude":40.0,"longitude":-70.0}`)
	if second.Code != http.StatusOK {
		t.Fatalf("replace status = %d, want 200", second.Code)
	}
	if replaced := decodeSavedPlace(t, second); replaced.Label != "New House" || replaced.Latitude != 40 {
		t.Fatalf("replace echo = %+v", replaced)
	}
	if len(reg.rows) != 1 {
		t.Fatalf("replace minted a second row: %+v", reg.rows)
	}
}

// The kind that gets stored comes from the PATH, not from the body — the path
// is what routes and authorizes the write.
func TestSavedPlaces_PutTakesKindFromThePathNotTheBody(t *testing.T) {
	reg := newFakeSavedPlaces()
	mux := newSavedPlacesServer(t, reg)

	// No kind in the body at all: the ordinary case.
	w := savedPlacesCall(t, mux, http.MethodPut, "/api/users/me/places/work", validPlaceBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if reg.lastUpsert == nil || reg.lastUpsert.Kind != "work" {
		t.Fatalf("stored kind = %+v, want work (from the path)", reg.lastUpsert)
	}

	// Kind present and AGREEING is fine — a client may post a whole SavedPlace
	// back without stripping the field.
	agree := savedPlacesCall(t, mux, http.MethodPut, "/api/users/me/places/home",
		`{"kind":"home","label":"Casa","latitude":1,"longitude":2}`)
	if agree.Code != http.StatusOK {
		t.Fatalf("agreeing body status = %d, want 200", agree.Code)
	}

	// Kind present and DISAGREEING is a 400 — never honoured over the path. A
	// body that could redirect the write would let a stale client overwrite
	// Home while its URL said Work.
	conflict := savedPlacesCall(t, mux, http.MethodPut, "/api/users/me/places/work",
		`{"kind":"home","label":"Casa","latitude":1,"longitude":2}`)
	if conflict.Code != http.StatusBadRequest {
		t.Fatalf("mismatched body kind status = %d, want 400", conflict.Code)
	}
	if reg.rows["work"].Label == "Casa" {
		t.Fatal("a mismatched body must not write to either slot")
	}
}

// DELETE is 204 whether or not a row was there. A 404 would tell a retrying
// client that work it already completed had failed.
func TestSavedPlaces_DeleteIsIdempotent204(t *testing.T) {
	reg := newFakeSavedPlaces()
	reg.rows["home"] = SavedPlaceData{Kind: "home", Label: "Casa", Latitude: 1, Longitude: 2}
	mux := newSavedPlacesServer(t, reg)

	first := savedPlacesCall(t, mux, http.MethodDelete, "/api/users/me/places/home", "")
	if first.Code != http.StatusNoContent {
		t.Fatalf("first delete status = %d, want 204", first.Code)
	}
	if first.Body.Len() != 0 {
		t.Fatalf("204 must have no body, got %q", first.Body.String())
	}

	// Second delete: nothing there. Still 204, still no body.
	second := savedPlacesCall(t, mux, http.MethodDelete, "/api/users/me/places/home", "")
	if second.Code != http.StatusNoContent {
		t.Fatalf("re-delete status = %d, want 204 (idempotent, never 404)", second.Code)
	}

	// Deleting one slot must not touch the other.
	if _, gone := reg.rows["home"]; gone {
		t.Fatal("home row survived the delete")
	}
}

func TestSavedPlaces_DeleteOnlyTouchesTheNamedSlot(t *testing.T) {
	reg := newFakeSavedPlaces()
	reg.rows["home"] = SavedPlaceData{Kind: "home", Label: "Casa", Latitude: 1, Longitude: 2}
	reg.rows["work"] = SavedPlaceData{Kind: "work", Label: "HQ", Latitude: 3, Longitude: 4}
	mux := newSavedPlacesServer(t, reg)

	if got := savedPlacesCall(t, mux, http.MethodDelete, "/api/users/me/places/work", "").Code; got != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}
	if _, ok := reg.rows["home"]; !ok {
		t.Fatal("deleting work took home with it")
	}
}

// An unknown or wrongly-cased kind is a 400 on every verb that carries one.
// Case-sensitivity is deliberate: accepting 'Home' would let two spellings of
// one slot reach an upsert whose conflict target is the exact bytes.
func TestSavedPlaces_InvalidKindIs400OnEveryVerb(t *testing.T) {
	tests := []struct {
		name string
		kind string
	}{
		{"unknown slot", "gym"},
		{"wrong case", "Home"},
		{"upper case", "WORK"},
		{"empty-ish", "%20"},
		{"sql-ish", "home'"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := newFakeSavedPlaces()
			mux := newSavedPlacesServer(t, reg)

			put := savedPlacesCall(t, mux, http.MethodPut, "/api/users/me/places/"+tc.kind, validPlaceBody)
			if put.Code != http.StatusBadRequest {
				t.Errorf("PUT status = %d, want 400", put.Code)
			}
			del := savedPlacesCall(t, mux, http.MethodDelete, "/api/users/me/places/"+tc.kind, "")
			if del.Code != http.StatusBadRequest {
				t.Errorf("DELETE status = %d, want 400", del.Code)
			}
			if reg.lastUpsert != nil || len(reg.deleted) != 0 {
				t.Error("an invalid kind must never reach the store")
			}
		})
	}
}

// Body validation. Every case is a 400 and NONE of them reaches the store —
// the CHECK constraint is a backstop, not the validator.
func TestSavedPlaces_PutRejectsInvalidBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing label", `{"latitude":37.7,"longitude":-122.4}`},
		{"blank label", `{"label":"","latitude":37.7,"longitude":-122.4}`},
		{"whitespace-only label", `{"label":"   ","latitude":37.7,"longitude":-122.4}`},
		{"label over 200 runes", `{"label":"` + strings.Repeat("a", 201) + `","latitude":37.7,"longitude":-122.4}`},
		{"missing latitude", `{"label":"Home","longitude":-122.4}`},
		{"missing longitude", `{"label":"Home","latitude":37.7}`},
		{"latitude above range", `{"label":"Home","latitude":90.1,"longitude":-122.4}`},
		{"latitude below range", `{"label":"Home","latitude":-90.1,"longitude":-122.4}`},
		{"longitude above range", `{"label":"Home","latitude":37.7,"longitude":180.1}`},
		{"longitude below range", `{"label":"Home","latitude":37.7,"longitude":-180.1}`},
		{"latitude non-finite via overflow", `{"label":"Home","latitude":1e400,"longitude":-122.4}`},
		{"unknown key", `{"label":"Home","latitude":37.7,"longitude":-122.4,"lat":1}`},
		{"malformed json", `{"label":`},
		{"empty body", `{}`},
		{"wrong type", `{"label":"Home","latitude":"37.7","longitude":-122.4}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := newFakeSavedPlaces()
			mux := newSavedPlacesServer(t, reg)

			w := savedPlacesCall(t, mux, http.MethodPut, "/api/users/me/places/home", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
			}
			if reg.lastUpsert != nil {
				t.Fatal("an invalid body must never reach the store")
			}
		})
	}
}

// The boundary values are LEGAL. A test that only proves rejection would pass
// with `if true { reject }` — these pin the other side of the range.
func TestSavedPlaces_PutAcceptsBoundaryCoordinates(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"north pole", `{"label":"N","latitude":90,"longitude":0}`},
		{"south pole", `{"label":"S","latitude":-90,"longitude":0}`},
		{"antimeridian east", `{"label":"E","latitude":0,"longitude":180}`},
		{"antimeridian west", `{"label":"W","latitude":0,"longitude":-180}`},
		// Zero is a REAL coordinate, not a missing one — the pointer decode is
		// what keeps these distinguishable.
		{"null island", `{"label":"Zero","latitude":0,"longitude":0}`},
		{"label at exactly 200 runes", `{"label":"` + strings.Repeat("a", 200) + `","latitude":1,"longitude":2}`},
		// Runes, not bytes: 200 multi-byte characters is 600 bytes and must
		// still be accepted.
		{"200 multi-byte runes", `{"label":"` + strings.Repeat("é", 200) + `","latitude":1,"longitude":2}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := newSavedPlacesServer(t, newFakeSavedPlaces())
			w := savedPlacesCall(t, mux, http.MethodPut, "/api/users/me/places/home", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
			}
		})
	}
}

// The stored label is trimmed, so " Home " and "Home" are one label rather
// than two that render identically and compare unequal in a client cache.
func TestSavedPlaces_PutTrimsTheLabel(t *testing.T) {
	reg := newFakeSavedPlaces()
	mux := newSavedPlacesServer(t, reg)

	w := savedPlacesCall(t, mux, http.MethodPut, "/api/users/me/places/home",
		`{"label":"  1 Ferry Building  ","latitude":1,"longitude":2}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if reg.lastUpsert.Label != "1 Ferry Building" {
		t.Fatalf("stored label = %q, want it trimmed", reg.lastUpsert.Label)
	}
	if got := decodeSavedPlace(t, w).Label; got != "1 Ferry Building" {
		t.Fatalf("echoed label = %q, want it trimmed", got)
	}
}

// Every verb needs a bearer, and none of them may touch the store without one.
func TestSavedPlaces_AuthIsRequiredOnEveryVerb(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"list", http.MethodGet, "/api/users/me/places", ""},
		{"put", http.MethodPut, "/api/users/me/places/home", validPlaceBody},
		{"delete", http.MethodDelete, "/api/users/me/places/home", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name+" without a header", func(t *testing.T) {
			reg := newFakeSavedPlaces()
			mux := newSavedPlacesServer(t, reg)

			var r *http.Request
			if tc.body == "" {
				r = httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, nil)
			} else {
				r = httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, strings.NewReader(tc.body))
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r) // no Authorization header

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
			if reg.lastUpsert != nil || len(reg.deleted) != 0 {
				t.Fatal("an unauthenticated request must never reach the store")
			}
		})

		t.Run(tc.name+" with a rejected token", func(t *testing.T) {
			reg := newFakeSavedPlaces()
			h := NewSavedPlacesHandler(
				&stubTokenValidator{err: errors.New("expired")}, reg, discardLogger())
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/users/me/places", h.ServeList)
			mux.HandleFunc("PUT /api/users/me/places/{kind}", h.ServePut)
			mux.HandleFunc("DELETE /api/users/me/places/{kind}", h.ServeDelete)

			if got := savedPlacesCall(t, mux, tc.method, tc.path, tc.body).Code; got != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", got)
			}
		})
	}
}

// There is no userId anywhere in the path or the body, and there must never be
// one: this is a /users/me surface and the JWT subject IS the resource.
func TestSavedPlaces_SurfaceIsSelfScopedOnly(t *testing.T) {
	reg := newFakeSavedPlaces()
	mux := newSavedPlacesServer(t, reg)

	// A body carrying a userId is an UNKNOWN KEY and is rejected outright —
	// it is not silently ignored, which would let a client believe it worked.
	w := savedPlacesCall(t, mux, http.MethodPut, "/api/users/me/places/home",
		`{"userId":"cusr_someone_else","label":"Casa","latitude":1,"longitude":2}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown userId key", w.Code)
	}
	if reg.lastUpsert != nil {
		t.Fatal("a body naming another user must never reach the store")
	}
}

// A store failure is a 500 with the shared envelope — and the error body must
// not carry a coordinate or a label, on any path.
func TestSavedPlaces_StoreFailuresAre500AndLeakNothing(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*fakeSavedPlacesRegistry)
		method string
		path   string
		body   string
	}{
		{
			name:   "list fails",
			setup:  func(f *fakeSavedPlacesRegistry) { f.listErr = errors.New("db down") },
			method: http.MethodGet, path: "/api/users/me/places",
		},
		{
			name:   "upsert fails",
			setup:  func(f *fakeSavedPlacesRegistry) { f.upsertErr = errors.New("encrypt failed") },
			method: http.MethodPut, path: "/api/users/me/places/home", body: validPlaceBody,
		},
		{
			name:   "delete fails",
			setup:  func(f *fakeSavedPlacesRegistry) { f.deleteErr = errors.New("db down") },
			method: http.MethodDelete, path: "/api/users/me/places/home",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := newFakeSavedPlaces()
			tc.setup(reg)
			mux := newSavedPlacesServer(t, reg)

			w := savedPlacesCall(t, mux, tc.method, tc.path, tc.body)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", w.Code)
			}
			// P1 must not reach the wire in an error envelope.
			for _, leak := range []string{"37.7955", "-122.3937", "Ferry", "encrypt failed", "db down"} {
				if strings.Contains(w.Body.String(), leak) {
					t.Errorf("error body leaked %q: %s", leak, w.Body.String())
				}
			}
		})
	}
}

// A 400 must not echo the rejected input back either — an out-of-range
// coordinate is still a location, and error envelopes end up in crash
// reporters and log aggregators.
func TestSavedPlaces_ValidationErrorsDoNotEchoTheInput(t *testing.T) {
	mux := newSavedPlacesServer(t, newFakeSavedPlaces())

	w := savedPlacesCall(t, mux, http.MethodPut, "/api/users/me/places/home",
		`{"label":"12 Secret Lane","latitude":91.5,"longitude":-122.3937}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	for _, leak := range []string{"91.5", "-122.3937", "Secret"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("400 body echoed %q: %s", leak, w.Body.String())
		}
	}
}

// The response never omits a key, even when the value is a zero. `omitempty`
// on these floats would drop a legitimate coordinate.
func TestSavedPlaces_ResponseNeverOmitsAZeroCoordinate(t *testing.T) {
	reg := newFakeSavedPlaces()
	mux := newSavedPlacesServer(t, reg)

	w := savedPlacesCall(t, mux, http.MethodPut, "/api/users/me/places/home",
		`{"label":"Zero","latitude":0,"longitude":0}`)
	body := w.Body.String()
	for _, key := range []string{`"kind"`, `"label"`, `"latitude"`, `"longitude"`} {
		if !strings.Contains(body, key) {
			t.Errorf("response dropped %s: %s", key, body)
		}
	}
}

// Wrong methods on the mounted paths are 405 from the handler's own guard.
func TestSavedPlaces_WrongMethodIs405(t *testing.T) {
	h := NewSavedPlacesHandler(&stubTokenValidator{userID: savedPlacesUserID}, newFakeSavedPlaces(), discardLogger())

	tests := []struct {
		name    string
		handler http.HandlerFunc
		method  string
	}{
		{"list handler with PUT", h.ServeList, http.MethodPut},
		{"put handler with GET", h.ServePut, http.MethodGet},
		{"delete handler with POST", h.ServeDelete, http.MethodPost},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(context.Background(), tc.method, "/api/users/me/places", nil)
			r.Header.Set("Authorization", "Bearer token")
			w := httptest.NewRecorder()
			tc.handler(w, r)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", w.Code)
			}
		})
	}
}
