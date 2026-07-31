// HTTP coverage for GET /api/vehicles/{vehicleId}/booked-windows (MYR-385,
// rest-api.md §7.22).
//
// The store-level equivalence — "the windows are exactly what the gate would
// refuse" — is pinned against a real Postgres in
// internal/store/ride_request_booked_windows_read_test.go. What is left for
// this layer is the part a picker actually meets: the AUTH MATRIX (which must
// be create's, exactly), the range validation, and the wire projection —
// including the P1 negative, that nothing about the occupying ride's party
// reaches the response.

package telemetry

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

const bookedWindowsPath = "/api/vehicles/" + fixtureSnapshotRowID + "/booked-windows"

// errBookedWindowsStore stands in for any store failure — the read is either
// authoritative or it is an outage; there is no degraded answer.
var errBookedWindowsStore = errors.New("booked-windows: store unavailable")

// windowsMux mounts the endpoint the way wiring.go does.
func windowsMux(h *RideRequestHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/vehicles/{vehicleId}/booked-windows", h.ServeBookedWindows)
	return mux
}

// windowsHandler builds a handler whose JWT resolves to callerID, over the
// fixture vehicle owned by shareOwnerUser, with the given share reader (nil =
// no sharing wired, the fail-closed default). The owner is pinned rather than
// parameterised: every case here varies the CALLER against one fixed car, and
// a second owner would only give the matrix a way to be accidentally trivial.
func windowsHandler(store RideRequestStore, callerID string, shares VehicleShareReader) *RideRequestHandler {
	reader := &stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)}
	opts := []RideRequestOption{}
	if shares != nil {
		opts = append(opts, WithRideShareReader(shares))
	}
	return NewRideRequestHandler(
		&stubTokenValidator{userID: callerID},
		reader,
		store,
		nil,
		discardLogger(),
		opts...,
	)
}

func decodeWindows(t *testing.T, body []byte) bookedWindowsResponse {
	t.Helper()
	var resp bookedWindowsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	return resp
}

// TestBookedWindowsAuthMatrix is the load-bearing test on this endpoint.
//
// The rule is not "similar to" the ride-create gate, it IS the ride-create
// gate: this surface answers "what would create refuse?", so a caller create
// would turn away must be turned away here identically or the endpoint becomes
// an oracle answering a question POST /api/ride-requests would not. The matrix
// is therefore the same four rows create's own gate is tested over, and a
// divergence in any of them is the bug this test exists to catch.
func TestBookedWindowsAuthMatrix(t *testing.T) {
	tests := []struct {
		name       string
		caller     string
		shares     VehicleShareReader
		wantStatus int
		wantCode   wserrors.ErrorCode
		wantStore  bool
	}{
		{
			name:   "owner is served",
			caller: shareOwnerUser,
			// The nil reader is the assertion: an owner must never need a
			// share lookup to see their own car's calendar.
			shares:     nil,
			wantStatus: http.StatusOK,
			wantStore:  true,
		},
		{
			name:       "viewer holding the ride capability is served",
			caller:     shareViewerUser,
			shares:     grantingReader(rideGrant),
			wantStatus: http.StatusOK,
			wantStore:  true,
		},
		{
			// The base-capability viewer can SEE the car and cannot BOOK it.
			// A picker they could never submit from has nothing to dim, and
			// serving them would turn any share at all into a way to watch a
			// stranger's car fill up.
			name:       "viewer without the ride capability is refused",
			caller:     shareViewerUser,
			shares:     grantingReader(baseGrant),
			wantStatus: http.StatusForbidden,
			wantCode:   wserrors.ErrCodeVehicleNotOwned,
		},
		{
			// Suspension denies identically to having no grant at all — the
			// caller learns nothing about which it is.
			name:       "suspended ride grant is refused",
			caller:     shareViewerUser,
			shares:     grantingReader(auth.ShareGrant{AllowRides: true, Suspended: true}),
			wantStatus: http.StatusForbidden,
			wantCode:   wserrors.ErrCodeVehicleNotOwned,
		},
		{
			name:       "stranger is refused",
			caller:     shareOtherUser,
			shares:     grantingReader(rideGrant),
			wantStatus: http.StatusForbidden,
			wantCode:   wserrors.ErrCodeVehicleNotOwned,
		},
		{
			// No sharing subsystem wired: every non-owner is denied rather
			// than silently admitted.
			name:       "non-owner with no share reader is refused",
			caller:     shareViewerUser,
			shares:     nil,
			wantStatus: http.StatusForbidden,
			wantCode:   wserrors.ErrCodeVehicleNotOwned,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{}
			h := windowsHandler(store, tt.caller, tt.shares)
			rec := doRequest(t, windowsMux(h), http.MethodGet, bookedWindowsPath, "", "Bearer t")

			if tt.wantStatus != http.StatusOK {
				assertErrEnvelope(t, rec, tt.wantStatus, tt.wantCode)
			} else if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d want 200. body=%s", rec.Code, rec.Body.String())
			}
			// A refused caller must not have reached the store at all — a read
			// that happened and was then discarded is still a read.
			if got := store.windowsCalls > 0; got != tt.wantStore {
				t.Errorf("store reached = %v, want %v", got, tt.wantStore)
			}
		})
	}
}

// TestBookedWindowsUnauthenticated pins that the token comes first, before the
// vehicle is even resolved.
func TestBookedWindowsUnauthenticated(t *testing.T) {
	store := &fakeRideStore{}
	h := windowsHandler(store, shareOwnerUser, nil)
	rec := doRequest(t, windowsMux(h), http.MethodGet, bookedWindowsPath, "", "")
	assertErrEnvelope(t, rec, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed)
	if store.windowsCalls != 0 {
		t.Error("an unauthenticated call must not reach the store")
	}
}

// TestBookedWindowsUnknownVehicle pins the 404, which must be indistinguishable
// from the vehicle simply not existing — the same split every per-vehicle
// handler makes.
func TestBookedWindowsUnknownVehicle(t *testing.T) {
	store := &fakeRideStore{}
	reader := &stubVehicleSnapshotReader{err: fmtNotFound()}
	h := NewRideRequestHandler(
		&stubTokenValidator{userID: shareOwnerUser}, reader, store, nil, discardLogger())
	rec := doRequest(t, windowsMux(h), http.MethodGet, bookedWindowsPath, "", "Bearer t")
	assertErrEnvelope(t, rec, http.StatusNotFound, wserrors.ErrCodeNotFound)
}

// TestBookedWindowsRangeValidation covers §7.22's four validation rules.
//
// The two that matter are the ones that REFUSE rather than clamp. An empty or
// reversed range can only ever answer `items: []`, which a picker reads as
// "wide open" — a wrong answer dressed as a right one. An over-wide range is
// refused for the same reason: a silently clamped answer looks complete and is
// not, and under-dimming is precisely the failure this endpoint removes.
func TestBookedWindowsRangeValidation(t *testing.T) {
	base := time.Date(2029, 6, 12, 15, 0, 0, 0, time.UTC)
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }

	tests := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{"no params defaults to now..now+7d", "", http.StatusOK},
		{"explicit valid range", "?from=" + rfc(base) + "&to=" + rfc(base.Add(24*time.Hour)), http.StatusOK},
		{"from only, to defaults", "?from=" + rfc(base), http.StatusOK},
		{"to only, from defaults to now", "?to=" + rfc(time.Now().Add(time.Hour)), http.StatusOK},
		{"from equals to", "?from=" + rfc(base) + "&to=" + rfc(base), http.StatusBadRequest},
		{"from after to", "?from=" + rfc(base.Add(time.Hour)) + "&to=" + rfc(base), http.StatusBadRequest},
		{"range wider than 14 days", "?from=" + rfc(base) + "&to=" + rfc(base.Add(15*24*time.Hour)), http.StatusBadRequest},
		{"exactly 14 days is allowed", "?from=" + rfc(base) + "&to=" + rfc(base.Add(14*24*time.Hour)), http.StatusOK},
		{"unparseable from", "?from=tomorrow", http.StatusBadRequest},
		{"bare date is not RFC 3339", "?from=2029-06-12", http.StatusBadRequest},
		{"unparseable to", "?from=" + rfc(base) + "&to=soon", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{}
			h := windowsHandler(store, shareOwnerUser, nil)
			rec := doRequest(t, windowsMux(h), http.MethodGet, bookedWindowsPath+tt.query, "", "Bearer t")

			if tt.wantStatus == http.StatusBadRequest {
				assertErrEnvelope(t, rec, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest)
				if store.windowsCalls != 0 {
					t.Error("an invalid range must not reach the store")
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d want 200. body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestBookedWindowsDefaultRange pins the defaults §7.22 promises, by reading
// what the handler actually handed the store.
func TestBookedWindowsDefaultRange(t *testing.T) {
	store := &fakeRideStore{}
	h := windowsHandler(store, shareOwnerUser, nil)

	before := time.Now().UTC()
	rec := doRequest(t, windowsMux(h), http.MethodGet, bookedWindowsPath, "", "Bearer t")
	after := time.Now().UTC()

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := store.windowsCall
	if got.from.Before(before.Add(-time.Second)) || got.from.After(after.Add(time.Second)) {
		t.Errorf("default from = %s, want ~now (%s..%s)", got.from, before, after)
	}
	if want := got.from.Add(bookedWindowsDefaultRange); !got.to.Equal(want) {
		t.Errorf("default to = %s, want from+7d = %s", got.to, want)
	}
	if got.vehicleID != fixtureSnapshotRowID {
		t.Errorf("vehicleID = %q, want %q", got.vehicleID, fixtureSnapshotRowID)
	}
	// `own` is resolved against the JWT SUBJECT — there is no client-supplied
	// identity on this endpoint and there must never be one.
	if got.callerID != shareOwnerUser {
		t.Errorf("callerID = %q, want the JWT subject %q", got.callerID, shareOwnerUser)
	}
}

// TestBookedWindowsMaxRangeMirrorsTheStore pins the two caps together. The
// handler layer cannot import internal/store, so the constant is duplicated —
// and a duplicated constant that nothing compares is a constant that drifts.
func TestBookedWindowsMaxRangeMirrorsTheStore(t *testing.T) {
	// store.MaxBookedWindowRange, restated. Kept as an arithmetic expression
	// rather than a duration literal so the two read the same way.
	const storeMax = 14 * 24 * time.Hour
	if bookedWindowsMaxRange != storeMax {
		t.Fatalf("handler cap %s != store cap %s", bookedWindowsMaxRange, storeMax)
	}
}

// TestBookedWindowsWireProjection pins the response shape, including the P1
// negative: the raw JSON must contain the two instants and the two flags and
// NOTHING about the occupying ride's party.
func TestBookedWindowsWireProjection(t *testing.T) {
	start := time.Date(2029, 6, 12, 11, 15, 0, 0, time.UTC)
	store := &fakeRideStore{
		windowsResult: []BookedWindowData{
			{Start: start, End: start.Add(90 * time.Minute), Pending: true, Own: false},
			{Start: start.Add(4 * time.Hour), End: start.Add(5*time.Hour + 30*time.Minute), Pending: false, Own: true},
		},
	}
	h := windowsHandler(store, shareOwnerUser, nil)
	rec := doRequest(t, windowsMux(h), http.MethodGet, bookedWindowsPath, "", "Bearer t")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}

	resp := decodeWindows(t, rec.Body.Bytes())
	if len(resp.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(resp.Items))
	}
	if resp.Items[0].Start != "2029-06-12T11:15:00Z" {
		t.Errorf("start = %q, want RFC 3339 UTC", resp.Items[0].Start)
	}
	if resp.Items[0].End != "2029-06-12T12:45:00Z" {
		t.Errorf("end = %q", resp.Items[0].End)
	}
	if !resp.Items[0].Pending || resp.Items[0].Own {
		t.Errorf("item[0] flags = pending %v own %v, want true/false", resp.Items[0].Pending, resp.Items[0].Own)
	}
	if resp.Items[1].Pending || !resp.Items[1].Own {
		t.Errorf("item[1] flags = pending %v own %v, want false/true", resp.Items[1].Pending, resp.Items[1].Own)
	}

	// The keys, exhaustively — the schema is additionalProperties:false and an
	// extra key here is a contract break, not a nicety.
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	for i, item := range raw.Items {
		if len(item) != 4 {
			t.Errorf("item[%d] has %d keys, want exactly 4: %v", i, len(item), item)
		}
		for _, key := range []string{"start", "end", "pending", "own"} {
			if _, ok := item[key]; !ok {
				t.Errorf("item[%d] missing %q", i, key)
			}
		}
		// The P1 guard, stated as a list rather than left to review: the
		// conflicting ride's id, rider, requester name and places belong to a
		// party the caller is not (data-classification.md §1.9).
		for _, forbidden := range []string{
			"id", "rideRequestId", "riderId", "ownerId", "requesterName",
			"passengerName", "passengerPhone", "pickup", "dropoff", "status", "scheduledFor",
		} {
			if _, ok := item[forbidden]; ok {
				t.Errorf("item[%d] leaks %q — that field belongs to the other party", i, forbidden)
			}
		}
	}
}

// TestBookedWindowsFreeVehicleIsEmptyArray pins the common case. `items: []`,
// never `null` and never an omitted key: a picker must render a free car as
// unrestricted, and a null would be a decode hazard on the way there.
func TestBookedWindowsFreeVehicleIsEmptyArray(t *testing.T) {
	store := &fakeRideStore{windowsResult: nil}
	h := windowsHandler(store, shareOwnerUser, nil)
	rec := doRequest(t, windowsMux(h), http.MethodGet, bookedWindowsPath, "", "Bearer t")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != "{\"items\":[]}\n" {
		t.Errorf("body = %q, want an empty items array", body)
	}
}

// TestBookedWindowsStoreFailureIs500 pins that a read failure is an outage, not
// an empty calendar. Answering `items: []` on a query error would tell the
// picker the car is wide open — the single most dangerous wrong answer this
// endpoint can give.
func TestBookedWindowsStoreFailureIs500(t *testing.T) {
	store := &fakeRideStore{windowsErr: errBookedWindowsStore}
	h := windowsHandler(store, shareOwnerUser, nil)
	rec := doRequest(t, windowsMux(h), http.MethodGet, bookedWindowsPath, "", "Bearer t")
	assertErrEnvelope(t, rec, http.StatusInternalServerError, wserrors.ErrCodeInternalError)
}
