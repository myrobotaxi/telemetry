package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// --- Test doubles ---

// fakeRideStore is a configurable in-memory RideRequestStore. Each field
// steers one method; the capture fields record what the handler passed.
type fakeRideStore struct {
	created      RideRequestData
	createErr    error
	createdInput RideRequestCreateInput

	getRec RideRequestData
	getErr error

	updated      RideRequestData
	updateErr    error
	updatedID    string
	updatedState string
	updatedFrom  []string
	updateCalls  int

	riderPage RideRequestListPage
	riderErr  error
	riderCall struct {
		id     string
		cursor RideRequestListCursor
		limit  int
	}
}

func (f *fakeRideStore) Create(_ context.Context, in RideRequestCreateInput) (RideRequestData, error) {
	f.createdInput = in
	if f.createErr != nil {
		return RideRequestData{}, f.createErr
	}
	// Echo server-derived ids onto the returned record when the fixture
	// didn't pin them, so happy-path assertions see the handler's values.
	rec := f.created
	if rec.RiderID == "" {
		rec.RiderID = in.RiderID
	}
	if rec.OwnerID == "" {
		rec.OwnerID = in.OwnerID
	}
	if rec.VehicleID == "" {
		rec.VehicleID = in.VehicleID
	}
	return rec, nil
}

func (f *fakeRideStore) GetByID(_ context.Context, _ string) (RideRequestData, error) {
	if f.getErr != nil {
		return RideRequestData{}, f.getErr
	}
	return f.getRec, nil
}

// UpdateStatusFrom mimics the guarded store transition: it enforces the
// allowed-from set against getRec's status (as the DB would against the
// live row), so a fixture whose status is outside `from` yields
// ErrRideStatusConflict even when the handler's pre-check was bypassed or
// raced. updateErr (when set) short-circuits everything.
func (f *fakeRideStore) UpdateStatusFrom(_ context.Context, id string, from []string, to string) (RideRequestData, error) {
	f.updateCalls++
	f.updatedID = id
	f.updatedState = to
	f.updatedFrom = from
	if f.updateErr != nil {
		return RideRequestData{}, f.updateErr
	}
	current := f.getRec.Status
	if f.updated.ID != "" {
		current = f.updated.Status
	}
	legal := false
	for _, s := range from {
		if s == current {
			legal = true
			break
		}
	}
	if !legal {
		return RideRequestData{}, fmt.Errorf("update ride request status: %w", ErrRideStatusConflict)
	}
	rec := f.updated
	if rec.ID == "" {
		rec = f.getRec
	}
	rec.Status = to
	return rec, nil
}

func (f *fakeRideStore) ListByRiderPage(_ context.Context, riderID string, cursor RideRequestListCursor, limit int) (RideRequestListPage, error) {
	f.riderCall.id = riderID
	f.riderCall.cursor = cursor
	f.riderCall.limit = limit
	if f.riderErr != nil {
		return RideRequestListPage{}, f.riderErr
	}
	return f.riderPage, nil
}

func (f *fakeRideStore) ListByOwnerPage(_ context.Context, _ string, _ *string, _ RideRequestListCursor, _ int) (RideRequestListPage, error) {
	return RideRequestListPage{}, nil
}

// fakeRidePublisher captures every published event so tests can assert the
// WS/dispatch seam fired with the right payload.
type fakeRidePublisher struct {
	events []events.Event
	err    error
}

func (f *fakeRidePublisher) Publish(_ context.Context, ev events.Event) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, ev)
	return nil
}

const (
	rideUserID   = "clrider1234567890abcdef"
	rideOwnerID  = "clrider1234567890abcdef" // v1: owner == rider (owner-only access)
	rideAuthOK   = "Bearer valid-token"
	rideID       = "crr0123456789abcdef0123456789abcd"
	rideVehicle  = fixtureSnapshotRowID
	rideOtherUsr = "clotheruser00000000000"
)

func newRideHandler(store RideRequestStore, reader *stubVehicleSnapshotReader, pub RideEventPublisher, userID string) *RideRequestHandler {
	return NewRideRequestHandler(
		&stubTokenValidator{userID: userID},
		reader,
		store,
		pub,
		discardLogger(),
	)
}

func rideMux(h *RideRequestHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/ride-requests", h.ServeCreate)
	mux.HandleFunc("GET /api/ride-requests", h.ServeList)
	mux.HandleFunc("GET /api/ride-requests/{id}", h.ServeGet)
	mux.HandleFunc("POST /api/ride-requests/{id}/cancel", h.ServeCancel)
	return mux
}

// fixtureRideData returns a persisted-shaped ride requested by rideUserID
// (the authenticated caller in these tests), with the given owner + status,
// on the pinned fixture vehicle.
func fixtureRideData(owner, status string) RideRequestData {
	addr := "221 Folsom St, San Francisco"
	now := time.Date(2026, 6, 15, 16, 12, 0, 0, time.UTC)
	return RideRequestData{
		ID:        rideID,
		RiderID:   rideUserID,
		OwnerID:   owner,
		VehicleID: rideVehicle,
		Pickup:    RidePlaceData{Latitude: 37.7955, Longitude: -122.3937, Label: "Home", Address: &addr},
		Dropoff:   RidePlaceData{Latitude: 37.7766, Longitude: -122.3946, Label: "Caltrain"},
		Status:    status,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// --- Create ---

func TestRideRequestHandler_Create(t *testing.T) {
	validBody := `{"vehicleId":"` + rideVehicle + `","pickup":{"lat":37.79,"lng":-122.39,"label":"Home"},"dropoff":{"lat":37.77,"lng":-122.39,"label":"Caltrain"}}`

	tests := []struct {
		name        string
		authHeader  string
		token       *stubTokenValidator
		reader      *stubVehicleSnapshotReader
		body        string
		wantStatus  int
		wantErrCode wserrors.ErrorCode
	}{
		{
			name:        "missing auth",
			authHeader:  "",
			token:       &stubTokenValidator{userID: rideUserID},
			reader:      &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)},
			body:        validBody,
			wantStatus:  http.StatusUnauthorized,
			wantErrCode: wserrors.ErrCodeAuthFailed,
		},
		{
			name:        "invalid token",
			authHeader:  rideAuthOK,
			token:       &stubTokenValidator{err: errors.New("expired")},
			reader:      &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)},
			body:        validBody,
			wantStatus:  http.StatusUnauthorized,
			wantErrCode: wserrors.ErrCodeAuthFailed,
		},
		{
			name:        "malformed json",
			authHeader:  rideAuthOK,
			token:       &stubTokenValidator{userID: rideUserID},
			reader:      &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)},
			body:        `{"vehicleId":`,
			wantStatus:  http.StatusBadRequest,
			wantErrCode: wserrors.ErrCodeInvalidRequest,
		},
		{
			name:        "unknown field rejected",
			authHeader:  rideAuthOK,
			token:       &stubTokenValidator{userID: rideUserID},
			reader:      &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)},
			body:        `{"vehicleId":"v","pickup":{"lat":1,"lng":1,"label":"a"},"dropoff":{"lat":1,"lng":1,"label":"b"},"bogus":1}`,
			wantStatus:  http.StatusBadRequest,
			wantErrCode: wserrors.ErrCodeInvalidRequest,
		},
		{
			name:        "missing vehicleId",
			authHeader:  rideAuthOK,
			token:       &stubTokenValidator{userID: rideUserID},
			reader:      &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)},
			body:        `{"pickup":{"lat":1,"lng":1,"label":"a"},"dropoff":{"lat":1,"lng":1,"label":"b"}}`,
			wantStatus:  http.StatusBadRequest,
			wantErrCode: wserrors.ErrCodeInvalidRequest,
		},
		{
			name:        "pickup out of range",
			authHeader:  rideAuthOK,
			token:       &stubTokenValidator{userID: rideUserID},
			reader:      &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)},
			body:        `{"vehicleId":"v","pickup":{"lat":99,"lng":1,"label":"a"},"dropoff":{"lat":1,"lng":1,"label":"b"}}`,
			wantStatus:  http.StatusBadRequest,
			wantErrCode: wserrors.ErrCodeInvalidRequest,
		},
		{
			name:        "bad scheduledFor",
			authHeader:  rideAuthOK,
			token:       &stubTokenValidator{userID: rideUserID},
			reader:      &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)},
			body:        `{"vehicleId":"v","pickup":{"lat":1,"lng":1,"label":"a"},"dropoff":{"lat":1,"lng":1,"label":"b"},"scheduledFor":"soon"}`,
			wantStatus:  http.StatusBadRequest,
			wantErrCode: wserrors.ErrCodeInvalidRequest,
		},
		{
			name:        "vehicle not found",
			authHeader:  rideAuthOK,
			token:       &stubTokenValidator{userID: rideUserID},
			reader:      &stubVehicleSnapshotReader{err: fmtNotFound()},
			body:        validBody,
			wantStatus:  http.StatusNotFound,
			wantErrCode: wserrors.ErrCodeNotFound,
		},
		{
			name:        "vehicle access denied (not owner)",
			authHeader:  rideAuthOK,
			token:       &stubTokenValidator{userID: rideUserID},
			reader:      &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideOtherUsr)},
			body:        validBody,
			wantStatus:  http.StatusForbidden,
			wantErrCode: wserrors.ErrCodeVehicleNotOwned,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{}
			h := NewRideRequestHandler(tt.token, tt.reader, store, &fakeRidePublisher{}, discardLogger())
			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", tt.body, tt.authHeader)
			assertErrEnvelope(t, rec, tt.wantStatus, tt.wantErrCode)
		})
	}
}

func TestRideRequestHandler_Create_HappyPath(t *testing.T) {
	store := &fakeRideStore{created: fixtureRideData(rideUserID, rideStatusRequested)}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)}, pub, rideUserID)

	body := `{"vehicleId":"` + rideVehicle + `","pickup":{"lat":37.79,"lng":-122.39,"label":"Home"},"dropoff":{"lat":37.77,"lng":-122.39,"label":"Caltrain"},"scheduledFor":"2026-06-18T16:00:00Z"}`
	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", body, rideAuthOK)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201. body=%s", rec.Code, rec.Body.String())
	}
	// Server derives rider (JWT sub) + owner (vehicle owner); never trusts client.
	if store.createdInput.RiderID != rideUserID || store.createdInput.OwnerID != rideUserID {
		t.Errorf("derived ids: rider=%q owner=%q", store.createdInput.RiderID, store.createdInput.OwnerID)
	}
	if store.createdInput.ScheduledFor == nil {
		t.Error("scheduledFor not parsed into input")
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["id"] != rideID || got["status"] != rideStatusRequested {
		t.Errorf("body id/status: %v / %v", got["id"], got["status"])
	}
	// WS created event published to the parties.
	if len(pub.events) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(pub.events))
	}
	ev, ok := pub.events[0].Payload.(events.RideRequestCreatedEvent)
	if !ok {
		t.Fatalf("expected RideRequestCreatedEvent, got %T", pub.events[0].Payload)
	}
	if ev.RideRequestID != rideID || ev.RiderID != rideUserID || ev.OwnerID != rideUserID {
		t.Errorf("event fields: %+v", ev)
	}
}

// --- Cancel ---

func TestRideRequestHandler_Cancel(t *testing.T) {
	tests := []struct {
		name        string
		caller      string
		rec         RideRequestData
		getErr      error
		wantStatus  int
		wantErrCode wserrors.ErrorCode
		wantEvent   bool
	}{
		{name: "rider cancels requested", caller: rideUserID, rec: fixtureRideData(rideUserID, rideStatusRequested), wantStatus: http.StatusOK, wantEvent: true},
		{name: "rider cancels accepted", caller: rideUserID, rec: fixtureRideData(rideUserID, rideStatusAccepted), wantStatus: http.StatusOK, wantEvent: true},
		{name: "conflict from declined", caller: rideUserID, rec: fixtureRideData(rideUserID, rideStatusDeclined), wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict},
		{name: "conflict from completed", caller: rideUserID, rec: fixtureRideData(rideUserID, rideStatusCompleted), wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict},
		{name: "conflict from cancelled", caller: rideUserID, rec: fixtureRideData(rideUserID, rideStatusCancelled), wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict},
		{name: "conflict from enroute", caller: rideUserID, rec: fixtureRideData(rideUserID, rideStatusEnroute), wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict},
		{name: "owner cannot cancel (rider-only)", caller: rideOwnerID + "X", rec: fixtureRideData(rideOwnerID+"X", rideStatusRequested), wantStatus: http.StatusForbidden, wantErrCode: wserrors.ErrCodePermissionDenied},
		{name: "non-party gets 404", caller: rideOtherUsr, rec: fixtureRideData(rideUserID, rideStatusRequested), wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound},
		{name: "unknown id 404", caller: rideUserID, getErr: fmtNotFound(), wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{getRec: tt.rec, getErr: tt.getErr}
			pub := &fakeRidePublisher{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{}, pub, tt.caller)
			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/cancel", "", rideAuthOK)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d want %d. body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusOK {
				if store.updatedState != rideStatusCancelled {
					t.Errorf("expected UpdateStatus(cancelled), got %q", store.updatedState)
				}
				var got map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &got)
				if got["status"] != rideStatusCancelled {
					t.Errorf("body status: %v", got["status"])
				}
			} else {
				assertErrEnvelope(t, rec, tt.wantStatus, tt.wantErrCode)
			}
			gotEvent := len(pub.events) == 1
			if gotEvent != tt.wantEvent {
				t.Errorf("event published=%v want=%v", gotEvent, tt.wantEvent)
			}
			if tt.wantEvent {
				if _, ok := pub.events[0].Payload.(events.RideStatusChangedEvent); !ok {
					t.Errorf("expected RideStatusChangedEvent, got %T", pub.events[0].Payload)
				}
			}
		})
	}
}

// TestRideRequestHandler_Cancel_GuardWinsRace simulates the check-then-write
// race: the pre-check read sees `requested` (legal), but by write time the
// row moved to a state outside the allowed-from set (e.g. the owner declined
// concurrently). The guarded UpdateStatusFrom must refuse — the loser gets
// 409 conflict and publishes NO ride_status_changed event.
func TestRideRequestHandler_Cancel_GuardWinsRace(t *testing.T) {
	readRec := fixtureRideData(rideUserID, rideStatusRequested) // pre-check passes
	writeRec := fixtureRideData(rideUserID, rideStatusDeclined) // guard sees the concurrent decline
	store := &fakeRideStore{getRec: readRec, updated: writeRec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{}, pub, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/cancel", "", rideAuthOK)

	assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeConflict)
	if store.updateCalls != 1 {
		t.Errorf("expected exactly one guarded write attempt, got %d", store.updateCalls)
	}
	if len(store.updatedFrom) != 2 || store.updatedFrom[0] != rideStatusRequested || store.updatedFrom[1] != rideStatusAccepted {
		t.Errorf("guard allowed-from set: %v", store.updatedFrom)
	}
	if len(pub.events) != 0 {
		t.Errorf("losing transition must publish no events, got %d", len(pub.events))
	}
}

// --- Get ---

func TestRideRequestHandler_Get(t *testing.T) {
	tests := []struct {
		name       string
		caller     string
		rec        RideRequestData
		getErr     error
		wantStatus int
	}{
		{name: "rider party 200", caller: rideUserID, rec: fixtureRideData(rideOtherUsr, rideStatusRequested), wantStatus: http.StatusOK},
		{name: "owner party 200", caller: rideOtherUsr, rec: fixtureRideData(rideOtherUsr, rideStatusRequested), wantStatus: http.StatusOK},
		{name: "non-party 404", caller: "clstranger00000000000", rec: fixtureRideData(rideOtherUsr, rideStatusRequested), wantStatus: http.StatusNotFound},
		{name: "unknown 404", caller: rideUserID, getErr: fmtNotFound(), wantStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{getRec: tt.rec, getErr: tt.getErr}
			h := newRideHandler(store, &stubVehicleSnapshotReader{}, &fakeRidePublisher{}, tt.caller)
			rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/"+rideID, "", rideAuthOK)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status got %d want %d. body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusOK {
				var got map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &got)
				if got["id"] != rideID {
					t.Errorf("body id: %v", got["id"])
				}
			}
		})
	}
}

// --- List ---

func TestRideRequestHandler_List(t *testing.T) {
	t.Run("empty list yields items [] not null", func(t *testing.T) {
		store := &fakeRideStore{riderPage: RideRequestListPage{Items: nil, HasMore: false}}
		h := newRideHandler(store, &stubVehicleSnapshotReader{}, &fakeRidePublisher{}, rideUserID)
		rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests", "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"items":[]`) {
			t.Errorf("expected items:[], got %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"nextCursor":null`) {
			t.Errorf("expected nextCursor:null, got %s", rec.Body.String())
		}
	})

	t.Run("hasMore emits a decodable nextCursor", func(t *testing.T) {
		last := fixtureRideData(rideUserID, rideStatusRequested)
		store := &fakeRideStore{riderPage: RideRequestListPage{Items: []RideRequestData{last}, HasMore: true}}
		h := newRideHandler(store, &stubVehicleSnapshotReader{}, &fakeRidePublisher{}, rideUserID)
		rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests", "", rideAuthOK)
		var env struct {
			Items      []map[string]any `json:"items"`
			NextCursor *string          `json:"nextCursor"`
			HasMore    bool             `json:"hasMore"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !env.HasMore || env.NextCursor == nil {
			t.Fatalf("expected hasMore + nextCursor, got %+v", env)
		}
		cur, err := decodeRideCursor(*env.NextCursor)
		if err != nil {
			t.Fatalf("nextCursor not decodable: %v", err)
		}
		if cur.ID != last.ID || !cur.CreatedAt.Equal(last.CreatedAt) {
			t.Errorf("cursor anchor mismatch: %+v", cur)
		}
	})

	t.Run("scopes to the authenticated rider", func(t *testing.T) {
		store := &fakeRideStore{}
		h := newRideHandler(store, &stubVehicleSnapshotReader{}, &fakeRidePublisher{}, rideUserID)
		_ = doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests?limit=5", "", rideAuthOK)
		if store.riderCall.id != rideUserID || store.riderCall.limit != 5 {
			t.Errorf("list call: id=%q limit=%d", store.riderCall.id, store.riderCall.limit)
		}
	})

	badInputs := []struct {
		name string
		q    string
	}{
		{"limit zero", "?limit=0"},
		{"limit over max", "?limit=101"},
		{"limit non-numeric", "?limit=abc"},
		{"malformed cursor", "?cursor=not-base64!!"},
	}
	for _, tt := range badInputs {
		t.Run("400 on "+tt.name, func(t *testing.T) {
			store := &fakeRideStore{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{}, &fakeRidePublisher{}, rideUserID)
			rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests"+tt.q, "", rideAuthOK)
			assertErrEnvelope(t, rec, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest)
		})
	}
}

func TestRideCursor_RoundTrip(t *testing.T) {
	ts := time.Date(2026, 6, 15, 16, 12, 0, 123456789, time.UTC)
	enc := encodeRideCursor(ts, "crr-abc")
	got, err := decodeRideCursor(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != "crr-abc" || !got.CreatedAt.Equal(ts) {
		t.Errorf("round trip mismatch: %+v (want %v)", got, ts)
	}
	for _, bad := range []string{"", "!!!", "eyJmb28iOjF9"} { // last is valid base64 JSON but wrong fields
		if _, err := decodeRideCursor(bad); err == nil {
			t.Errorf("expected error decoding %q", bad)
		}
	}
}

// --- helpers ---

func fmtNotFound() error {
	return fmt.Errorf("get: %w", sdk.ErrNotFound)
}

func doRequest(t *testing.T, mux *http.ServeMux, method, target, body, auth string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequestWithContext(context.Background(), method, target, nil)
	}
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func assertErrEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode wserrors.ErrorCode) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status: got %d want %d. body=%s", rec.Code, wantStatus, rec.Body.String())
	}
	if wantCode == "" {
		return
	}
	var env wserrors.ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Code != wantCode {
		t.Errorf("error.code: got %q want %q", env.Error.Code, wantCode)
	}
}
