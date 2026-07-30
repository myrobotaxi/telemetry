package telemetry

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// --- fakes -----------------------------------------------------------------

// fakeGrantRevoker is the network seam (TeslaGrantRevoker). It records the
// token it was handed so the tests can prove the STORED refresh token — not a
// freshly-minted one — is what gets presented to Tesla.
type fakeGrantRevoker struct {
	err    error
	calls  int
	gotTok string
}

func (f *fakeGrantRevoker) Revoke(_ context.Context, refreshToken string) error {
	f.calls++
	f.gotTok = refreshToken
	return f.err
}

// fakeLinkRevoker stands in for *TeslaLinkRevoker at the two handler seams.
// It carries BOTH order-recording shapes the two test files use: a string
// trail (account-deletion tests) and a shared int counter (teardown tests).
type fakeLinkRevoker struct {
	accepted bool

	linkCalls int
	lastCalls int
	gotUser   string
	gotVeh    string

	trail *[]string

	counter *int
	seq     int
}

func (f *fakeLinkRevoker) note() {
	if f.trail != nil {
		*f.trail = append(*f.trail, "revoke_tesla")
	}
	if f.counter != nil {
		*f.counter++
		f.seq = *f.counter
	}
}

func (f *fakeLinkRevoker) RevokeTeslaLink(_ context.Context, userID string) bool {
	f.linkCalls++
	f.gotUser = userID
	f.note()
	return f.accepted
}

func (f *fakeLinkRevoker) RevokeIfLastVehicle(_ context.Context, userID, vehicleID string) bool {
	f.lastCalls++
	f.gotUser = userID
	f.gotVeh = vehicleID
	f.note()
	return f.accepted
}

func newLinkRevoker(tokens TeslaTokenProvider, vehicles AccountOwnedVehicleLister, revoker TeslaGrantRevoker) *TeslaLinkRevoker {
	return NewTeslaLinkRevoker(tokens, vehicles, revoker, discardLogger())
}

//nolint:gosec // test fixture, not a real credential
func storedToken(refresh string) *stubTeslaTokenProvider {
	return &stubTeslaTokenProvider{token: TeslaToken{AccessToken: "at", RefreshToken: refresh}}
}

// --- TeslaLinkRevoker.RevokeTeslaLink --------------------------------------

func TestTeslaLinkRevoker_RevokeTeslaLink(t *testing.T) {
	tests := []struct {
		name        string
		tokens      *stubTeslaTokenProvider
		revokeErr   error
		wantCalls   int
		wantToken   string
		wantAccept  bool
		description string
	}{
		{
			name:        "revokes with the stored refresh token",
			tokens:      storedToken("rt-stored"),
			wantCalls:   1,
			wantToken:   "rt-stored",
			wantAccept:  true,
			description: "the happy path presents the refresh token, which kills the whole grant",
		},
		{
			name:        "no tokens on file is a clean skip",
			tokens:      &stubTeslaTokenProvider{err: errors.New("no rows")},
			wantCalls:   0,
			wantAccept:  false,
			description: "the re-run case: a previous run already deleted the Account row",
		},
		{
			name:        "no refresh token on file is a clean skip",
			tokens:      storedToken(""),
			wantCalls:   0,
			wantAccept:  false,
			description: "an access-token-only row has nothing that can revoke the grant",
		},
		{
			name:        "Tesla failure is non-fatal",
			tokens:      storedToken("rt-stored"),
			revokeErr:   errors.New("tesla 503"),
			wantCalls:   1,
			wantToken:   "rt-stored",
			wantAccept:  false,
			description: "the call was made and it failed; the caller carries on regardless",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			grant := &fakeGrantRevoker{err: tc.revokeErr}
			r := newLinkRevoker(tc.tokens, nil, grant)

			got := r.RevokeTeslaLink(context.Background(), deletionUserID)

			if got != tc.wantAccept {
				t.Errorf("RevokeTeslaLink = %v, want %v (%s)", got, tc.wantAccept, tc.description)
			}
			if grant.calls != tc.wantCalls {
				t.Errorf("Revoke calls = %d, want %d", grant.calls, tc.wantCalls)
			}
			if tc.wantToken != "" && grant.gotTok != tc.wantToken {
				t.Errorf("revoked token = %q, want %q", grant.gotTok, tc.wantToken)
			}
		})
	}
}

// TestTeslaLinkRevoker_UnconfiguredIsSkip proves a half-wired revoker cannot
// panic a deletion — the deployment without Tesla credentials must degrade to
// the pre-MYR-366 behaviour, not to a 500.
func TestTeslaLinkRevoker_UnconfiguredIsSkip(t *testing.T) {
	var nilRevoker *TeslaLinkRevoker
	if nilRevoker.RevokeTeslaLink(context.Background(), deletionUserID) {
		t.Error("a nil revoker reported success")
	}
	if nilRevoker.RevokeIfLastVehicle(context.Background(), deletionUserID, "cveh_1") {
		t.Error("a nil revoker reported success")
	}
	if newLinkRevoker(nil, nil, nil).RevokeTeslaLink(context.Background(), deletionUserID) {
		t.Error("a revoker with no token provider reported success")
	}
}

// --- TeslaLinkRevoker.RevokeIfLastVehicle ----------------------------------

func TestTeslaLinkRevoker_RevokeIfLastVehicle(t *testing.T) {
	const target = "cveh_target"

	tests := []struct {
		name        string
		owned       []string
		listErr     error
		wantCalls   int
		description string
	}{
		{
			name:        "last vehicle revokes",
			owned:       []string{target},
			wantCalls:   1,
			description: "this is the removal that deletes the Account row, so the grant goes with it",
		},
		{
			name:        "mid-fleet removal does NOT revoke",
			owned:       []string{target, "cveh_other"},
			wantCalls:   0,
			description: "the owner keeps a car; revoking would break its telemetry",
		},
		{
			name:        "three cars, still mid-fleet",
			owned:       []string{"cveh_a", target, "cveh_b"},
			wantCalls:   0,
			description: "position in the set is irrelevant — only the count is",
		},
		{
			name:        "car not owned does NOT revoke",
			owned:       []string{"cveh_other"},
			wantCalls:   0,
			description: "a teardown that removes nothing must not revoke anything",
		},
		{
			name:        "no cars at all does NOT revoke",
			owned:       nil,
			wantCalls:   0,
			description: "already-gone: the teardown will be a no-op",
		},
		{
			name:        "lister failure does NOT revoke",
			listErr:     errors.New("db down"),
			wantCalls:   0,
			description: "unknown fleet size: guessing wrong would break the owner's other cars",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			grant := &fakeGrantRevoker{}
			r := newLinkRevoker(
				storedToken("rt-stored"),
				&fakeOwnedVehicleLister{ids: tc.owned, err: tc.listErr},
				grant,
			)

			got := r.RevokeIfLastVehicle(context.Background(), deletionUserID, target)

			if grant.calls != tc.wantCalls {
				t.Errorf("Revoke calls = %d, want %d (%s)", grant.calls, tc.wantCalls, tc.description)
			}
			if want := tc.wantCalls > 0; got != want {
				t.Errorf("RevokeIfLastVehicle = %v, want %v", got, want)
			}
		})
	}
}

// --- Path A: the account-deletion sequence ---------------------------------

// TestAccountDeletion_RevokesTeslaBeforeDeletingTokens is the ordering pin.
// The revoke MUST precede the vehicle teardown (whose last-vehicle arm deletes
// the "Account" row) and the identity delete (whose "User" cascade takes any
// Account row that survived). After either, the refresh token the revoke needs
// no longer exists.
func TestAccountDeletion_RevokesTeslaBeforeDeletingTokens(t *testing.T) {
	var trail []string
	revoker := &fakeLinkRevoker{accepted: true, trail: &trail}
	teardown := newFakeAccountTeardown()
	teardown.order = &trail
	data := &fakeAccountData{order: &trail}

	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles:  &fakeOwnedVehicleLister{ids: []string{"cveh_1", "cveh_2"}},
		TeslaLink: revoker,
		Teardown:  teardown,
		Rides:     &fakeRideCanceller{},
		Data:      data,
	})

	if w := callDelete(h); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}

	if revoker.linkCalls != 1 {
		t.Fatalf("RevokeTeslaLink calls = %d, want 1", revoker.linkCalls)
	}
	if revoker.gotUser != deletionUserID {
		t.Errorf("revoked for user %q, want %q", revoker.gotUser, deletionUserID)
	}

	revokeAt := indexOf(trail, "revoke_tesla")
	if revokeAt < 0 {
		t.Fatalf("revoke never recorded; trail = %v", trail)
	}
	for _, after := range []string{"teardown:cveh_1", "teardown:cveh_2", "delete_identity"} {
		at := indexOf(trail, after)
		if at < 0 {
			t.Fatalf("%q never ran; trail = %v", after, trail)
		}
		if revokeAt > at {
			t.Errorf("revoke ran at %d, AFTER %q at %d — the token is gone by then; trail = %v",
				revokeAt, after, at, trail)
		}
	}
}

// TestAccountDeletion_TeslaRevocationIsNonFatal covers the three ways the
// revoke can come up empty. None of them may change the outcome: the user
// asked us to delete their account, and Tesla does not get a veto.
func TestAccountDeletion_TeslaRevocationIsNonFatal(t *testing.T) {
	tests := []struct {
		name      string
		revoker   *fakeLinkRevoker
		wantCalls int
	}{
		{
			name:      "Tesla refused the revocation",
			revoker:   &fakeLinkRevoker{accepted: false},
			wantCalls: 1,
		},
		{
			name:      "no tokens on file (re-run, or a rider who never linked)",
			revoker:   &fakeLinkRevoker{accepted: false},
			wantCalls: 1,
		},
		{
			name:      "no Tesla OAuth client configured (nil seam)",
			revoker:   nil,
			wantCalls: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := &fakeAccountData{}
			deps := AccountDeletionDeps{
				Vehicles: &fakeOwnedVehicleLister{ids: []string{"cveh_1"}},
				Teardown: newFakeAccountTeardown(),
				Rides:    &fakeRideCanceller{},
				Data:     data,
			}
			if tc.revoker != nil {
				deps.TeslaLink = tc.revoker
			}
			h := newDeletionHandler(t, deps)

			w := callDelete(h)

			if w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 — a Tesla failure blocked the deletion", w.Code)
			}
			if data.identityCalls != 1 {
				t.Errorf("DeleteIdentity calls = %d, want 1 — the sequence did not finish", data.identityCalls)
			}
			if tc.revoker != nil && tc.revoker.linkCalls != tc.wantCalls {
				t.Errorf("RevokeTeslaLink calls = %d, want %d", tc.revoker.linkCalls, tc.wantCalls)
			}
		})
	}
}

// TestAccountDeletion_RevocationIsRerunnable pins the second-run behaviour: a
// re-run finds no tokens, the revoker skips cleanly, and the endpoint is still
// a 204.
func TestAccountDeletion_RevocationIsRerunnable(t *testing.T) {
	grant := &fakeGrantRevoker{}
	// First run has a token; the second models the Account row being gone.
	tokens := storedToken("rt-stored")
	revoker := newLinkRevoker(tokens, nil, grant)

	data := &fakeAccountData{}
	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles:  &fakeOwnedVehicleLister{ids: []string{"cveh_1"}},
		TeslaLink: revoker,
		Teardown:  newFakeAccountTeardown(),
		Rides:     &fakeRideCanceller{},
		Data:      data,
	})

	if w := callDelete(h); w.Code != http.StatusNoContent {
		t.Fatalf("first run status = %d, want 204", w.Code)
	}
	if grant.calls != 1 {
		t.Fatalf("first run Revoke calls = %d, want 1", grant.calls)
	}

	// The first run's deletion took the Account row with it.
	tokens.token = TeslaToken{}
	tokens.err = errors.New("no rows")

	if w := callDelete(h); w.Code != http.StatusNoContent {
		t.Fatalf("second run status = %d, want 204", w.Code)
	}
	if grant.calls != 1 {
		t.Errorf("second run made %d extra Revoke call(s); a re-run must skip cleanly", grant.calls-1)
	}
}

// --- Path B: the single-vehicle teardown -----------------------------------

// TestVehicleTeardown_RevokesOnlyOnLastVehicle covers the branch that decides
// whether the Tesla link survives the removal, and pins the ordering against
// the local teardown that deletes the token.
func TestVehicleTeardown_RevokesOnlyOnLastVehicle(t *testing.T) {
	tests := []struct {
		name        string
		wasLast     bool
		accepted    bool
		wantCalls   int
		description string
	}{
		{
			name:        "last vehicle revokes the grant",
			wasLast:     true,
			accepted:    true,
			wantCalls:   1,
			description: "this teardown deletes the Account row, so the grant must go first",
		},
		{
			name:        "mid-fleet removal keeps the link",
			wasLast:     false,
			wantCalls:   1,
			description: "the seam is still consulted; it is the revoker that declines",
		},
		{
			name:        "Tesla refusing does not fail the teardown",
			wasLast:     true,
			accepted:    false,
			wantCalls:   1,
			description: "best-effort: the local teardown is authoritative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			order := 0
			revoker := &fakeLinkRevoker{accepted: tc.accepted, counter: &order}
			writer := &fakeTeardownWriter{
				result: VehicleTeardownResult{
					Removed:            true,
					WasLastVehicle:     tc.wasLast,
					TeslaTokensCleared: tc.wasLast,
				},
				order: &order,
			}

			h := NewVehicleTeardownHandler(
				&stubTokenValidator{userID: teardownUserID},
				&stubVehicleSnapshotReader{row: ownedTeardownRow()},
				validResolver(),
				nil, // no fleet client: the stream-config delete is out of scope here
				writer,
				VehicleTeardownConfig{RevokeClientID: teardownClientID},
				discardLogger(),
				WithTeslaGrantRevocation(revoker),
			)

			rec := doTeardown(t, h, http.MethodDelete, true)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, tc.description)
			}
			if revoker.lastCalls != tc.wantCalls {
				t.Errorf("RevokeIfLastVehicle calls = %d, want %d", revoker.lastCalls, tc.wantCalls)
			}
			if revoker.gotVeh != teardownVehicleID {
				t.Errorf("revoke asked about vehicle %q, want %q", revoker.gotVeh, teardownVehicleID)
			}
			if !writer.called {
				t.Fatal("the authoritative local teardown did not run")
			}
			if revoker.seq > writer.seq {
				t.Errorf("revoke ran at %d, AFTER the teardown at %d — the token is deleted by then",
					revoker.seq, writer.seq)
			}
		})
	}
}

// TestVehicleTeardown_NoRevokerConfigured pins the pre-MYR-366 behaviour for a
// deployment with no Tesla OAuth client: the teardown runs, unchanged, and
// nothing reaches out to Tesla.
func TestVehicleTeardown_NoRevokerConfigured(t *testing.T) {
	writer := &fakeTeardownWriter{result: VehicleTeardownResult{Removed: true, WasLastVehicle: true}}
	h := newTeardownHandler(
		&stubVehicleSnapshotReader{row: ownedTeardownRow()},
		validResolver(),
		nil,
		writer,
	)

	if rec := doTeardown(t, h, http.MethodDelete, true); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !writer.called {
		t.Error("teardown did not run without a revoker wired")
	}
}

// indexOf returns the position of want in trail, or -1.
func indexOf(trail []string, want string) int {
	for i, s := range trail {
		if s == want {
			return i
		}
	}
	return -1
}
