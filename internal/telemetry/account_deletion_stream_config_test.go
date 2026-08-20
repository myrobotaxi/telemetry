package telemetry

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// MYR-593. Deleting an account used to remove its cars' rows and leave their
// Tesla-side fleet-telemetry configs running — a pure cost leak with no way
// back, because the config outlives the account by up to its 350-day `exp`, the
// MYR-592 inactivity sweeper joins from a Vehicle row that no longer exists, and
// the owner who could revoke it by hand no longer has an account.
//
// These tests pin the three properties the fix has to hold: the delete is
// issued per owned VIN, it is issued BEFORE anything destroys the token that
// authenticates it, and no answer from Tesla can stop the deletion.

// --- fakes -----------------------------------------------------------------

// streamConfigCall is one (owner, VIN) pair the sequence asked to sever.
type streamConfigCall struct {
	userID string
	vin    string
}

// fakeStreamConfigDeleter stands in for *StreamConfigTeardown at the
// account-deletion seam. It records every call and can refuse a nominated VIN,
// which is how the "Tesla said no" arms are built.
type fakeStreamConfigDeleter struct {
	// refuse names the VINs Tesla rejects. Everything else succeeds.
	refuse map[string]bool
	// refuseAll models a total outage: nothing gets through.
	refuseAll bool

	calls []streamConfigCall
	trail *[]string
}

func (f *fakeStreamConfigDeleter) DeleteStreamConfig(_ context.Context, userID, vin string) bool {
	f.calls = append(f.calls, streamConfigCall{userID: userID, vin: vin})
	if f.trail != nil {
		*f.trail = append(*f.trail, "stream_config:"+vin)
	}
	return !f.refuseAll && !f.refuse[vin]
}

func (f *fakeStreamConfigDeleter) vins() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.vin)
	}
	return out
}

// --- ordering: the whole point ---------------------------------------------

// TestAccountDeletion_DeletesStreamConfigsBeforeTheTokensGo is THE guard.
//
// Three later things destroy what the Tesla config delete authenticates with,
// and the call has to precede all of them: the OAuth grant revoke (after which
// the access token is dead at Tesla however fresh our copy looks), the
// per-vehicle teardown (whose last-vehicle arm DELETEs the "Account" row the
// token lives in), and the identity transaction (whose "User" cascade takes any
// Account row that survived).
//
// Against the pre-MYR-593 sequence this test fails at the first assertion —
// there were no stream_config entries in the trail at all.
func TestAccountDeletion_DeletesStreamConfigsBeforeTheTokensGo(t *testing.T) {
	var trail []string
	configs := &fakeStreamConfigDeleter{trail: &trail}
	revoker := &fakeLinkRevoker{accepted: true, trail: &trail}
	teardown := newFakeAccountTeardown()
	teardown.order = &trail
	data := &fakeAccountData{order: &trail}

	vehicles := &fakeOwnedVehicleLister{
		ids:  []string{"cveh_1", "cveh_2"},
		vins: map[string]string{"cveh_1": "5YJ3E1EA1PF000001", "cveh_2": "5YJ3E1EA1PF000002"},
	}

	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles:      vehicles,
		StreamConfigs: configs,
		TeslaLink:     revoker,
		Teardown:      teardown,
		Rides:         &fakeRideCanceller{},
		Data:          data,
	})

	if w := callDelete(h); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}

	// (a) One delete per owned VIN, keyed on the owner whose token pays for it.
	wantVINs := []string{"5YJ3E1EA1PF000001", "5YJ3E1EA1PF000002"}
	if got := configs.vins(); len(got) != 2 || got[0] != wantVINs[0] || got[1] != wantVINs[1] {
		t.Fatalf("stream-config deletes = %v, want one per owned VIN %v", got, wantVINs)
	}
	for _, c := range configs.calls {
		if c.userID != deletionUserID {
			t.Errorf("stream-config delete for VIN %s ran as %q, want the owner %q",
				c.vin, c.userID, deletionUserID)
		}
	}

	// (b) Every one of them before every token-destroying step.
	for _, vin := range wantVINs {
		at := indexOf(trail, "stream_config:"+vin)
		if at < 0 {
			t.Fatalf("no stream-config delete for %s; trail = %v", vin, trail)
		}
		for _, after := range []string{"revoke_tesla", "teardown:cveh_1", "teardown:cveh_2", "delete_identity"} {
			other := indexOf(trail, after)
			if other < 0 {
				t.Fatalf("%q never ran; trail = %v", after, trail)
			}
			if at > other {
				t.Errorf("stream-config delete for %s ran at %d, AFTER %q at %d — the token that "+
					"authenticates it is gone by then and the config bills on; trail = %v",
					vin, at, after, other, trail)
			}
		}
	}
}

// TestAccountDeletion_ReadsTheFleetOnce pins the single-read property the two
// steps depend on. Listing twice — once to sever, once to tear down — would let
// a car that appeared between the reads be torn down with its config still
// running, or a car that vanished be severed and never removed.
func TestAccountDeletion_ReadsTheFleetOnce(t *testing.T) {
	vehicles := &fakeOwnedVehicleLister{ids: []string{"cveh_1"}}
	configs := &fakeStreamConfigDeleter{}
	teardown := newFakeAccountTeardown()

	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles:      vehicles,
		StreamConfigs: configs,
		Teardown:      teardown,
		Rides:         &fakeRideCanceller{},
		Data:          &fakeAccountData{},
	})

	if w := callDelete(h); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if vehicles.calls != 1 {
		t.Errorf("owned-vehicle reads = %d, want 1 — the config delete and the teardown must work "+
			"from ONE view of the fleet", vehicles.calls)
	}
	if len(configs.calls) != 1 || len(teardown.seen) != 1 {
		t.Errorf("severed %d and tore down %d, want 1 and 1", len(configs.calls), len(teardown.seen))
	}
}

// --- best-effort: Tesla does not get a veto --------------------------------

// TestAccountDeletion_StreamConfigFailureDoesNotAbort covers every way the
// Tesla-side delete can come up empty. The person asked us to delete their
// account; an unreachable car, a dead token or an outage may cost us money but
// must not cost them their deletion.
func TestAccountDeletion_StreamConfigFailureDoesNotAbort(t *testing.T) {
	const vinA, vinB = "5YJ3E1EA1PF000001", "5YJ3E1EA1PF000002"

	tests := []struct {
		name    string
		configs *fakeStreamConfigDeleter
		// wantAttempts is how many VINs the sequence tried, nil deleter aside.
		wantAttempts int
	}{
		{
			name:         "Tesla refused one car (unreachable, or a 404 for a config never applied)",
			configs:      &fakeStreamConfigDeleter{refuse: map[string]bool{vinA: true}},
			wantAttempts: 2,
		},
		{
			name:         "Tesla refused every car (outage, or the token was already dead)",
			configs:      &fakeStreamConfigDeleter{refuseAll: true},
			wantAttempts: 2,
		},
		{
			name:         "no deleter wired (no tesla-http-proxy configured)",
			configs:      nil,
			wantAttempts: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			teardown := newFakeAccountTeardown()
			data := &fakeAccountData{}
			deps := AccountDeletionDeps{
				Vehicles: &fakeOwnedVehicleLister{
					ids:  []string{"cveh_1", "cveh_2"},
					vins: map[string]string{"cveh_1": vinA, "cveh_2": vinB},
				},
				Teardown: teardown,
				Rides:    &fakeRideCanceller{},
				Data:     data,
			}
			// Assigned inside the guard: a typed nil in the interface would
			// satisfy the handler's nil check and defeat the third case.
			if tc.configs != nil {
				deps.StreamConfigs = tc.configs
			}

			h := newDeletionHandler(t, deps)

			if w := callDelete(h); w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 — a Tesla-side failure must never fail the deletion", w.Code)
			}

			// The deletion finished in full: both cars torn down, identity gone.
			if len(teardown.seen) != 2 {
				t.Errorf("vehicles torn down = %v, want both", teardown.seen)
			}
			if data.identityCalls != 1 {
				t.Errorf("identity transactions = %d, want 1", data.identityCalls)
			}
			if got := data.identityCounts.VehicleCount; got != 2 {
				t.Errorf("audit vehicle count = %d, want 2", got)
			}

			if tc.configs != nil && len(tc.configs.calls) != tc.wantAttempts {
				t.Errorf("stream-config attempts = %d, want %d — one refusal must not stop the next car",
					len(tc.configs.calls), tc.wantAttempts)
			}
		})
	}
}

// TestAccountDeletion_VINlessCarIsStillTornDown covers the nullable column. A
// car linked but never synced has no VIN and therefore no Tesla-side config;
// the shared step skips it, and the row teardown must run regardless.
func TestAccountDeletion_VINlessCarIsStillTornDown(t *testing.T) {
	configs := &fakeStreamConfigDeleter{}
	teardown := newFakeAccountTeardown()

	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles: &fakeOwnedVehicleLister{
			ids:  []string{"cveh_novin", "cveh_real"},
			vins: map[string]string{"cveh_novin": "", "cveh_real": "5YJ3E1EA1PF000009"},
		},
		StreamConfigs: configs,
		Teardown:      teardown,
		Rides:         &fakeRideCanceller{},
		Data:          &fakeAccountData{},
	})

	if w := callDelete(h); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if len(teardown.seen) != 2 {
		t.Errorf("vehicles torn down = %v, want both — an empty VIN blocks the Tesla call, not the row",
			teardown.seen)
	}
	// The empty VIN still reaches the shared step, which is where the skip
	// lives — one place, so both severing paths make the same decision.
	if got := configs.vins(); len(got) != 2 || got[0] != "" {
		t.Errorf("stream-config calls = %v, want both VINs offered to the shared step", got)
	}
}

// TestAccountDeletion_ListFailureStopsBeforeAnythingIsDestroyed pins the one
// fatal arm of the new read. A fleet we could not enumerate is a fleet we
// cannot prove we finished, so nothing may be severed, torn down or erased.
func TestAccountDeletion_ListFailureStopsBeforeAnythingIsDestroyed(t *testing.T) {
	configs := &fakeStreamConfigDeleter{}
	teardown := newFakeAccountTeardown()
	data := &fakeAccountData{}

	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles:      &fakeOwnedVehicleLister{err: errors.New("vehicle read failed")},
		StreamConfigs: configs,
		Teardown:      teardown,
		Rides:         &fakeRideCanceller{},
		Data:          data,
	})

	if w := callDelete(h); w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if len(configs.calls) != 0 {
		t.Errorf("severed %v on a fleet we could not read", configs.vins())
	}
	if len(teardown.seen) != 0 {
		t.Errorf("tore down %v on a fleet we could not read", teardown.seen)
	}
	if data.identityCalls != 0 {
		t.Error("identity transaction ran on a fleet we could not read")
	}
}

// --- the shared step itself -------------------------------------------------

// TestStreamConfigTeardown_EveryFailureIsASkip pins the type both severing
// paths hold. Extracting it is what stops "remove a vehicle" meaning two
// different things, so its behaviour is pinned once, here, rather than twice.
func TestStreamConfigTeardown_EveryFailureIsASkip(t *testing.T) {
	const vin = "5YJ3E1EA1PF000001"

	tests := []struct {
		name     string
		fleet    *fakeConfigDeleter
		resolver teslaTokenResolver
		vin      string
		want     bool
		wantCall bool
	}{
		{
			name:     "Tesla accepted",
			fleet:    &fakeConfigDeleter{},
			resolver: validResolver(),
			vin:      vin,
			want:     true,
			wantCall: true,
		},
		{
			name:     "Tesla refused",
			fleet:    &fakeConfigDeleter{err: errors.New("502 bad gateway")},
			resolver: validResolver(),
			vin:      vin,
			wantCall: true,
		},
		{
			name:     "no token on file",
			fleet:    &fakeConfigDeleter{},
			resolver: &fakeTokenResolver{err: errors.New("no tesla account")},
			vin:      vin,
		},
		{
			name:     "empty VIN (a car linked but never synced)",
			fleet:    &fakeConfigDeleter{},
			resolver: validResolver(),
			vin:      "",
		},
		{
			name:     "no deleter wired",
			fleet:    nil,
			resolver: validResolver(),
			vin:      vin,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Assigned inside the guard so a typed nil never reaches the
			// interface — the exact trap the nil check is there to catch.
			var deleter FleetConfigDeleter
			if tc.fleet != nil {
				deleter = tc.fleet
			}
			step := NewStreamConfigTeardown(deleter, tc.resolver, discardLogger())

			if got := step.DeleteStreamConfig(context.Background(), "cuser_1", tc.vin); got != tc.want {
				t.Errorf("DeleteStreamConfig = %v, want %v", got, tc.want)
			}
			if tc.fleet != nil && tc.fleet.called != tc.wantCall {
				t.Errorf("Tesla call made = %v, want %v", tc.fleet.called, tc.wantCall)
			}
			if tc.wantCall && tc.fleet.gotVIN != tc.vin {
				t.Errorf("deleted config for VIN %q, want %q", tc.fleet.gotVIN, tc.vin)
			}
		})
	}
}

// TestStreamConfigTeardown_NoTokenSourceIsASkip covers the other half of the
// unwired case: a deleter with nothing to authenticate it. Both halves are
// required and neither substitutes for the other.
func TestStreamConfigTeardown_NoTokenSourceIsASkip(t *testing.T) {
	fleet := &fakeConfigDeleter{}
	step := NewStreamConfigTeardown(fleet, nil, discardLogger())

	if step.DeleteStreamConfig(context.Background(), "cuser_1", "5YJ3E1EA1PF000001") {
		t.Error("reported success with no token source wired")
	}
	if fleet.called {
		t.Error("called Tesla with no token source wired")
	}
}
