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
	"net/url"
	"testing"
	"time"

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

// testBookedWindowsMax is what these tests INJECT as the §7.22 cap. It happens
// to equal the production value (store.MaxBookedWindowRange, carried in by
// wiring.go) so the §7.22 boundary cases below read as the documented ones —
// but nothing here asserts the two are equal, because a test comparing two
// literals in two packages proves only that somebody typed the same number
// twice. That the SERVER runs on the store's value is a property of wiring.go
// and is pinned in cmd/telemetry-server/wiring_routes_test.go;
// TestBookedWindowsCapIsTheInjectedOne below pins that the handler honours
// whatever it is handed rather than a number of its own.
const testBookedWindowsMax = 14 * 24 * time.Hour

// windowsHandler builds a handler whose JWT resolves to callerID, over the
// fixture vehicle owned by shareOwnerUser, with the given share reader (nil =
// no sharing wired, the fail-closed default). The owner is pinned rather than
// parameterised: every case here varies the CALLER against one fixed car, and
// a second owner would only give the matrix a way to be accidentally trivial.
func windowsHandler(store RideRequestStore, callerID string, shares VehicleShareReader) *RideRequestHandler {
	return windowsHandlerWithMax(store, callerID, shares, testBookedWindowsMax)
}

// windowsHandlerWithMax is windowsHandler with the §7.22 range cap under the
// test's control — zero meaning "wiring.go forgot the option".
func windowsHandlerWithMax(
	store RideRequestStore, callerID string, shares VehicleShareReader, maxRange time.Duration,
) *RideRequestHandler {
	reader := &stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)}
	opts := []RideRequestOption{WithBookedWindowsMaxRange(maxRange)}
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

// TestBookedWindowsAuthMatrix covers what is SPECIFIC to this endpoint's gate.
//
// The rule is not "similar to" the ride-create gate, it IS the ride-create
// gate: this surface answers "what would create refuse?", so a caller create
// would turn away must be turned away here identically or the endpoint becomes
// an oracle answering a question POST /api/ride-requests would not. THE TIER
// MATRIX ITSELF LIVES WHERE EVERY OTHER PER-VEHICLE ENDPOINT'S DOES —
// TestEndpointGrantMatrix in vehicle_share_access_test.go, where this endpoint
// is now a row carrying the same expectations as POST /api/ride-requests, so
// the two gates are compared side by side in one table rather than in two
// files that can drift.
//
// What is left here is what that table cannot express: the two REFUSALS whose
// shape matters (the error code, not just the status), and the two "no
// subsystem wired" defaults which are properties of this handler's
// construction rather than of a tier.
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
			// The base-capability viewer can SEE the car and cannot BOOK it.
			// A picker they could never submit from has nothing to dim, and
			// serving them would turn any share at all into a way to watch a
			// stranger's car fill up. Pinned here for the CODE: the refusal
			// must be vehicle_not_owned, indistinguishable from a stranger's.
			name:       "viewer without the ride capability is refused",
			caller:     shareViewerUser,
			shares:     grantingReader(baseGrant),
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
			// than silently admitted. Not a tier at all — a property of how
			// this handler is constructed, so the tier table cannot state it.
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
		&stubTokenValidator{userID: shareOwnerUser}, reader, store, nil, discardLogger(),
		WithBookedWindowsMaxRange(testBookedWindowsMax))
	rec := doRequest(t, windowsMux(h), http.MethodGet, bookedWindowsPath, "", "Bearer t")
	assertErrEnvelope(t, rec, http.StatusNotFound, wserrors.ErrCodeNotFound)
}

// TestBookedWindowsRangeValidation covers §7.22's validation rules AND, for
// every accepted range, the instants the handler actually handed the store.
//
// THE SECOND HALF IS THE POINT. A status-only table passes unchanged against a
// handler that parses `from`/`to` and then ignores them — the picker would be
// answered about the default week whatever it asked for, and every assertion
// here would stay green. So each 200 row states the exact [from, to) the store
// must have been called with.
//
// The two rules that matter are the ones that REFUSE rather than clamp. An
// empty or reversed range can only ever answer `items: []`, which a picker
// reads as "wide open" — a wrong answer dressed as a right one. An over-wide
// range is refused for the same reason: a silently clamped answer looks
// complete and is not, and under-dimming is precisely the failure this endpoint
// removes.
func TestBookedWindowsRangeValidation(t *testing.T) {
	base := time.Date(2029, 6, 12, 15, 0, 0, 0, time.UTC)
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	at := func(t time.Time) *time.Time { return &t }

	// An RFC 3339 instant carrying a NON-ZERO UTC offset, percent-encoded for
	// the query string. See the "+ must be percent-encoded" case below.
	offsetFrom := "2029-06-12T15:00:00+02:00"
	offsetTo := "2029-06-13T15:00:00+02:00"
	enc := func(s string) string { return url.QueryEscape(s) }

	tests := []struct {
		name       string
		query      string
		wantStatus int
		// wantFrom/wantTo are the instants the STORE must be called with. A nil
		// wantFrom means the row's `from` is "now" and is pinned by
		// TestBookedWindowsDefaultRange instead.
		wantFrom, wantTo *time.Time
	}{
		{
			name: "no params defaults to now..now+7d", query: "", wantStatus: http.StatusOK,
		},
		{
			name:  "explicit valid range",
			query: "?from=" + rfc(base) + "&to=" + rfc(base.Add(24*time.Hour)), wantStatus: http.StatusOK,
			wantFrom: at(base), wantTo: at(base.Add(24 * time.Hour)),
		},
		{
			name: "from only, to defaults", query: "?from=" + rfc(base), wantStatus: http.StatusOK,
			wantFrom: at(base), wantTo: at(base.Add(bookedWindowsDefaultRange)),
		},
		{
			name: "to only, from defaults to now", query: "?to=" + rfc(time.Now().Add(time.Hour)), wantStatus: http.StatusOK,
		},
		{
			// A NON-UTC offset is legal RFC 3339 and must be honoured as the
			// INSTANT it names, not re-read as wall time: 15:00+02:00 is
			// 13:00Z, and a handler that dropped the offset would dim the wrong
			// two hours of the picker.
			name:  "non-UTC offset is honoured as the instant it names",
			query: "?from=" + enc(offsetFrom) + "&to=" + enc(offsetTo), wantStatus: http.StatusOK,
			wantFrom: at(time.Date(2029, 6, 12, 13, 0, 0, 0, time.UTC)),
			wantTo:   at(time.Date(2029, 6, 13, 13, 0, 0, 0, time.UTC)),
		},
		{
			// AND THE TRAP, pinned so §7.22's caveat has a test behind it: a
			// LITERAL `+` in a query string decodes to a SPACE, so the
			// unencoded form of the row above arrives as "2029-06-12T15:00:00
			// 02:00" and is not RFC 3339 at all. The 400 is correct; clients
			// must percent-encode it (or just send UTC `Z`).
			name:  "a literal + in the query string decodes to a space and is refused",
			query: "?from=" + offsetFrom, wantStatus: http.StatusBadRequest,
		},
		{name: "from equals to", query: "?from=" + rfc(base) + "&to=" + rfc(base), wantStatus: http.StatusBadRequest},
		{
			name:  "from after to",
			query: "?from=" + rfc(base.Add(time.Hour)) + "&to=" + rfc(base), wantStatus: http.StatusBadRequest,
		},
		{
			name:  "range wider than 14 days",
			query: "?from=" + rfc(base) + "&to=" + rfc(base.Add(15*24*time.Hour)), wantStatus: http.StatusBadRequest,
		},
		{
			name:  "exactly 14 days is allowed",
			query: "?from=" + rfc(base) + "&to=" + rfc(base.Add(14*24*time.Hour)), wantStatus: http.StatusOK,
			wantFrom: at(base), wantTo: at(base.Add(14 * 24 * time.Hour)),
		},
		{name: "unparseable from", query: "?from=tomorrow", wantStatus: http.StatusBadRequest},
		{name: "bare date is not RFC 3339", query: "?from=2029-06-12", wantStatus: http.StatusBadRequest},
		{name: "unparseable to", query: "?from=" + rfc(base) + "&to=soon", wantStatus: http.StatusBadRequest},
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
			if store.windowsCalls != 1 {
				t.Fatalf("store calls = %d, want exactly 1", store.windowsCalls)
			}
			if tt.wantFrom != nil && !store.windowsCall.from.Equal(*tt.wantFrom) {
				t.Errorf("store from = %s, want %s", store.windowsCall.from.UTC(), tt.wantFrom.UTC())
			}
			if tt.wantTo != nil && !store.windowsCall.to.Equal(*tt.wantTo) {
				t.Errorf("store to = %s, want %s", store.windowsCall.to.UTC(), tt.wantTo.UTC())
			}
		})
	}
}

// TestBookedWindowsCapIsTheInjectedOne pins that the cap is the value WIRED IN,
// not a number the handler keeps of its own — which is the whole reason
// store.MaxBookedWindowRange is now passed through wiring.go instead of being
// restated here. A handler still holding a private 14 days passes every §7.22
// boundary case above and fails both rows below.
func TestBookedWindowsCapIsTheInjectedOne(t *testing.T) {
	base := time.Date(2029, 6, 12, 15, 0, 0, 0, time.UTC)
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	rangeQuery := func(d time.Duration) string {
		return "?from=" + rfc(base) + "&to=" + rfc(base.Add(d))
	}

	tests := []struct {
		name       string
		maxRange   time.Duration
		span       time.Duration
		wantStatus int
		wantCode   wserrors.ErrorCode
	}{
		{
			// A cap NARROWER than the production one: a 3-day ask must now be
			// refused, which no hard-coded 14 days would do.
			name:     "a narrower injected cap refuses a span the default would allow",
			maxRange: 2 * 24 * time.Hour, span: 3 * 24 * time.Hour,
			wantStatus: http.StatusBadRequest, wantCode: wserrors.ErrCodeInvalidRequest,
		},
		{
			name: "exactly the injected cap is allowed", maxRange: 2 * 24 * time.Hour, span: 2 * 24 * time.Hour,
			wantStatus: http.StatusOK,
		},
		{
			// A cap WIDER than production: a 20-day ask must now be served.
			name:     "a wider injected cap allows a span the default would refuse",
			maxRange: 30 * 24 * time.Hour, span: 20 * 24 * time.Hour,
			wantStatus: http.StatusOK,
		},
		{
			// wiring.go forgot the option. Serving without a cap would let one
			// call ask about a decade, so the endpoint refuses everything and
			// says so in the log — the same fail-closed reading a missing Live
			// Activity registry gets (§7.21).
			name:     "an unconfigured cap is a deployment error, not an open door",
			maxRange: 0, span: time.Hour,
			wantStatus: http.StatusInternalServerError, wantCode: wserrors.ErrCodeInternalError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{}
			h := windowsHandlerWithMax(store, shareOwnerUser, nil, tt.maxRange)
			rec := doRequest(t, windowsMux(h), http.MethodGet,
				bookedWindowsPath+rangeQuery(tt.span), "", "Bearer t")

			if tt.wantStatus != http.StatusOK {
				assertErrEnvelope(t, rec, tt.wantStatus, tt.wantCode)
				if store.windowsCalls != 0 {
					t.Error("a refused range must not reach the store")
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d want 200. body=%s", rec.Code, rec.Body.String())
			}
			if store.windowsCalls != 1 {
				t.Errorf("store calls = %d, want 1", store.windowsCalls)
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
