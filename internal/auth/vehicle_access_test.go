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
	grants map[string]SharePermission
	err    error
}

func (s *stubShareLookup) GetSharePermission(_ context.Context, userID, vehicleID string) (SharePermission, error) {
	if s.err != nil {
		return SharePermission(""), s.err
	}
	if p, ok := s.grants[userID+"|"+vehicleID]; ok {
		return p, nil
	}
	return SharePermission(""), ErrNoVehicleAccess
}

func TestSharePermission_AtLeast(t *testing.T) {
	tests := []struct {
		name     string
		have     SharePermission
		required SharePermission
		want     bool
	}{
		{"live satisfies live", PermissionLive, PermissionLive, true},
		{"live does not satisfy live_history", PermissionLive, PermissionLiveHistory, false},
		{"live does not satisfy rides", PermissionLive, PermissionRides, false},
		{"live_history satisfies live (cumulative)", PermissionLiveHistory, PermissionLive, true},
		{"live_history satisfies live_history", PermissionLiveHistory, PermissionLiveHistory, true},
		{"live_history does not satisfy rides", PermissionLiveHistory, PermissionRides, false},
		{"rides satisfies live (cumulative)", PermissionRides, PermissionLive, true},
		{"rides satisfies live_history (cumulative)", PermissionRides, PermissionLiveHistory, true},
		{"rides satisfies rides", PermissionRides, PermissionRides, true},
		// Fail-closed edges: an unknown tier on either side opens nothing.
		{"empty held tier satisfies nothing", SharePermission(""), PermissionLive, false},
		{"unknown held tier satisfies nothing", SharePermission("admin"), PermissionLive, false},
		{"unknown required tier is never satisfied", PermissionRides, SharePermission("admin"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.have.AtLeast(tt.required); got != tt.want {
				t.Errorf("%q.AtLeast(%q) = %v, want %v", tt.have, tt.required, got, tt.want)
			}
		})
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
		{"live_history", "live_history", PermissionLiveHistory, false},
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
		grants     map[string]SharePermission
		shareErr   error
		noShareDep bool
		wantRole   Role
		wantTier   SharePermission
		wantDenied bool
		wantErr    bool
	}{
		{
			name:      "owner resolves owner with no tier",
			ownerByID: map[string]string{vehicle: caller},
			wantRole:  RoleOwner,
			wantTier:  SharePermission(""),
		},
		{
			name:      "accepted share resolves viewer with its tier",
			ownerByID: map[string]string{vehicle: other},
			grants:    map[string]SharePermission{caller + "|" + vehicle: PermissionLiveHistory},
			wantRole:  RoleViewer,
			wantTier:  PermissionLiveHistory,
		},
		{
			name:       "non-owner without a share is denied",
			ownerByID:  map[string]string{vehicle: other},
			grants:     map[string]SharePermission{},
			wantDenied: true,
		},
		{
			// The grant is for somebody else's account — holding a grant
			// on the same vehicle must not leak across users.
			name:       "share belonging to another user does not grant access",
			ownerByID:  map[string]string{vehicle: other},
			grants:     map[string]SharePermission{"user-third|" + vehicle: PermissionRides},
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

			role, tier, err := a.ResolveVehicleAccess(context.Background(), caller, vehicle)

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
				if role != Role("") || tier != SharePermission("") {
					t.Errorf("expected zero role/tier on failure, got (%q, %q)", role, tier)
				}
				return
			}
			if role != tt.wantRole {
				t.Errorf("role = %q, want %q", role, tt.wantRole)
			}
			if tier != tt.wantTier {
				t.Errorf("tier = %q, want %q", tier, tt.wantTier)
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
