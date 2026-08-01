package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// MYR-172 §7.21 — the rider's Live Activity push-token endpoints.

// fakeActivityRegistry is an in-memory LiveActivityRegistry.
type fakeActivityRegistry struct {
	mu sync.Mutex

	// tokens is keyed "<rideID>|<userID>" so a test can prove the handler
	// scoped the write to the ride AND the caller, not merely to one of them.
	tokens   map[string]string
	sandbox  map[string]bool
	endedFor []string

	registerErr error
	endErr      error
	endResult   bool
}

func newFakeActivityRegistry() *fakeActivityRegistry {
	return &fakeActivityRegistry{
		tokens:    map[string]string{},
		sandbox:   map[string]bool{},
		endResult: true,
	}
}

func activityKey(rideID, userID string) string { return rideID + "|" + userID }

func (f *fakeActivityRegistry) RegisterActivity(_ context.Context, rideID, userID, token string, sandbox bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registerErr != nil {
		return f.registerErr
	}
	f.tokens[activityKey(rideID, userID)] = token
	f.sandbox[activityKey(rideID, userID)] = sandbox
	return nil
}

func (f *fakeActivityRegistry) EndActivity(_ context.Context, rideID, userID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.endErr != nil {
		return false, f.endErr
	}
	f.endedFor = append(f.endedFor, activityKey(rideID, userID))
	return f.endResult, nil
}

// riderToken reads the token stored against the fixture ride for the fixture
// rider. The map is keyed on the PAIR so that a handler which wrote against
// the right ride but the wrong party — or vice versa — reads back empty here,
// which is the whole point of the assertion.
func (f *fakeActivityRegistry) riderToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[activityKey(rideID, rideUserID)]
}

// activityMux wires just the two §7.21 routes, mirroring production.
func activityMux(h *RideRequestHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/ride-requests/{id}/activity-token", h.ServeRegisterActivityToken)
	mux.HandleFunc("DELETE /api/ride-requests/{id}/activity-token", h.ServeEndActivityToken)
	return mux
}

// newActivityHandler builds the handler with the registry wired and `caller`
// as the authenticated user.
func newActivityHandler(store RideRequestStore, registry LiveActivityRegistry, caller string) *RideRequestHandler {
	return NewRideRequestHandler(
		&stubTokenValidator{userID: caller},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)},
		store,
		&fakeRidePublisher{},
		discardLogger(),
		WithLiveActivityRegistry(registry),
	)
}

func activityRequest(method, body string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), method,
		"/api/ride-requests/"+rideID+"/activity-token", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	return req
}

// TestActivityToken_AuthMatrix is the full authorization matrix.
//
// The 404-vs-403 split is the load-bearing part. A stranger gets 404, NOT 403,
// so the endpoint never confirms that a ride id exists to somebody with no
// relation to it; only a genuine party who happens to be the OWNER rather than
// the rider reaches a 403.
func TestActivityToken_AuthMatrix(t *testing.T) {
	const otherOwner = "clowner999999999999xyz"
	const stranger = "clstrangr00000000000zz"

	tests := []struct {
		name       string
		caller     string
		rec        RideRequestData
		getErr     error
		wantStatus int
	}{
		{
			name:       "the rider may register",
			caller:     rideUserID,
			rec:        fixtureRideData(otherOwner, rideStatusAccepted),
			wantStatus: http.StatusOK,
		},
		{
			name:   "the owner is a party but Live Activities are rider-only",
			caller: otherOwner,
			rec:    fixtureRideData(otherOwner, rideStatusAccepted),
			// 403, not 404: the owner legitimately knows this ride exists, so
			// hiding it would be a lie rather than a protection.
			wantStatus: http.StatusForbidden,
		},
		{
			name:   "a stranger gets 404, never 403",
			caller: stranger,
			rec:    fixtureRideData(otherOwner, rideStatusAccepted),
			// Indistinguishable from a ride that does not exist, so the
			// endpoint is not an oracle for ride ids.
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "an unknown ride is 404",
			caller:     rideUserID,
			getErr:     sdk.ErrNotFound,
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := newFakeActivityRegistry()
			store := &fakeRideStore{getRec: tt.rec, getErr: tt.getErr}
			h := newActivityHandler(store, registry, tt.caller)

			rec := httptest.NewRecorder()
			activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, `{"activityToken":"abc123","sandbox":true}`))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}

			// Nothing but a successful call may have written a row.
			wrote := registry.riderToken() != ""
			if wrote != (tt.wantStatus == http.StatusOK) {
				t.Errorf("registry write = %v, want %v", wrote, tt.wantStatus == http.StatusOK)
			}
		})
	}
}

// TestActivityToken_DeleteAuthMatrix repeats the matrix for the end endpoint.
// The two must not drift: an end endpoint with laxer auth would let an owner
// silence the rider's Activity.
func TestActivityToken_DeleteAuthMatrix(t *testing.T) {
	const otherOwner = "clowner999999999999xyz"
	const stranger = "clstrangr00000000000zz"

	tests := []struct {
		name       string
		caller     string
		wantStatus int
	}{
		{"the rider may end", rideUserID, http.StatusOK},
		{"the owner may not", otherOwner, http.StatusForbidden},
		{"a stranger gets 404", stranger, http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := newFakeActivityRegistry()
			store := &fakeRideStore{getRec: fixtureRideData(otherOwner, rideStatusAccepted)}
			h := newActivityHandler(store, registry, tt.caller)

			rec := httptest.NewRecorder()
			activityMux(h).ServeHTTP(rec, activityRequest(http.MethodDelete, ""))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestActivityToken_RegisterScopesTheWriteToTheRider proves the row is written
// against the RIDER from the ride record, never a client-supplied identity.
func TestActivityToken_RegisterScopesTheWriteToTheRider(t *testing.T) {
	registry := newFakeActivityRegistry()
	store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", rideStatusAccepted)}
	h := newActivityHandler(store, registry, rideUserID)

	rec := httptest.NewRecorder()
	activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, `{"activityToken":"deadbeef01","sandbox":true}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := registry.riderToken(); got != "deadbeef01" {
		t.Errorf("token stored against (ride, rider) = %q, want deadbeef01", got)
	}

	var body activityTokenResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.Registered || !body.Sandbox {
		t.Errorf("response = %+v, want registered and sandbox true", body)
	}
}

// TestActivityToken_ResponseNeverEchoesTheToken pins the P1 rule. The token is
// a capability; echoing it would put it in every client log and proxy trace for
// no benefit, since the caller already knows what it sent.
func TestActivityToken_ResponseNeverEchoesTheToken(t *testing.T) {
	registry := newFakeActivityRegistry()
	store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", rideStatusAccepted)}
	h := newActivityHandler(store, registry, rideUserID)

	secret := strings.Repeat("dec0de", 4)
	rec := httptest.NewRecorder()
	activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, `{"activityToken":"`+secret+`"}`))

	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("the response echoed the P1 activity token: %s", rec.Body.String())
	}
}

// TestActivityToken_Rotation proves a re-post REPLACES the token rather than
// erroring or accumulating. ActivityKit rotates the token mid-Activity, so this
// is the ordinary path and not an edge case.
func TestActivityToken_Rotation(t *testing.T) {
	registry := newFakeActivityRegistry()
	store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", rideStatusAccepted)}
	h := newActivityHandler(store, registry, rideUserID)
	mux := activityMux(h)

	for _, token := range []string{"aaa111", "bbb222"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, activityRequest(http.MethodPost, `{"activityToken":"`+token+`"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("registering %s: status = %d, want 200", token, rec.Code)
		}
	}

	if got := registry.riderToken(); got != "bbb222" {
		t.Errorf("stored token = %q, want the rotated value bbb222", got)
	}
}

// TestActivityToken_TerminalRideIsConflict — an Activity registered against a
// finished ride would never be pushed to, because the terminal `event: "end"`
// has already fired. The 409 tells the client to end it locally now.
func TestActivityToken_TerminalRideIsConflict(t *testing.T) {
	for _, status := range []string{rideStatusCompleted, rideStatusDeclined, rideStatusCancelled} {
		t.Run(status, func(t *testing.T) {
			registry := newFakeActivityRegistry()
			store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", status)}
			h := newActivityHandler(store, registry, rideUserID)

			rec := httptest.NewRecorder()
			activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, `{"activityToken":"cafe01"}`))

			if rec.Code != http.StatusConflict {
				t.Errorf("status = %d for terminal ride %q, want 409", rec.Code, status)
			}
			if registry.riderToken() != "" {
				t.Error("a token was stored against a ride that had already ended")
			}
		})
	}
}

// TestActivityToken_NonTerminalRidesAccepted is the mirror of the above, and
// guards the statuses most easily mistaken for endings.
//
// The `requested` row became load-bearing with MYR-398's v3 card, which starts
// the Activity at REQUEST rather than at accept. It passed before that change
// and passes after it — the registration guard refuses exactly two things, a
// terminal status and a lapsed reservation, and `requested` was never either —
// so this is the test that says the Dispatch phase needed no widening HERE, as
// opposed to in the ticker's active-leg query, where it did.
func TestActivityToken_NonTerminalRidesAccepted(t *testing.T) {
	for _, status := range []string{rideStatusRequested, rideStatusAccepted, rideStatusArrived, rideStatusEnroute} {
		t.Run(status, func(t *testing.T) {
			registry := newFakeActivityRegistry()
			store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", status)}
			h := newActivityHandler(store, registry, rideUserID)

			rec := httptest.NewRecorder()
			activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, `{"activityToken":"cafe01"}`))

			if rec.Code != http.StatusOK {
				t.Errorf("status = %d for live ride %q, want 200", rec.Code, status)
			}
		})
	}
}

// TestActivityToken_BadBodies covers the 400s.
func TestActivityToken_BadBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"malformed json", `{"activityToken":`},
		{"unknown field", `{"activityToken":"cafe01","nope":1}`},
		{"empty token", `{"activityToken":""}`},
		{"whitespace token", `{"activityToken":"   "}`},
		{"token too long", `{"activityToken":"` + strings.Repeat("a", maxActivityTokenLen+1) + `"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := newFakeActivityRegistry()
			store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", rideStatusAccepted)}
			h := newActivityHandler(store, registry, rideUserID)

			rec := httptest.NewRecorder()
			activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, tt.body))

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestActivityToken_EndIsIdempotent — the client's end and the server's
// terminal-state push race by design, so a second end is a 200 reporting false
// rather than an error.
func TestActivityToken_EndIsIdempotent(t *testing.T) {
	registry := newFakeActivityRegistry()
	registry.endResult = false // nothing live to close
	store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", rideStatusAccepted)}
	h := newActivityHandler(store, registry, rideUserID)

	rec := httptest.NewRecorder()
	activityMux(h).ServeHTTP(rec, activityRequest(http.MethodDelete, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body activityEndedResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Ended {
		t.Error("ended = true when nothing was live to close")
	}
}

// TestActivityToken_EndWorksOnTerminalRides — a completed ride is exactly when
// a client is most likely to end its Activity, so the terminal guard must NOT
// apply on this side.
func TestActivityToken_EndWorksOnTerminalRides(t *testing.T) {
	registry := newFakeActivityRegistry()
	store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", rideStatusCompleted)}
	h := newActivityHandler(store, registry, rideUserID)

	rec := httptest.NewRecorder()
	activityMux(h).ServeHTTP(rec, activityRequest(http.MethodDelete, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d on a completed ride, want 200 — ending is exactly what a client does here", rec.Code)
	}
}

// TestActivityToken_UnwiredRegistryIs500 pins the fail-closed default.
func TestActivityToken_UnwiredRegistryIs500(t *testing.T) {
	store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", rideStatusAccepted)}
	h := NewRideRequestHandler(
		&stubTokenValidator{userID: rideUserID},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)},
		store,
		&fakeRidePublisher{},
		discardLogger(),
	)

	rec := httptest.NewRecorder()
	activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, `{"activityToken":"cafe01"}`))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d with no registry wired, want 500", rec.Code)
	}
}

// TestActivityToken_StoreFailureIs500AndHidesTheToken.
func TestActivityToken_StoreFailureIs500AndHidesTheToken(t *testing.T) {
	registry := newFakeActivityRegistry()
	registry.registerErr = errors.New("pool exhausted")
	store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", rideStatusAccepted)}
	h := newActivityHandler(store, registry, rideUserID)

	// Built at runtime rather than as a literal: a string constant assigned to
	// a name like `secret` is exactly the shape gosec G101 flags, and it is a
	// test fixture, not a credential.
	secret := strings.Repeat("fee1", 5)
	rec := httptest.NewRecorder()
	activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, `{"activityToken":"`+secret+`"}`))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("the error envelope leaked the P1 activity token: %s", rec.Body.String())
	}
}

// --- MYR-172 review fixes ---

// expiredReservationRide is a ride exactly as the reservation sweeper leaves
// one: status still `accepted`, the give-up recorded only in the dispatch
// columns. Nothing about the ride's own status says the Activity is over.
func expiredReservationRide() RideRequestData {
	rec := fixtureRideData("clowner999999999999xyz", rideStatusAccepted)
	status := dispatchStatusFailed
	code := dispatchErrorReservationExpired
	rec.DispatchStatus = &status
	rec.DispatchError = &code
	return rec
}

// TestActivityToken_ExpiredReservationIsRefused is the headline fix.
//
// The sweeper ends and tombstones a late reservation's Activities but leaves
// the ride at `accepted`, and it LATCHES dispatched_at, so a second expiry is
// impossible. The register guard only read the status, and the upsert clears
// ended_at by design — so one ordinary ActivityKit token rotation after the
// expiry resurrected the row, the ETA ticker picked it back up, and the rider
// watched a phantom countdown to a pickup nobody was ever making, with nothing
// left in the system able to end it again.
func TestActivityToken_ExpiredReservationIsRefused(t *testing.T) {
	registry := newFakeActivityRegistry()
	store := &fakeRideStore{getRec: expiredReservationRide()}
	h := newActivityHandler(store, registry, rideUserID)

	rec := httptest.NewRecorder()
	activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, `{"activityToken":"cafe01"}`))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d for an expired reservation, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if registry.riderToken() != "" {
		t.Error("a token was registered against a ride whose reservation had expired —" +
			" the ETA ticker will resume a countdown nothing can ever end")
	}

	// The sub-code is load-bearing: the ride's status is `accepted`, so a
	// client seeing a bare `conflict` on a live-looking ride could not tell a
	// lapsed reservation from a server bug, and would keep retrying.
	var body struct {
		Error struct {
			Code    string  `json:"code"`
			SubCode *string `json:"subCode"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if body.Error.Code != string(wserrors.ErrCodeConflict) {
		t.Errorf("error.code = %q, want conflict", body.Error.Code)
	}
	if body.Error.SubCode == nil {
		t.Fatal("error.subCode is null; the client cannot distinguish this from an ordinary conflict")
	}
	if got, want := *body.Error.SubCode, string(wserrors.SubCodeReservationExpired); got != want {
		t.Errorf("error.subCode = %q, want %q", got, want)
	}
}

// TestActivityToken_OtherDispatchFailuresStillRegister is the counterweight,
// and it is why the guard reads BOTH dispatch columns rather than just the
// status.
//
// `dispatch_status = failed` on its own means any nav push that did not land —
// the car was asleep, the proxy was down. That ride is still genuinely
// happening (the owner can drive it manually), so its Activity must keep
// working, token rotations included. Refusing every failure would have swapped
// one broken lock screen for a different one.
func TestActivityToken_OtherDispatchFailuresStillRegister(t *testing.T) {
	for _, code := range []string{"vehicle_asleep", "coordinate_out_of_range", ""} {
		t.Run("dispatch_error="+code, func(t *testing.T) {
			rec := fixtureRideData("clowner999999999999xyz", rideStatusAccepted)
			status := dispatchStatusFailed
			rec.DispatchStatus = &status
			if code != "" {
				rec.DispatchError = &code
			}

			registry := newFakeActivityRegistry()
			h := newActivityHandler(&fakeRideStore{getRec: rec}, registry, rideUserID)

			w := httptest.NewRecorder()
			activityMux(h).ServeHTTP(w, activityRequest(http.MethodPost, `{"activityToken":"cafe01"}`))

			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 — a failed nav push does not end the ride", w.Code)
			}
			if registry.riderToken() != "cafe01" {
				t.Error("the rotation was refused for a ride that is still happening")
			}
		})
	}
}

// TestActivityToken_StoreGuardRaceIsConflict covers the OTHER half of the fix.
//
// The handler's checks are a read; the registration is a write. A POST that
// arrives while the ride is transitioning passes every check above and then
// races the terminal tombstone. The upsert's own SQL predicate is what actually
// holds, and its refusal must surface as a 409 rather than the 500 an
// unrecognised store error would produce — a 500 tells the client to retry,
// which is precisely wrong here.
func TestActivityToken_StoreGuardRaceIsConflict(t *testing.T) {
	registry := newFakeActivityRegistry()
	registry.registerErr = ErrLiveActivityRideClosed
	store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", rideStatusAccepted)}
	h := newActivityHandler(store, registry, rideUserID)

	rec := httptest.NewRecorder()
	activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, `{"activityToken":"cafe01"}`))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d when the write's guard refused, want 409 (body %s)", rec.Code, rec.Body.String())
	}

	// No sub-code: by this point we no longer know WHICH ending won the race,
	// and naming one would be a guess the client would act on.
	var body struct {
		Error struct {
			SubCode *string `json:"subCode"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if body.Error.SubCode != nil {
		t.Errorf("error.subCode = %q on a race we cannot attribute, want null", *body.Error.SubCode)
	}
}

// TestActivityToken_NonHexIsRejected pins the charset validation (P1 hardening).
//
// An APNs push token is the hex rendering of opaque binary and always has been,
// so anything else could not address a device even if we stored it. Rejecting
// it at the door means the pathological value never reaches the sender, which
// interpolates the token into an APNs request URL — the path on which the
// "never logged in full" rule in data-classification.md §1.18 has to hold.
func TestActivityToken_NonHexIsRejected(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"url metacharacters", "aabb/../%2e%2e"},
		{"whitespace inside", "aabb ccdd"},
		{"a query string", "aabbccdd?x=1"},
		{"newline (log injection)", `aabb\ncc`},
		{"plainly not a token", "not-a-token"},
		{"unicode", "aabbccdd東"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := newFakeActivityRegistry()
			store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", rideStatusAccepted)}
			h := newActivityHandler(store, registry, rideUserID)

			rec := httptest.NewRecorder()
			body, err := json.Marshal(map[string]string{"activityToken": tt.token})
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, string(body)))

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d for %q, want 400", rec.Code, tt.token)
			}
			// The rejection describes the RULE and never the value (P1).
			if strings.Contains(rec.Body.String(), tt.token) {
				t.Errorf("the 400 echoed the rejected token: %s", rec.Body.String())
			}
			if registry.riderToken() != "" {
				t.Error("a non-hex token was stored")
			}
		})
	}
}

// TestActivityToken_RealTokenShapesAccepted is the mirror: the validation must
// not reject anything Apple actually mints. Upper case is accepted because it
// is the same token, and a client that upper-cased its hex would otherwise file
// a bug nobody could diagnose.
func TestActivityToken_RealTokenShapesAccepted(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"64 lowercase hex, today's shape", strings.Repeat("0a1b2c3d", 8)},
		{"upper case", strings.Repeat("0A1B2C3D", 8)},
		{"mixed case", strings.Repeat("0a1B2c3D", 8)},
		{"a longer future token", strings.Repeat("f", maxActivityTokenLen)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := newFakeActivityRegistry()
			store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", rideStatusAccepted)}
			h := newActivityHandler(store, registry, rideUserID)

			rec := httptest.NewRecorder()
			activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, `{"activityToken":"`+tt.token+`"}`))

			if rec.Code != http.StatusOK {
				t.Errorf("status = %d for a valid token shape, want 200 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestIsTerminalRideStatusMatchesTheSender keeps the handler's terminal set in
// lockstep with the one the Live Activity sender uses to decide `event: "end"`.
//
// They cannot share a constant without internal/telemetry depending on
// internal/push, so this is the pin. If they drifted, a ride could be ended on
// the lock screen while the endpoint still accepted registrations for it — or
// the reverse, which is worse: a 409 on a ride the sender still considers live.
func TestIsTerminalRideStatusMatchesTheSender(t *testing.T) {
	// Mirrors push.terminalStatuses. Kept as a literal rather than an import
	// so the dependency direction stays one-way.
	senderTerminal := map[string]bool{
		"completed": true,
		"declined":  true,
		"cancelled": true,
	}

	for _, status := range []string{
		rideStatusRequested, rideStatusAccepted, rideStatusDeclined,
		rideStatusEnroute, rideStatusArrived, rideStatusCompleted, rideStatusCancelled,
	} {
		if got, want := isTerminalRideStatus(status), senderTerminal[status]; got != want {
			t.Errorf("isTerminalRideStatus(%q) = %v, but push.terminalStatuses says %v", status, got, want)
		}
	}
}
