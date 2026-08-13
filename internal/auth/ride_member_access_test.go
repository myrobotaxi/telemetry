package auth

import (
	"context"
	"errors"
	"testing"
)

// MYR-540 — GROUP-RIDE MEMBERSHIP as the third source of vehicle access.
//
// The SQL half (the access-set UNION leg and its status predicate) is pinned by
// the store's integration tests; what is pinned here is the resolution: which
// TIER a member gets, where membership sits in the precedence order, and which
// way each failure resolves.

// stubRideMembership answers the per-vehicle membership probe.
type stubRideMembership struct {
	riding bool
	err    error
	calls  int
}

func (s *stubRideMembership) IsRidingVehicle(context.Context, string, string) (bool, error) {
	s.calls++
	return s.riding, s.err
}

func TestResolveVehicleAccess_RideMembership(t *testing.T) {
	const (
		caller  = "cjoiner-540"
		owner   = "cowner-540"
		vehicle = "cveh-540"
	)

	tests := []struct {
		name string
		// grants seeds the share lookup; empty means "no accepted share".
		grants     map[string]ShareGrant
		shareErr   error
		noRideDep  bool
		riding     bool
		ridingErr  error
		wantRole   Role
		wantRides  bool
		wantDenied bool
		wantProbes int
	}{
		{
			// The admission this issue exists for: no share at all, but the
			// caller is riding in the car right now.
			name:       "a live member with no share resolves as a viewer",
			riding:     true,
			wantRole:   RoleViewer,
			wantProbes: 1,
		},
		{
			// THE TIER. A member holds the ZERO-VALUE capability set, so riding
			// along never becomes permission to summon the car.
			name:       "a member's grant carries no ride capability",
			riding:     true,
			wantRole:   RoleViewer,
			wantRides:  false,
			wantProbes: 1,
		},
		{
			// The ride ended (or there never was one). The statement's own
			// status predicate is what makes this the answer; here it is the
			// stub, and the point is that a false probe is a DENIAL and not a
			// viewer with an empty capability set.
			name:       "no live membership is a denial",
			riding:     false,
			wantDenied: true,
			wantProbes: 1,
		},
		{
			// PRECEDENCE. Somebody who holds a real `rides` share on a car they
			// also happen to be riding in must keep the grant they actually
			// hold, not be downgraded to the bare membership tier for the
			// length of one ride — so the probe must not even run.
			name:       "an accepted share wins and the membership probe never runs",
			grants:     map[string]ShareGrant{caller + "|" + vehicle: {AllowRides: true}},
			riding:     true,
			wantRole:   RoleViewer,
			wantRides:  true,
			wantProbes: 0,
		},
		{
			// FAILS CLOSED, as the caller's own denial rather than as an error:
			// this probe only runs on a path that was already going to deny, so
			// a database blip must not turn a 403 into a 500.
			name:       "an unreadable membership denies rather than erroring",
			ridingErr:  errors.New("boom"),
			wantDenied: true,
			wantProbes: 1,
		},
		{
			// Unwired is the pre-MYR-540 behaviour, unchanged.
			name:       "no membership lookup configured denies as before",
			noRideDep:  true,
			wantDenied: true,
			wantProbes: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			querier := &stubQuerier{ownerByID: map[string]string{vehicle: owner}}
			rides := &stubRideMembership{riding: tt.riding, err: tt.ridingErr}
			a := &JWTAuthenticator{
				secret:      []byte(testSecret),
				cache:       newVehicleCache(querier, vehicleCacheTTL),
				ownerLookup: querier,
				shares:      &stubShareLookup{grants: tt.grants, err: tt.shareErr},
			}
			if !tt.noRideDep {
				a.rides = rides
			}

			role, grant, err := a.ResolveVehicleAccess(context.Background(), caller, vehicle)

			if tt.wantDenied {
				if !errors.Is(err, ErrNoVehicleAccess) {
					t.Fatalf("err = %v, want ErrNoVehicleAccess", err)
				}
				if role != Role("") {
					t.Errorf("role = %q, want the empty deny-all sentinel", role)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if role != tt.wantRole {
					t.Errorf("role = %q, want %q", role, tt.wantRole)
				}
				if grant.GrantsRides() != tt.wantRides {
					t.Errorf("GrantsRides() = %v, want %v", grant.GrantsRides(), tt.wantRides)
				}
			}
			if rides.calls != tt.wantProbes {
				t.Errorf("membership probe ran %d time(s), want %d", rides.calls, tt.wantProbes)
			}
		})
	}
}

// TestResolveVehicleAccess_OwnerNeverProbesMembership pins the cheapest half of
// the precedence rule: the owner short-circuits before any lookup at all, so the
// overwhelmingly common resolution costs exactly what it did before MYR-540.
func TestResolveVehicleAccess_OwnerNeverProbesMembership(t *testing.T) {
	const owner, vehicle = "cowner-540", "cveh-540"
	querier := &stubQuerier{ownerByID: map[string]string{vehicle: owner}}
	rides := &stubRideMembership{riding: true}
	a := &JWTAuthenticator{
		secret:      []byte(testSecret),
		cache:       newVehicleCache(querier, vehicleCacheTTL),
		ownerLookup: querier,
		shares:      &stubShareLookup{},
		rides:       rides,
	}

	role, _, err := a.ResolveVehicleAccess(context.Background(), owner, vehicle)
	if err != nil || role != RoleOwner {
		t.Fatalf("role = %q, err = %v; want owner, nil", role, err)
	}
	if rides.calls != 0 {
		t.Errorf("the membership probe ran %d time(s) for the owner; it must never run", rides.calls)
	}
}
