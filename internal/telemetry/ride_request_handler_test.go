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

	// Active-instant guard (MYR-230). activeErr short-circuits; otherwise a
	// zero-value activeRec (empty ID) is reported as "no open instant ride"
	// (sdk.ErrNotFound) so create falls through, and a populated activeRec
	// stands in for an existing open ride.
	activeRec   RideRequestData
	activeErr   error
	activeCalls int
	// activeAfterCreateOnly makes the guard MISS on the pre-check and only
	// surface activeRec after Create ran — used to drive the DB race backstop
	// (unique-index rejection re-read) deterministically.
	activeAfterCreateOnly bool
	createCalled          bool

	updated      RideRequestData
	updateErr    error
	updatedID    string
	updatedState string
	updatedFrom  []string
	updateCalls  int
	cancelledBy  string
	// dispatchGuardedCalls counts the writes that went through the MYR-376
	// DORMANCY-guarded variant, so a test can pin that the pickup path uses it
	// (and that nothing else does).
	dispatchGuardedCalls int
	// windowGuardedCalls counts the writes that went through the MYR-383
	// BOOKING-LOCKED variant, so a test can pin that ACCEPT uses it (and that
	// nothing else does). windowConflictAt / windowConflictActive steer that
	// write's window probe: the first stands in for a conflicting RESERVATION
	// and names its instant, the second for an ACTIVE INSTANT ride, which has
	// no instant to name.
	windowGuardedCalls   int
	windowConflictAt     *time.Time
	windowConflictActive bool

	riderPage RideRequestListPage
	riderErr  error
	riderCall struct {
		id     string
		cursor RideRequestListCursor
		limit  int
	}

	// MYR-385 booked-windows read. windowsCall records the range and the
	// caller the handler passed down, which is how the §7.22 tests pin the
	// default range and that `own` is resolved against the JWT subject rather
	// than against anything client-supplied.
	windowsResult []BookedWindowData
	windowsErr    error
	windowsCalls  int
	windowsCall   struct {
		vehicleID string
		callerID  string
		from      time.Time
		to        time.Time
	}

	ownerPage RideRequestListPage
	ownerErr  error
	ownerCall struct {
		id     string
		status *string
		cursor RideRequestListCursor
		limit  int
	}

	// Upcoming-reservations slice of the owner feed (MYR-360).
	// upcomingPage/upcomingErr steer ListUpcomingByOwnerVehiclePage;
	// upcomingCall records what the handler passed so a test can pin the
	// owner scoping (JWT sub, never a client value) and the ASCENDING
	// (scheduledFor, id) cursor. The anchor is captured as plain fields so
	// the assertions read the same whichever cursor type carries it.
	upcomingPage  RideRequestListPage
	upcomingErr   error
	upcomingCalls int
	upcomingCall  struct {
		ownerID            string
		vehicleID          string
		cursorScheduledFor time.Time
		cursorID           string
		limit              int
	}

	// Active-rides slice of the owner feed (MYR-400). Same shape as the
	// upcoming fixture above, but the anchor is the DESCENDING (createdAt, id)
	// cursor — this slice keeps the feed's default ordering. Named
	// activeRides* to stay clear of the MYR-230 active-INSTANT-guard fields.
	activeRidesPage  RideRequestListPage
	activeRidesErr   error
	activeRidesCalls int
	activeRidesCall  struct {
		ownerID         string
		vehicleID       string
		cursorCreatedAt time.Time
		cursorID        string
		limit           int
	}
}

func (f *fakeRideStore) Create(_ context.Context, in RideRequestCreateInput) (RideRequestData, error) {
	f.createdInput = in
	f.createCalled = true
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

func (f *fakeRideStore) GetActiveInstantByRider(_ context.Context, _ string) (RideRequestData, error) {
	f.activeCalls++
	if f.activeErr != nil {
		return RideRequestData{}, f.activeErr
	}
	if f.activeAfterCreateOnly && !f.createCalled {
		return RideRequestData{}, fmtNotFound()
	}
	if f.activeRec.ID == "" {
		return RideRequestData{}, fmtNotFound()
	}
	return f.activeRec, nil
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

// UpdateStatusFromCancelled mimics the MYR-522 initiator-stamping cancel
// write: the same guarded transition with to fixed to "cancelled", recording
// the party stamp the way the single UPDATE writes cancelled_by. The returned
// record carries the stamp so handler tests can assert the wire projection.
func (f *fakeRideStore) UpdateStatusFromCancelled(ctx context.Context, id string, from []string, by string) (RideRequestData, error) {
	f.cancelledBy = by
	rec, err := f.UpdateStatusFrom(ctx, id, from, "cancelled")
	if err != nil {
		return RideRequestData{}, err
	}
	stamp := by
	rec.CancelledBy = &stamp
	return rec, nil
}

// UpdateStatusFromDispatched models the MYR-376 dormancy-guarded write the way
// the single UPDATE behaves: the reservation predicate is evaluated as part of
// the SAME write as the allowed-from guard, against the CURRENT row — never as
// a pre-check the caller could have raced past. A dormant reservation matches no
// row and yields ErrRideReservationDormant; everything else defers to the plain
// guard, so an instant ride is unaffected whatever its dispatch outcome.
//
// Dormancy here is the three-armed SQL predicate, verbatim: a reservation is
// dormant only while it is BOTH undispatched AND not yet due. `scheduledFor` in
// the past lifts it on its own (§7.8 manual proceed), exactly as
// `scheduled_for <= NOW()` does in the store. The store's own docker suite is
// what pins the SQL; this keeps the fake from disagreeing with it.
func (f *fakeRideStore) UpdateStatusFromDispatched(ctx context.Context, id string, from []string, to string) (RideRequestData, error) {
	f.dispatchGuardedCalls++
	current := f.getRec
	if f.updated.ID != "" {
		current = f.updated
	}
	if f.updateErr == nil && current.ScheduledFor != nil &&
		current.ScheduledFor.After(time.Now()) &&
		(current.DispatchStatus == nil || *current.DispatchStatus != "sent") {
		// Count the attempt exactly as the real write would: the refusal comes
		// from the write, not from a check that skipped it.
		f.updateCalls++
		f.updatedID = id
		f.updatedState = to
		f.updatedFrom = from
		return RideRequestData{}, fmt.Errorf("update ride request status: %w", ErrRideReservationDormant)
	}
	return f.UpdateStatusFrom(ctx, id, from, to)
}

// UpdateStatusFromUnconflicted models the MYR-383 booking-locked accept write
// the way the real transaction behaves: the window probe runs as part of the
// SAME write as the allowed-from guard, against the CURRENT row — never as a
// pre-check the caller could have raced past. `windowConflictAt` (when the
// fixture sets it) stands in for a probe that found the vehicle already
// promised, and the ride never transitions; everything else defers to the plain
// guard, so an INSTANT accept is unaffected whatever the fixture holds.
//
// A nil-instant conflict is spelled with `windowConflictActive`, the
// active-instant arm, which the store reports with a NULL scheduled_for. The
// store's own docker suite is what pins the SQL; this keeps the fake from
// disagreeing with it.
func (f *fakeRideStore) UpdateStatusFromUnconflicted(ctx context.Context, id string, from []string, to string) (RideRequestData, error) {
	f.windowGuardedCalls++
	current := f.getRec
	if f.updated.ID != "" {
		current = f.updated
	}
	if f.updateErr == nil && current.ScheduledFor != nil &&
		(f.windowConflictAt != nil || f.windowConflictActive) {
		// Count the attempt exactly as the real write would: the refusal comes
		// from the write, not from a check that skipped it.
		f.updateCalls++
		f.updatedID = id
		f.updatedState = to
		f.updatedFrom = from
		return RideRequestData{}, fmt.Errorf("update ride request status: %w",
			&RideWindowConflictError{ConflictAt: f.windowConflictAt})
	}
	return f.UpdateStatusFrom(ctx, id, from, to)
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

func (f *fakeRideStore) ListByOwnerPage(_ context.Context, ownerID string, status *string, cursor RideRequestListCursor, limit int) (RideRequestListPage, error) {
	f.ownerCall.id = ownerID
	f.ownerCall.status = status
	f.ownerCall.cursor = cursor
	f.ownerCall.limit = limit
	if f.ownerErr != nil {
		return RideRequestListPage{}, f.ownerErr
	}
	return f.ownerPage, nil
}

func (f *fakeRideStore) ListUpcomingByOwnerVehiclePage(_ context.Context, ownerID, vehicleID string, cursor RideRequestUpcomingCursor, limit int) (RideRequestListPage, error) {
	f.upcomingCalls++
	f.upcomingCall.ownerID = ownerID
	f.upcomingCall.vehicleID = vehicleID
	f.upcomingCall.cursorScheduledFor = cursor.ScheduledFor
	f.upcomingCall.cursorID = cursor.ID
	f.upcomingCall.limit = limit
	if f.upcomingErr != nil {
		return RideRequestListPage{}, f.upcomingErr
	}
	return f.upcomingPage, nil
}

func (f *fakeRideStore) ListActiveByOwnerVehiclePage(_ context.Context, ownerID, vehicleID string, cursor RideRequestListCursor, limit int) (RideRequestListPage, error) {
	f.activeRidesCalls++
	f.activeRidesCall.ownerID = ownerID
	f.activeRidesCall.vehicleID = vehicleID
	f.activeRidesCall.cursorCreatedAt = cursor.CreatedAt
	f.activeRidesCall.cursorID = cursor.ID
	f.activeRidesCall.limit = limit
	if f.activeRidesErr != nil {
		return RideRequestListPage{}, f.activeRidesErr
	}
	return f.activeRidesPage, nil
}

func (f *fakeRideStore) ListBookedWindows(_ context.Context, vehicleID, callerID string, from, to time.Time) ([]BookedWindowData, error) {
	f.windowsCalls++
	f.windowsCall.vehicleID = vehicleID
	f.windowsCall.callerID = callerID
	f.windowsCall.from = from
	f.windowsCall.to = to
	if f.windowsErr != nil {
		return nil, f.windowsErr
	}
	return f.windowsResult, nil
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
	mux.HandleFunc("POST /api/ride-requests/{id}/picked-up", h.ServePickedUp)
	mux.HandleFunc("POST /api/ride-requests/{id}/start", h.ServeStart)
	mux.HandleFunc("POST /api/ride-requests/{id}/dropped-off", h.ServeDroppedOff)
	// Owner surface (MYR-175) — mirrors the production wiring, including the
	// literal-vs-wildcard coexistence of /incoming and /{id}.
	mux.HandleFunc("GET /api/ride-requests/incoming", h.ServeIncoming)
	mux.HandleFunc("POST /api/ride-requests/{id}/accept", h.ServeAccept)
	mux.HandleFunc("POST /api/ride-requests/{id}/decline", h.ServeDecline)
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

// --- One active ride per rider (MYR-230) ---

const rideActiveInstantBody = `{"vehicleId":"` + rideVehicle + `","pickup":{"lat":37.79,"lng":-122.39,"label":"Home"},"dropoff":{"lat":37.77,"lng":-122.39,"label":"Caltrain"}}`

// decodeRideActive decodes a 409 ride_active body and asserts the typed code.
func decodeRideActive(t *testing.T, rec *httptest.ResponseRecorder) rideActiveErrorResponse {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409. body=%s", rec.Code, rec.Body.String())
	}
	var body rideActiveErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode ride_active body: %v", err)
	}
	if body.Error.Code != wserrors.ErrCodeRideActive {
		t.Fatalf("error.code: got %q want %q", body.Error.Code, wserrors.ErrCodeRideActive)
	}
	return body
}

// TestRideRequestHandler_Create_RideActive_PreCheck: an instant create while
// an open instant ride already exists is refused 409 ride_active, carries the
// existing ride under activeRideRequest for the client to adopt, and never
// reaches Create (fast path).
func TestRideRequestHandler_Create_RideActive_PreCheck(t *testing.T) {
	existing := fixtureRideData(rideUserID, rideStatusAccepted)
	store := &fakeRideStore{activeRec: existing}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)}, pub, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", rideActiveInstantBody, rideAuthOK)

	body := decodeRideActive(t, rec)
	if body.ActiveRideRequest.ID != existing.ID {
		t.Errorf("activeRideRequest.id: got %q want %q", body.ActiveRideRequest.ID, existing.ID)
	}
	if body.ActiveRideRequest.Status != rideStatusAccepted {
		t.Errorf("activeRideRequest.status: got %q", body.ActiveRideRequest.Status)
	}
	if store.createdInput.VehicleID != "" {
		t.Error("Create must not run when the pre-check already found an open ride")
	}
	if len(pub.events) != 0 {
		t.Errorf("no event should be published on a refused create, got %d", len(pub.events))
	}
}

// TestRideRequestHandler_Create_RideActive_RaceBackstop: the pre-check MISSES
// (no open ride yet) but a concurrent instant create wins the unique-index
// race, so Create returns ErrRideActive. The handler must re-read the winner
// and return the same 409 ride_active body — the DB is the authoritative
// guard even when the pre-check passed.
func TestRideRequestHandler_Create_RideActive_RaceBackstop(t *testing.T) {
	winner := fixtureRideData(rideUserID, rideStatusRequested)
	store := &fakeRideStore{
		activeAfterCreateOnly: true,
		activeRec:             winner,
		createErr:             fmt.Errorf("create ride request: %w", ErrRideActive),
	}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)}, &fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", rideActiveInstantBody, rideAuthOK)

	body := decodeRideActive(t, rec)
	if body.ActiveRideRequest.ID != winner.ID {
		t.Errorf("activeRideRequest.id: got %q want %q", body.ActiveRideRequest.ID, winner.ID)
	}
	if !store.createCalled {
		t.Error("Create must be attempted after the pre-check missed")
	}
	if store.activeCalls != 2 {
		t.Errorf("expected pre-check + backstop re-read = 2 active lookups, got %d", store.activeCalls)
	}
}

// TestRideRequestHandler_Create_ScheduledExempt: a scheduled request is never
// blocked by the active-instant guard — the pre-check is skipped entirely and
// the create proceeds even when an open instant ride exists.
func TestRideRequestHandler_Create_ScheduledExempt(t *testing.T) {
	store := &fakeRideStore{
		activeRec: fixtureRideData(rideUserID, rideStatusAccepted), // an open instant ride exists
		created:   fixtureRideData(rideUserID, rideStatusRequested),
	}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)}, &fakeRidePublisher{}, rideUserID)

	body := `{"vehicleId":"` + rideVehicle + `","pickup":{"lat":37.79,"lng":-122.39,"label":"Home"},"dropoff":{"lat":37.77,"lng":-122.39,"label":"Caltrain"},"scheduledFor":"2026-06-18T16:00:00Z"}`
	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", body, rideAuthOK)

	if rec.Code != http.StatusCreated {
		t.Fatalf("scheduled create must be allowed: got %d. body=%s", rec.Code, rec.Body.String())
	}
	if store.activeCalls != 0 {
		t.Errorf("scheduled create must skip the active-instant guard, got %d lookups", store.activeCalls)
	}
}

// TestRideRequestHandler_Create_RideActive_PreCheckStoreError: a store failure
// on the guard pre-check surfaces as 500, not a false ride_active.
func TestRideRequestHandler_Create_RideActive_PreCheckStoreError(t *testing.T) {
	store := &fakeRideStore{activeErr: errors.New("db down")}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)}, &fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", rideActiveInstantBody, rideAuthOK)

	assertErrEnvelope(t, rec, http.StatusInternalServerError, wserrors.ErrCodeInternalError)
	if store.createCalled {
		t.Error("Create must not run when the pre-check errored")
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
		// MYR-522: the owner's cancel exists now, but a REQUESTED ride keeps
		// decline as the owner's one exit — a 409 naming that door, not a 403.
		{name: "owner cancel of a requested ride points at decline", caller: rideOwnerID + "X", rec: fixtureRideData(rideOwnerID+"X", rideStatusRequested), wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict},
		{name: "non-party gets 404", caller: rideOtherUsr, rec: fixtureRideData(rideUserID, rideStatusRequested), wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound},
		{name: "unknown id 404", caller: rideUserID, getErr: fmtNotFound(), wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{getRec: tt.rec, getErr: tt.getErr}
			pub := &fakeRidePublisher{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, tt.caller)
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
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

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
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, tt.caller)
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
		h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideUserID)
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
		h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideUserID)
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
		h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideUserID)
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
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideUserID)
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
