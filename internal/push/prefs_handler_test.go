package push

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// MYR-349 §7.19 — the notification-preference endpoints.

// fakePrefsRegistry records exactly what the handler handed the store, so the
// tests can prove the user id comes from the JWT and never from the body, and
// that an omitted key travels as nil rather than as false.
type fakePrefsRegistry struct {
	stored   Prefs
	readErr  error
	writeErr error

	readCalled  bool
	writeCalled bool
	gotUserID   string
	gotUpdate   PrefsUpdate
}

func newFakePrefsRegistry() *fakePrefsRegistry {
	return &fakePrefsRegistry{stored: DefaultPrefs()}
}

func (f *fakePrefsRegistry) PrefsForUser(_ context.Context, userID string) (Prefs, error) {
	f.readCalled = true
	f.gotUserID = userID
	if f.readErr != nil {
		return Prefs{}, f.readErr
	}
	return f.stored, nil
}

func (f *fakePrefsRegistry) UpdatePrefs(_ context.Context, userID string, update PrefsUpdate) (Prefs, error) {
	f.writeCalled = true
	f.gotUserID, f.gotUpdate = userID, update
	if f.writeErr != nil {
		return Prefs{}, f.writeErr
	}
	// Mirror the store's partial-write semantics so the handler tests exercise
	// the same "omitted means leave alone" rule the SQL keeps.
	apply := func(dst *bool, src *bool) {
		if src != nil {
			*dst = *src
		}
	}
	apply(&f.stored.RideLifecycle, update.RideLifecycle)
	apply(&f.stored.DriveStarted, update.DriveStarted)
	apply(&f.stored.DriveCompleted, update.DriveCompleted)
	apply(&f.stored.ChargingComplete, update.ChargingComplete)
	apply(&f.stored.ViewerJoined, update.ViewerJoined)
	return f.stored, nil
}

const prefsUserID = "cuser-prefs-001"

// doPrefsRequest mounts the handler on a real mux and performs one request.
func doPrefsRequest(
	t *testing.T,
	registry PrefsRegistry,
	auth tokenValidator,
	method, body string,
	withAuth bool,
) *httptest.ResponseRecorder {
	t.Helper()
	h := NewPrefsHandler(auth, registry, discardLogger())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/users/me/push-prefs", h.ServeGet)
	mux.HandleFunc("PUT /api/users/me/push-prefs", h.ServePut)

	req := httptest.NewRequestWithContext(context.Background(), method, "/api/users/me/push-prefs", strings.NewReader(body))
	if withAuth {
		req.Header.Set("Authorization", "Bearer test-token")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func boolptr(b bool) *bool { return &b }

// decodePrefsBody decodes a §7.19 response into a raw map, so the tests can
// assert on the WIRE KEYS rather than on a Go struct that would happily accept
// a renamed field. MYR-362's lesson from the other side of the wire: a client's
// optional property decodes a wrong key to nil silently, so the server's own
// tests have to pin the exact strings it emits.
func decodePrefsBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

// TestPrefsHandler_GetEmitsAllFiveKeys is the contract's shape assertion. All
// five keys, always, with no omitempty — an absent key would be read by the app
// as its own default and would render a switch in the wrong position.
func TestPrefsHandler_GetEmitsAllFiveKeys(t *testing.T) {
	registry := newFakePrefsRegistry()
	registry.stored = Prefs{
		RideLifecycle:    true,
		DriveStarted:     false,
		DriveCompleted:   true,
		ChargingComplete: false,
		ViewerJoined:     false,
	}

	rec := doPrefsRequest(t, registry, &stubTokenValidator{userID: prefsUserID}, http.MethodGet, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	body := decodePrefsBody(t, rec)
	want := map[string]bool{
		"rideLifecycle":    true,
		"driveStarted":     false,
		"driveCompleted":   true,
		"chargingComplete": false,
		"viewerJoined":     false,
	}
	if len(body) != len(want) {
		t.Errorf("response has %d keys (%v), want exactly %d", len(body), body, len(want))
	}
	for key, expected := range want {
		got, ok := body[key]
		if !ok {
			t.Errorf("key %q missing — every category must be present in every response, "+
				"including the ones that are false", key)
			continue
		}
		if got != expected {
			t.Errorf("%s = %v, want %v", key, got, expected)
		}
	}
	if registry.gotUserID != prefsUserID {
		t.Errorf("read for user %q, want the JWT subject %q", registry.gotUserID, prefsUserID)
	}
}

// TestPrefsHandler_FalseSurvivesTheWire is the omitempty guard, stated on its
// own because it is the single most likely way this endpoint would break: a
// `json:"...,omitempty"` added during a tidy-up drops every `false`, which is
// exactly the half of each value the feature exists to carry.
func TestPrefsHandler_FalseSurvivesTheWire(t *testing.T) {
	registry := newFakePrefsRegistry()
	registry.stored = Prefs{} // every category off

	rec := doPrefsRequest(t, registry, &stubTokenValidator{userID: prefsUserID}, http.MethodGet, "", true)
	body := decodePrefsBody(t, rec)

	for _, key := range []string{"rideLifecycle", "driveStarted", "driveCompleted", "chargingComplete", "viewerJoined"} {
		got, ok := body[key]
		if !ok {
			t.Errorf("key %q was dropped when false — omitempty has crept onto a preference "+
				"bool, and every switch a user turns OFF now reads as absent", key)
			continue
		}
		if got != false {
			t.Errorf("%s = %v, want false", key, got)
		}
	}
}

// TestPrefsHandler_PutIsPartial proves an omitted key travels as nil (leave
// alone) rather than as false. A whole-object PUT would let one screen switch
// off four categories its user never touched.
func TestPrefsHandler_PutIsPartial(t *testing.T) {
	registry := newFakePrefsRegistry()

	rec := doPrefsRequest(t, registry, &stubTokenValidator{userID: prefsUserID},
		http.MethodPut, `{"chargingComplete":false}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	got := registry.gotUpdate
	if got.ChargingComplete == nil || *got.ChargingComplete != false {
		t.Errorf("chargingComplete update = %v, want a non-nil false", got.ChargingComplete)
	}
	for name, ptr := range map[string]*bool{
		"rideLifecycle":  got.RideLifecycle,
		"driveStarted":   got.DriveStarted,
		"driveCompleted": got.DriveCompleted,
		"viewerJoined":   got.ViewerJoined,
	} {
		if ptr != nil {
			t.Errorf("%s update = %v, want nil — an omitted key must mean LEAVE ALONE, "+
				"never 'set to the zero value'", name, *ptr)
		}
	}
}

// TestPrefsHandler_PutEchoesTheWholeSetAfterTheWrite pins the echo. The client
// adopts this body rather than the bool it sent, so it must carry the four
// categories the request never mentioned.
func TestPrefsHandler_PutEchoesTheWholeSetAfterTheWrite(t *testing.T) {
	registry := newFakePrefsRegistry()
	// Somebody had already silenced drive-started on another device.
	registry.stored = DefaultPrefs()
	registry.stored.DriveStarted = false

	rec := doPrefsRequest(t, registry, &stubTokenValidator{userID: prefsUserID},
		http.MethodPut, `{"chargingComplete":false}`, true)
	body := decodePrefsBody(t, rec)

	if body["chargingComplete"] != false {
		t.Errorf("chargingComplete = %v, want false — the echo must reflect the write", body["chargingComplete"])
	}
	if body["driveStarted"] != false {
		t.Errorf("driveStarted = %v, want false — the echo must carry the categories the "+
			"REQUEST never mentioned, or a client adopting it would resurrect a "+
			"preference somebody switched off elsewhere", body["driveStarted"])
	}
	if body["rideLifecycle"] != true || body["driveCompleted"] != true || body["viewerJoined"] != true {
		t.Errorf("untouched categories changed: %v", body)
	}
}

// TestPrefsHandler_PutEmptyObjectIsALegalNoOp — §7.19 defines `{}` as a plain
// read-after-write.
func TestPrefsHandler_PutEmptyObjectIsALegalNoOp(t *testing.T) {
	registry := newFakePrefsRegistry()
	registry.stored.ViewerJoined = false

	rec := doPrefsRequest(t, registry, &stubTokenValidator{userID: prefsUserID}, http.MethodPut, `{}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodePrefsBody(t, rec)
	if body["viewerJoined"] != false {
		t.Errorf("viewerJoined = %v, want the stored false", body["viewerJoined"])
	}
}

// TestPrefsHandler_PutRejectsUnknownKeys is the anti-lie guard one layer down.
// On a body where EVERY key is optional, a typo'd or renamed category would
// otherwise return 200 having changed nothing — the client would show the
// switch in its new position and the notification would keep arriving, which is
// precisely the defect MYR-349 exists to remove.
func TestPrefsHandler_PutRejectsUnknownKeys(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "typo", body: `{"chargingComplet":false}`},
		{name: "snake_case instead of camelCase", body: `{"charging_complete":false}`},
		{name: "a category that does not exist", body: `{"tipsAndProductNews":false}`},
		{name: "malformed json", body: `{"chargingComplete":`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := newFakePrefsRegistry()
			rec := doPrefsRequest(t, registry, &stubTokenValidator{userID: prefsUserID},
				http.MethodPut, tt.body, true)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if registry.writeCalled {
				t.Error("the store was written despite a rejected body")
			}
		})
	}
}

// TestPrefsHandler_RequiresAuth — both verbs are 401 without a valid bearer,
// and neither touches the store.
func TestPrefsHandler_RequiresAuth(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		body     string
		withAuth bool
		auth     tokenValidator
	}{
		{name: "GET without header", method: http.MethodGet, auth: &stubTokenValidator{userID: prefsUserID}},
		{name: "PUT without header", method: http.MethodPut, body: `{}`, auth: &stubTokenValidator{userID: prefsUserID}},
		{
			name: "GET with a rejected token", method: http.MethodGet, withAuth: true,
			auth: &stubTokenValidator{err: errors.New("expired")},
		},
		{
			name: "PUT with a rejected token", method: http.MethodPut, body: `{}`, withAuth: true,
			auth: &stubTokenValidator{err: errors.New("expired")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := newFakePrefsRegistry()
			rec := doPrefsRequest(t, registry, tt.auth, tt.method, tt.body, tt.withAuth)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if registry.readCalled || registry.writeCalled {
				t.Error("the store was touched on an unauthenticated request")
			}
		})
	}
}

// TestPrefsHandler_StoreFailureIs500 — a read or write that errors surfaces as
// a typed internal error, never as a fabricated set of defaults. A GET that
// answered "everything on" after a database failure would tell somebody their
// switches had been reset.
func TestPrefsHandler_StoreFailureIs500(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		registry := newFakePrefsRegistry()
		registry.readErr = errors.New("connection refused")
		rec := doPrefsRequest(t, registry, &stubTokenValidator{userID: prefsUserID}, http.MethodGet, "", true)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("write", func(t *testing.T) {
		registry := newFakePrefsRegistry()
		registry.writeErr = errors.New("connection refused")
		rec := doPrefsRequest(t, registry, &stubTokenValidator{userID: prefsUserID},
			http.MethodPut, `{"viewerJoined":false}`, true)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})
}

// TestPrefsHandler_UserComesFromTheJWTNotTheBody — there is no user id in the
// path or the body, and there must never be one: this is a /users/me surface.
func TestPrefsHandler_UserComesFromTheJWTNotTheBody(t *testing.T) {
	registry := newFakePrefsRegistry()
	// A body attempting to name somebody else must be rejected outright by the
	// strict decode, not quietly ignored.
	rec := doPrefsRequest(t, registry, &stubTokenValidator{userID: prefsUserID},
		http.MethodPut, `{"userId":"cvictim001","rideLifecycle":false}`, true)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a body carrying a userId", rec.Code)
	}
	if registry.writeCalled {
		t.Error("the store was written for a request that named another user")
	}
}

// TestPrefsUpdate_IsEmpty covers the log-line predicate.
func TestPrefsUpdate_IsEmpty(t *testing.T) {
	if !(PrefsUpdate{}).isEmpty() {
		t.Error("an all-nil update should report empty")
	}
	if (PrefsUpdate{ViewerJoined: boolptr(false)}).isEmpty() {
		t.Error("an update carrying one category should not report empty")
	}
}
