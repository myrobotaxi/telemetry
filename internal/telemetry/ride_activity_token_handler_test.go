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

func (f *fakeActivityRegistry) tokenFor(rideID, userID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[activityKey(rideID, userID)]
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

func activityRequest(method, rideID, body string) *http.Request {
	req := httptest.NewRequest(method, "/api/ride-requests/"+rideID+"/activity-token", strings.NewReader(body))
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
			activityMux(h).ServeHTTP(rec, activityRequest(
				http.MethodPost, rideID, `{"activityToken":"abc123","sandbox":true}`))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}

			// Nothing but a successful call may have written a row.
			wrote := registry.tokenFor(rideID, rideUserID) != ""
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
			activityMux(h).ServeHTTP(rec, activityRequest(http.MethodDelete, rideID, ""))

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
	activityMux(h).ServeHTTP(rec, activityRequest(
		http.MethodPost, rideID, `{"activityToken":"tok-abc","sandbox":true}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := registry.tokenFor(rideID, rideUserID); got != "tok-abc" {
		t.Errorf("token stored against (ride, rider) = %q, want tok-abc", got)
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

	const secret = "super-secret-activity-token"
	rec := httptest.NewRecorder()
	activityMux(h).ServeHTTP(rec, activityRequest(
		http.MethodPost, rideID, `{"activityToken":"`+secret+`"}`))

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

	for _, token := range []string{"tok-first", "tok-rotated"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, activityRequest(http.MethodPost, rideID, `{"activityToken":"`+token+`"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("registering %s: status = %d, want 200", token, rec.Code)
		}
	}

	if got := registry.tokenFor(rideID, rideUserID); got != "tok-rotated" {
		t.Errorf("stored token = %q, want the rotated value tok-rotated", got)
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
			activityMux(h).ServeHTTP(rec, activityRequest(
				http.MethodPost, rideID, `{"activityToken":"tok"}`))

			if rec.Code != http.StatusConflict {
				t.Errorf("status = %d for terminal ride %q, want 409", rec.Code, status)
			}
			if registry.tokenFor(rideID, rideUserID) != "" {
				t.Error("a token was stored against a ride that had already ended")
			}
		})
	}
}

// TestActivityToken_NonTerminalRidesAccepted is the mirror of the above, and
// guards the statuses most easily mistaken for endings.
func TestActivityToken_NonTerminalRidesAccepted(t *testing.T) {
	for _, status := range []string{rideStatusRequested, rideStatusAccepted, rideStatusArrived, rideStatusEnroute} {
		t.Run(status, func(t *testing.T) {
			registry := newFakeActivityRegistry()
			store := &fakeRideStore{getRec: fixtureRideData("clowner999999999999xyz", status)}
			h := newActivityHandler(store, registry, rideUserID)

			rec := httptest.NewRecorder()
			activityMux(h).ServeHTTP(rec, activityRequest(
				http.MethodPost, rideID, `{"activityToken":"tok"}`))

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
		{"unknown field", `{"activityToken":"tok","nope":1}`},
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
			activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, rideID, tt.body))

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
	activityMux(h).ServeHTTP(rec, activityRequest(http.MethodDelete, rideID, ""))

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
	activityMux(h).ServeHTTP(rec, activityRequest(http.MethodDelete, rideID, ""))

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
	activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, rideID, `{"activityToken":"tok"}`))

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

	const secret = "leaky-token-value"
	rec := httptest.NewRecorder()
	activityMux(h).ServeHTTP(rec, activityRequest(http.MethodPost, rideID, `{"activityToken":"`+secret+`"}`))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("the error envelope leaked the P1 activity token: %s", rec.Body.String())
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
