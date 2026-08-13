package telemetry

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// MYR-540 — the group ride at the HTTP boundary: the wire's three presence
// rules, the join endpoint's full error matrix, and which verbs the widened
// party set actually reaches.

const (
	groupJoinerID = "cjoiner540"
	groupOwnerID  = "cowner540"
	groupCode     = "RBO246"
)

// testRideSigner builds a signer over a fixed seed so a test can verify the
// signature the projection actually emitted rather than merely observe a
// non-empty string.
func testRideSigner(t *testing.T) *InviteLinkSigner {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	s, err := NewInviteLinkSigner(seed)
	if err != nil {
		t.Fatalf("build signer: %v", err)
	}
	return s
}

// groupRideData is fixtureRideData turned into an ACCEPTED group ride carrying a
// live code — the state the link is emitted from.
func groupRideData(status string) RideRequestData {
	rec := fixtureRideData(groupOwnerID, status)
	rec.GroupRide = true
	rec.JoinCode = groupCode
	exp := rec.UpdatedAt.Add(7 * 24 * time.Hour)
	rec.JoinCodeExpiresAt = &exp
	return rec
}

// --- shareUrl presence rules -------------------------------------------------

func TestRideWire_ShareURLPresenceRules(t *testing.T) {
	signer := testRideSigner(t)
	base := groupRideData(rideStatusAccepted)
	// `now` sits inside the code's TTL and well before any linger boundary.
	now := base.UpdatedAt.Add(time.Minute)

	tests := []struct {
		name string
		rec  func() RideRequestData
		now  time.Time
		want bool
	}{
		{
			name: "accepted group ride with a live code emits the link",
			rec:  func() RideRequestData { return base },
			now:  now,
			want: true,
		},
		{
			// The flag alone says nothing about links: `groupRide: true` with no
			// code is the ordinary state of a ride still waiting for the owner.
			name: "group ride before accept has no code and therefore no link",
			rec: func() RideRequestData {
				r := groupRideData(rideStatusRequested)
				r.JoinCode, r.JoinCodeExpiresAt = "", nil
				return r
			},
			now:  now,
			want: false,
		},
		{
			// A solo ride never gets one, whatever else is set on the row.
			name: "solo ride never emits a link even carrying a code",
			rec: func() RideRequestData {
				r := base
				r.GroupRide = false
				return r
			},
			now:  now,
			want: false,
		},
		{
			name: "an expired code emits nothing",
			rec:  func() RideRequestData { return base },
			now:  base.UpdatedAt.Add(8 * 24 * time.Hour),
			want: false,
		},
		{
			// Terminal PLUS the linger. Inside the window the field survives, so
			// a share sheet open when the owner tapped "Dropped off" does not
			// have a control blank under it mid-interaction.
			name: "a completed ride still emits inside the linger",
			rec:  func() RideRequestData { return groupRideData(rideStatusCompleted) },
			now:  base.UpdatedAt.Add(rideShareURLLinger - time.Second),
			want: true,
		},
		{
			name: "a completed ride stops emitting past the linger",
			rec:  func() RideRequestData { return groupRideData(rideStatusCompleted) },
			now:  base.UpdatedAt.Add(rideShareURLLinger + time.Second),
			want: false,
		},
		{
			// A cancelled ride has no CompletedAt at all, which is exactly why
			// the linger is measured from UpdatedAt: reading a zero CompletedAt
			// would put the window's end in 1970 and make the linger zero here.
			name: "a cancelled ride uses the same linger despite having no completedAt",
			rec:  func() RideRequestData { return groupRideData(rideStatusCancelled) },
			now:  base.UpdatedAt.Add(rideShareURLLinger - time.Second),
			want: true,
		},
		{
			name: "a declined ride stops emitting past the linger",
			rec:  func() RideRequestData { return groupRideData(rideStatusDeclined) },
			now:  base.UpdatedAt.Add(rideShareURLLinger + time.Second),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := toRideRequestWire(tc.rec(), signer, tc.now)
			if got := w.ShareURL != ""; got != tc.want {
				t.Fatalf("shareUrl present = %v, want %v (got %q)", got, tc.want, w.ShareURL)
			}
			// groupRide is projected unconditionally and independently: absence
			// of a link must never be readable as "not a group ride".
			if w.GroupRide != tc.rec().GroupRide {
				t.Errorf("groupRide = %v, want %v", w.GroupRide, tc.rec().GroupRide)
			}
		})
	}
}

// TestRideWire_ShareURLIsSignedOverTheRideDomain pins the URL's shape AND the
// domain separation, which is the whole reason one key is safe for two link
// kinds. A signature that verified against the invite payload would mean a valid
// invite link could be re-punctuated into a valid ride link.
func TestRideWire_ShareURLIsSignedOverTheRideDomain(t *testing.T) {
	signer := testRideSigner(t)
	rec := groupRideData(rideStatusAccepted)
	got := toRideRequestWire(rec, signer, rec.UpdatedAt.Add(time.Minute)).ShareURL

	if !strings.HasPrefix(got, RideLinkBaseURL+groupCode+"?k=") {
		t.Fatalf("shareUrl = %q, want the /ride/{code}?k= shape", got)
	}
	k := got[strings.Index(got, "?k=")+len("?k="):]
	parts := strings.Split(k, ".")
	if len(parts) != 3 {
		t.Fatalf("k = %q, want kid.exp.sig", k)
	}
	if parts[0] != inviteLinkKeyID {
		t.Errorf("key id = %q, want %q — the shell holds one key for both link kinds", parts[0], inviteLinkKeyID)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature is not base64url-unpadded: %v", err)
	}

	pub, err := base64.StdEncoding.DecodeString(signer.PublicKeyBase64())
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	if !ed25519.Verify(pub, []byte(rideLinkPayload(groupCode, parts[1])), sig) {
		t.Fatal("signature does not verify over `ride:{code}:{exp}` — the shell would bounce this link")
	}
	// The SAME bytes must NOT verify as an invite-link signature. This is the
	// property that makes sharing one key safe.
	if ed25519.Verify(pub, []byte(inviteLinkPayload(groupCode, parts[1], "", "")), sig) {
		t.Fatal("a ride signature verified as an invite signature; the domains are not separated")
	}
}

// TestRideWire_MembersOmittedWhenEmpty pins the additive rule that keeps every
// pre-MYR-540 row byte-identical: absent and empty mean the same thing, so a
// solo ride needs no `members: []`.
func TestRideWire_MembersOmittedWhenEmpty(t *testing.T) {
	rec := fixtureRideData(groupOwnerID, rideStatusAccepted)
	body, err := json.Marshal(toRideRequestWire(rec, nil, time.Now()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"members"`, `"shareUrl"`, `"groupRide"`} {
		if strings.Contains(string(body), key) {
			t.Errorf("solo ride serialized %s; pre-MYR-540 rows must be byte-identical: %s", key, body)
		}
	}
}

// TestRideWire_MembersProjectJoinersOnly pins the counting rule: the requester
// is never duplicated into the list, so a surface renders len(members)+1.
func TestRideWire_MembersProjectJoinersOnly(t *testing.T) {
	rec := groupRideData(rideStatusEnroute)
	rec.Members = []RideMemberData{
		{UserID: groupJoinerID, FirstName: "Sam"},
		{UserID: "cjoiner540b", FirstName: "Ada"},
	}
	w := toRideRequestWire(rec, nil, rec.UpdatedAt.Add(time.Minute))
	if len(w.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(w.Members))
	}
	for _, m := range w.Members {
		if m.UserID == rec.RiderID {
			t.Fatal("the requester appeared in members; the rendered count would be one too high")
		}
		if m.FirstName == "" {
			t.Error("firstName is empty; the ladder must always produce a printable name")
		}
	}
}

// --- the join endpoint -------------------------------------------------------

func joinMux(h *RideRequestHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/ride-requests/join", h.ServeJoin)
	return mux
}

func postJoin(h *RideRequestHandler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/ride-requests/join", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	joinMux(h).ServeHTTP(rec, req)
	return rec
}

func TestRideJoin_ErrorMatrix(t *testing.T) {
	joined := groupRideData(rideStatusAccepted)
	joined.Members = []RideMemberData{{UserID: groupJoinerID, FirstName: "Sam"}}

	tests := []struct {
		name       string
		body       string
		storeErr   error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "a body that is not JSON is a 400",
			body:       `{`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			// additionalProperties:false on the request schema.
			name:       "an unknown key is a 400",
			body:       `{"code":"RBO246","rideId":"cride1"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			// MALFORMED is 400, never 404: the 404 body is reserved for "this
			// code gets you nothing", and blurring the two would make the
			// deliberate indistinguishability harder to reason about.
			name:       "a short code is a 400, not a 404",
			body:       `{"code":"RBO"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "an empty code is a 400",
			body:       `{"code":""}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "an unknown/expired/terminal code is a 404",
			body:       `{"code":"RBO246"}`,
			storeErr:   sdk.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "a caller who is already a party is a 409",
			body:       `{"code":"RBO246"}`,
			storeErr:   ErrRideJoinSelfParty,
			wantStatus: http.StatusConflict,
			wantCode:   "conflict",
		},
		{
			name:       "a store failure is a 500",
			body:       `{"code":"RBO246"}`,
			storeErr:   errors.New("boom"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
		},
		{
			name:       "a good code is a 200",
			body:       `{"code":"RBO246"}`,
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeRideStore{joinRec: joined, joinCreated: true, joinErr: tc.storeErr}
			h := newRideHandler(store, &stubVehicleSnapshotReader{}, nil, groupJoinerID)
			res := postJoin(h, tc.body)
			if res.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", res.Code, tc.wantStatus, res.Body.String())
			}
			if tc.wantCode != "" && !strings.Contains(res.Body.String(), `"`+tc.wantCode+`"`) {
				t.Errorf("body = %s, want error code %q", res.Body.String(), tc.wantCode)
			}
			// THE CODE IS A BEARER CREDENTIAL AND MUST NEVER BE ECHOED. An error
			// message repeating it is a confirmation oracle.
			if strings.Contains(res.Body.String(), groupCode) {
				t.Errorf("the response echoed the join code: %s", res.Body.String())
			}
		})
	}
}

// TestRideJoin_UnauthenticatedIsRefused pins the product decision: app plus
// sign-in are required, and the joining account is the JWT subject.
func TestRideJoin_UnauthenticatedIsRefused(t *testing.T) {
	store := &fakeRideStore{joinRec: groupRideData(rideStatusAccepted)}
	h := newRideHandler(store, &stubVehicleSnapshotReader{}, nil, groupJoinerID)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/ride-requests/join", strings.NewReader(`{"code":"RBO246"}`))
	res := httptest.NewRecorder()
	joinMux(h).ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Code)
	}
	if store.joinCalls != 0 {
		t.Error("an unauthenticated request reached the store; the code space would be enumerable without an account")
	}
}

// TestRideJoin_NotFoundBodyIsIndistinguishable is the non-oracle property stated
// as an assertion rather than a comment: the body an UNKNOWN code produces must
// be byte-identical to the body a TERMINAL ride's code produces. It holds by
// construction — the store collapses both into one sentinel — and this pins that
// nothing downstream un-collapses them.
func TestRideJoin_NotFoundBodyIsIndistinguishable(t *testing.T) {
	bodies := make([]string, 0, 2)
	for _, storeErr := range []error{
		sdk.ErrNotFound, // unknown code
		// The store wraps the same sentinel for an expired code and for a code
		// on a ride that has reached a terminal status.
		errors.New("RideRequestRepo.JoinRideByCode: ride join code " + sdk.ErrNotFound.Error()),
	} {
		store := &fakeRideStore{joinErr: wrapNotFound(storeErr)}
		h := newRideHandler(store, &stubVehicleSnapshotReader{}, nil, groupJoinerID)
		res := postJoin(h, `{"code":"RBO246"}`)
		if res.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", res.Code)
		}
		bodies = append(bodies, res.Body.String())
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("404 bodies differ:\n unknown  = %s\n terminal = %s\nthe endpoint is an oracle", bodies[0], bodies[1])
	}
}

// wrapNotFound makes any error match errors.Is(err, sdk.ErrNotFound), mirroring
// what store.ErrRideJoinCodeNotFound does to all three dead-code cases.
func wrapNotFound(err error) error { return joinNotFoundError{err} }

type joinNotFoundError struct{ error }

func (joinNotFoundError) Is(target error) bool { return errors.Is(target, sdk.ErrNotFound) }

// TestRideJoin_NormalizesTheCode pins the copy-paste tolerance: the client's
// entry field upper-cases and strips, and so does the server, so a code that
// arrived through a chat app with a stray space still redeems.
func TestRideJoin_NormalizesTheCode(t *testing.T) {
	store := &fakeRideStore{joinRec: groupRideData(rideStatusAccepted), joinCreated: true}
	h := newRideHandler(store, &stubVehicleSnapshotReader{}, nil, groupJoinerID)

	if res := postJoin(h, `{"code":" rbo-246 "}`); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", res.Code, res.Body.String())
	}
	if store.joinCode != groupCode {
		t.Errorf("store saw code %q, want %q", store.joinCode, groupCode)
	}
	if store.joinUserID != groupJoinerID {
		t.Errorf("store saw user %q, want the JWT subject %q", store.joinUserID, groupJoinerID)
	}
}

// TestRideJoin_IdempotentReJoin pins the contract's retry promise: the same
// account submitting the same code twice gets the same 200 and the same ride,
// and only the FIRST one is announced on the bus.
func TestRideJoin_IdempotentReJoin(t *testing.T) {
	joined := groupRideData(rideStatusAccepted)
	joined.Members = []RideMemberData{{UserID: groupJoinerID, FirstName: "Sam"}}

	first := &fakeRidePublisher{}
	h := newRideHandler(&fakeRideStore{joinRec: joined, joinCreated: true}, &stubVehicleSnapshotReader{}, first, groupJoinerID)
	firstRes := postJoin(h, `{"code":"RBO246"}`)

	again := &fakeRidePublisher{}
	h2 := newRideHandler(&fakeRideStore{joinRec: joined, joinCreated: false}, &stubVehicleSnapshotReader{}, again, groupJoinerID)
	secondRes := postJoin(h2, `{"code":"RBO246"}`)

	if firstRes.Code != http.StatusOK || secondRes.Code != http.StatusOK {
		t.Fatalf("statuses = %d, %d; want 200, 200", firstRes.Code, secondRes.Code)
	}
	if firstRes.Body.String() != secondRes.Body.String() {
		t.Errorf("re-join answered a different body:\n first  = %s\n second = %s", firstRes.Body, secondRes.Body)
	}
	if countTopic(first.events, events.TopicRideMemberJoined) != 1 {
		t.Errorf("first join published %d member_joined events, want 1", countTopic(first.events, events.TopicRideMemberJoined))
	}
	if n := countTopic(again.events, events.TopicRideMemberJoined); n != 0 {
		t.Errorf("re-join published %d member_joined events, want 0 — a retry is not news", n)
	}
}

// TestRideJoin_RateLimited pins that the budget is spent per ATTEMPT, whatever
// the outcome: the code space is small enough to enumerate, and counting only
// failures would let an attacker interleave one valid code with guesses.
func TestRideJoin_RateLimited(t *testing.T) {
	store := &fakeRideStore{joinErr: wrapNotFound(sdk.ErrNotFound)}
	h := newRideHandler(store, &stubVehicleSnapshotReader{}, nil, groupJoinerID)

	for i := 0; i < redeemRateLimit; i++ {
		if res := postJoin(h, `{"code":"AAAAAA"}`); res.Code != http.StatusNotFound {
			t.Fatalf("attempt %d: status = %d, want 404", i, res.Code)
		}
	}
	res := postJoin(h, `{"code":"AAAAAA"}`)
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("status after %d attempts = %d, want 429", redeemRateLimit, res.Code)
	}
	if !strings.Contains(res.Body.String(), `"rate_limited"`) {
		t.Errorf("body = %s, want rate_limited", res.Body.String())
	}
}

// TestRideJoin_ResponseCarriesTheJoiner pins the contract's whole reason for
// answering with the ride rather than a bespoke shape: the joiner goes straight
// to the rider tracking surface, so they must already be in `members`.
func TestRideJoin_ResponseCarriesTheJoiner(t *testing.T) {
	joined := groupRideData(rideStatusAccepted)
	joined.Members = []RideMemberData{{UserID: groupJoinerID, FirstName: "Sam"}}
	h := newRideHandler(&fakeRideStore{joinRec: joined, joinCreated: true}, &stubVehicleSnapshotReader{}, nil, groupJoinerID)

	res := postJoin(h, `{"code":"RBO246"}`)
	var got rideRequestWire
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, res.Body.String())
	}
	if got.ID != joined.ID {
		t.Fatalf("id = %q, want the ride they joined", got.ID)
	}
	if len(got.Members) != 1 || got.Members[0].UserID != groupJoinerID {
		t.Fatalf("members = %+v, want the caller present", got.Members)
	}
}

// --- the widened party set ---------------------------------------------------

// stubMemberReader answers the ACL probe.
type stubMemberReader struct {
	member bool
	err    error
	calls  int
}

func (s *stubMemberReader) IsRideMember(_ context.Context, _, _ string) (bool, error) {
	s.calls++
	return s.member, s.err
}

// memberHandler builds a handler whose authenticated caller is a JOINED member
// of the ride the store returns.
func memberHandler(t *testing.T, store RideRequestStore, members *stubMemberReader) *RideRequestHandler {
	t.Helper()
	return NewRideRequestHandler(
		&stubTokenValidator{userID: groupJoinerID},
		&stubVehicleSnapshotReader{},
		store,
		nil,
		discardLogger(),
		WithRideMemberReader(members),
	)
}

// TestRideACL_MemberCanReadAndEditTheTrip is the client's FULL-COLLABORATION
// decision: a member edits the trip exactly as the requester does.
func TestRideACL_MemberCanReadAndEditTheTrip(t *testing.T) {
	rec := groupRideData(rideStatusAccepted)
	store := &fakeRideStore{getRec: rec}
	members := &stubMemberReader{member: true}
	mux := rideMux(memberHandler(t, store, members))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/ride-requests/"+rideID, nil)
	req.Header.Set("Authorization", "Bearer t")
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 — a member holds the full rider view", res.Code)
	}

	body := `{"tripVersion":0,"dropoff":{"lat":37.78,"lng":-122.40,"label":"Ferry Building"}}`
	req = httptest.NewRequestWithContext(context.Background(), http.MethodPatch, "/api/ride-requests/"+rideID+"/trip", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	res = httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200 (%s)", res.Code, res.Body.String())
	}
}

// TestRideACL_MemberCannotCancel is the OTHER half, and the more important one:
// reading and editing widened, ENDING the ride did not. MYR-537 gave the cancel
// to the requester and the owner; a member leaving is not a cancel.
//
// 403 rather than 404, deliberately: this caller can see the ride in their own
// app, so pretending it does not exist would read as a bug.
func TestRideACL_MemberCannotCancel(t *testing.T) {
	rec := groupRideData(rideStatusAccepted)
	store := &fakeRideStore{getRec: rec}
	mux := rideMux(memberHandler(t, store, &stubMemberReader{member: true}))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/ride-requests/"+rideID+"/cancel", nil)
	req.Header.Set("Authorization", "Bearer t")
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if store.updateCalls != 0 {
		t.Fatal("a member's cancel reached the guarded write; the ride would have ended")
	}
}

// TestRideACL_NonMemberStaysA404 pins that the widening did not open the door:
// somebody who is not a party still cannot learn the ride exists.
func TestRideACL_NonMemberStaysA404(t *testing.T) {
	store := &fakeRideStore{getRec: groupRideData(rideStatusAccepted)}
	mux := rideMux(memberHandler(t, store, &stubMemberReader{member: false}))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/ride-requests/"+rideID, nil)
	req.Header.Set("Authorization", "Bearer t")
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — a stranger must not learn the ride exists", res.Code)
	}
}

// TestRideACL_MembershipProbeFailsClosed pins the direction a lookup error
// resolves in. Admitting on an unreadable membership would hand a stranger
// somebody's pickup coordinates on a database blip.
func TestRideACL_MembershipProbeFailsClosed(t *testing.T) {
	store := &fakeRideStore{getRec: groupRideData(rideStatusAccepted)}
	mux := rideMux(memberHandler(t, store, &stubMemberReader{err: errors.New("boom")}))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/ride-requests/"+rideID, nil)
	req.Header.Set("Authorization", "Bearer t")
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 on an unreadable membership", res.Code)
	}
}

// TestRideACL_SoloRideNeverProbes pins the cost argument: the membership lookup
// is skipped entirely unless the ride is a group ride, so every ordinary ride
// costs exactly what it did before MYR-540.
func TestRideACL_SoloRideNeverProbes(t *testing.T) {
	rec := fixtureRideData(groupOwnerID, rideStatusAccepted) // not a group ride
	members := &stubMemberReader{member: true}
	mux := rideMux(memberHandler(t, &fakeRideStore{getRec: rec}, members))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/ride-requests/"+rideID, nil)
	req.Header.Set("Authorization", "Bearer t")
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if members.calls != 0 {
		t.Errorf("the membership probe ran %d time(s) on a solo ride; it must never run", members.calls)
	}
}

// countTopic counts published events on one topic.
func countTopic(evts []events.Event, topic events.Topic) int {
	n := 0
	for _, e := range evts {
		if e.Topic == topic {
			n++
		}
	}
	return n
}
