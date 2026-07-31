package auth

import (
	"context"
	"errors"
	"testing"
)

// stubShareLookup resolves accepted grants from a fixed map keyed
// "userID|vehicleID". A missing key is a denial, matching the DB-backed
// implementation's ErrNoVehicleAccess.
type stubShareLookup struct {
	grants map[string]ShareGrant
	err    error
}

func (s *stubShareLookup) GetShareGrant(_ context.Context, userID, vehicleID string) (ShareGrant, error) {
	if s.err != nil {
		return ShareGrant{}, s.err
	}
	if g, ok := s.grants[userID+"|"+vehicleID]; ok {
		return g, nil
	}
	return ShareGrant{}, ErrNoVehicleAccess
}

// TestShareGrant_Capabilities pins the MYR-369 capability model that replaced
// the cumulative tier. The suspension term appears in EVERY row, because the
// invariant under test is that no flag survives it.
func TestShareGrant_Capabilities(t *testing.T) {
	tests := []struct {
		name        string
		grant       ShareGrant
		wantActive  bool
		wantRides   bool
		wantHistory bool
		wantDerived SharePermission
	}{
		{"base grant is active and rides nothing", ShareGrant{}, true, false, false, PermissionLive},
		{"ride grant is active and rides", ShareGrant{AllowRides: true}, true, true, false, PermissionRides},
		// SUSPENSION OUT-VOTES EVERY CAPABILITY. This is the whole point of
		// routing gates through the methods rather than the bare fields.
		{"suspended base grant conveys nothing", ShareGrant{Suspended: true}, false, false, false, PermissionLive},
		{"suspended RIDE grant still conveys no rides", ShareGrant{AllowRides: true, Suspended: true}, false, false, false, PermissionRides},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.grant.Active(); got != tt.wantActive {
				t.Errorf("Active() = %v, want %v", got, tt.wantActive)
			}
			if got := tt.grant.GrantsRides(); got != tt.wantRides {
				t.Errorf("GrantsRides() = %v, want %v", got, tt.wantRides)
			}
			// MYR-369 retired the history capability outright: no grant
			// of any shape opens the drives surfaces.
			if got := tt.grant.GrantsHistory(); got != tt.wantHistory {
				t.Errorf("GrantsHistory() = %v, want %v", got, tt.wantHistory)
			}
			// Permission() is derived output and is deliberately total —
			// it reports what the grant WOULD convey. Callers must not
			// serialize it for a suspended grant, which is why the
			// suspended rows above still derive a value.
			if got := tt.grant.Permission(); got != tt.wantDerived {
				t.Errorf("Permission() = %q, want %q", got, tt.wantDerived)
			}
		})
	}
}

// TestGrantForPreset was deleted with GrantForPreset itself (MYR-369): the
// function had no caller, and the preset → flag mapping it restated is enforced
// in SQL. That mapping is pinned against the real statement by
// store.TestVehicleShareRepo_TierAtRedeem, including the retired live_history
// tier — which is where a test of it belongs, since that is where a divergence
// would actually change what a redeemed grant conveys.

// TestNormalizeInvitePermission pins the create-time collapse: the retired tier
// is accepted from a client and never persisted.
func TestNormalizeInvitePermission(t *testing.T) {
	if got := NormalizeInvitePermission(PermissionLiveHistory); got != PermissionLive {
		t.Errorf("live_history normalized to %q, want %q", got, PermissionLive)
	}
	for _, keep := range []SharePermission{PermissionLive, PermissionRides, SharePermission("weird")} {
		if got := NormalizeInvitePermission(keep); got != keep {
			t.Errorf("NormalizeInvitePermission(%q) = %q, want it unchanged", keep, got)
		}
	}
}

func TestParseSharePermission(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    SharePermission
		wantErr bool
	}{
		{"live", "live", PermissionLive, false},
		// STILL ACCEPTED despite being retired: rejecting it would break
		// invite creation for every un-updated client. The write path
		// normalizes it; the parser does not.
		{"retired live_history is still accepted as input", "live_history", PermissionLiveHistory, false},
		{"rides", "rides", PermissionRides, false},
		{"empty is rejected (fail-closed sentinel)", "", SharePermission(""), true},
		{"unknown is rejected", "owner", SharePermission(""), true},
		{"case sensitive", "LIVE", SharePermission(""), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSharePermission(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tt.in, got)
				}
				if !errors.Is(err, ErrUnknownSharePermission) {
					t.Errorf("error = %v, want ErrUnknownSharePermission", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJWTAuthenticator_ResolveVehicleAccess(t *testing.T) {
	const (
		caller  = "user-caller"
		other   = "user-other"
		vehicle = "vehicle-1"
	)

	tests := []struct {
		name       string
		ownerByID  map[string]string
		grants     map[string]ShareGrant
		shareErr   error
		noShareDep bool
		wantRole   Role
		wantGrant  ShareGrant
		wantDenied bool
		wantErr    bool
	}{
		{
			// An owner holds no grant; the ZERO ShareGrant is returned,
			// which is the most restrictive value, so a caller that
			// forgets to branch on the role under-grants.
			name:      "owner resolves owner with the zero grant",
			ownerByID: map[string]string{vehicle: caller},
			wantRole:  RoleOwner,
			wantGrant: ShareGrant{},
		},
		{
			name:      "accepted share resolves viewer with its flags",
			ownerByID: map[string]string{vehicle: other},
			grants:    map[string]ShareGrant{caller + "|" + vehicle: {AllowRides: true}},
			wantRole:  RoleViewer,
			wantGrant: ShareGrant{AllowRides: true},
		},
		{
			name:       "non-owner without a share is denied",
			ownerByID:  map[string]string{vehicle: other},
			grants:     map[string]ShareGrant{},
			wantDenied: true,
		},
		{
			// MYR-369 SECOND GATE. The DB statement already excludes
			// suspended rows, so a lookup that returns one models a stub,
			// a future implementation, or an edited WHERE clause. It must
			// be DENIED — not returned as a viewer with an empty
			// capability set, which would put the caller inside every
			// handler's viewer branch.
			name:       "suspended grant is denied, not a capability-less viewer",
			ownerByID:  map[string]string{vehicle: other},
			grants:     map[string]ShareGrant{caller + "|" + vehicle: {AllowRides: true, Suspended: true}},
			wantDenied: true,
		},
		{
			// The grant is for somebody else's account — holding a grant
			// on the same vehicle must not leak across users.
			name:       "share belonging to another user does not grant access",
			ownerByID:  map[string]string{vehicle: other},
			grants:     map[string]ShareGrant{"user-third|" + vehicle: {AllowRides: true}},
			wantDenied: true,
		},
		{
			name:       "no share lookup configured fails closed",
			ownerByID:  map[string]string{vehicle: other},
			noShareDep: true,
			wantDenied: true,
		},
		{
			name:      "unknown vehicle surfaces an error, not a denial",
			ownerByID: map[string]string{},
			wantErr:   true,
		},
		{
			// A transient share-lookup failure must NOT read as "denied":
			// the handler layer distinguishes 403 from 500, and silently
			// downgrading an outage to a denial hides it.
			name:      "share lookup outage surfaces an error",
			ownerByID: map[string]string{vehicle: other},
			shareErr:  errors.New("connection refused"),
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			querier := &stubQuerier{ownerByID: tt.ownerByID}
			a := &JWTAuthenticator{
				secret:      []byte(testSecret),
				cache:       newVehicleCache(querier, vehicleCacheTTL),
				ownerLookup: querier,
			}
			if !tt.noShareDep {
				a.shares = &stubShareLookup{grants: tt.grants, err: tt.shareErr}
			}

			role, grant, err := a.ResolveVehicleAccess(context.Background(), caller, vehicle)

			switch {
			case tt.wantDenied:
				if !errors.Is(err, ErrNoVehicleAccess) {
					t.Fatalf("err = %v, want ErrNoVehicleAccess", err)
				}
			case tt.wantErr:
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				if errors.Is(err, ErrNoVehicleAccess) {
					t.Fatal("outage/unknown-vehicle must not surface as a denial")
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}

			if tt.wantDenied || tt.wantErr {
				if role != Role("") || grant != (ShareGrant{}) {
					t.Errorf("expected zero role/grant on failure, got (%q, %+v)", role, grant)
				}
				return
			}
			if role != tt.wantRole {
				t.Errorf("role = %q, want %q", role, tt.wantRole)
			}
			if grant != tt.wantGrant {
				t.Errorf("grant = %+v, want %+v", grant, tt.wantGrant)
			}
		})
	}
}

// TestJWTAuthenticator_InvalidateVehicles pins the redeem/revoke cache bust:
// without it a freshly-redeemed car stays invisible, and a revoked viewer
// keeps resolving it, for the whole cache TTL.
func TestJWTAuthenticator_InvalidateVehicles(t *testing.T) {
	const user = "user-1"
	querier := &stubQuerier{ids: []string{"vehicle-a"}}
	a := &JWTAuthenticator{
		secret:      []byte(testSecret),
		cache:       newVehicleCache(querier, vehicleCacheTTL),
		ownerLookup: querier,
	}
	ctx := context.Background()

	if _, err := a.GetUserVehicles(ctx, user); err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	// The access set widens behind the cache (a share was just redeemed).
	querier.ids = []string{"vehicle-a", "vehicle-shared"}

	cached, err := a.GetUserVehicles(ctx, user)
	if err != nil {
		t.Fatalf("cached lookup: %v", err)
	}
	if len(cached) != 1 {
		t.Fatalf("expected the stale cached set of 1, got %v", cached)
	}

	a.InvalidateVehicles(user)

	fresh, err := a.GetUserVehicles(ctx, user)
	if err != nil {
		t.Fatalf("post-invalidate lookup: %v", err)
	}
	if len(fresh) != 2 {
		t.Errorf("after invalidate got %v, want the widened set of 2", fresh)
	}

	// Idempotent / defensive: unknown user and empty id are no-ops.
	a.InvalidateVehicles("nobody")
	a.InvalidateVehicles("")
}
