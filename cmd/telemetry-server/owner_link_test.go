package main

import (
	"context"
	"errors"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/teslaauth"
)

type fakeProvisioner struct {
	calls     int
	last      store.ProvisionInput
	canonical string
	err       error
}

func (f *fakeProvisioner) ProvisionTeslaOwner(_ context.Context, in store.ProvisionInput) (store.ProvisionResult, error) {
	f.calls++
	f.last = in
	if f.err != nil {
		return store.ProvisionResult{}, f.err
	}
	canon := f.canonical
	if canon == "" {
		canon = in.UserID
	}
	return store.ProvisionResult{CanonicalUserID: canon, Outcome: store.OutcomeNewUser}, nil
}

type fakeProfiles struct {
	name, email string
	err         error
}

func (f *fakeProfiles) GetUserProfile(_ context.Context, _ string) (string, string, error) {
	return f.name, f.email, f.err
}

type fakeHook struct {
	calls      int
	lastUserID string
}

func (f *fakeHook) AfterLink(_ context.Context, userID, _ string) {
	f.calls++
	f.lastUserID = userID
}

func TestOwnerLink_UpdateTeslaToken(t *testing.T) {
	ctx := context.Background()

	t.Run("success provisions with tesla sub and fires hook", func(t *testing.T) {
		prov := &fakeProvisioner{}
		hook := &fakeHook{}
		link := &ownerLink{
			provisioner: prov,
			profiles:    &fakeProfiles{name: "Ada", email: "ada@apple.example"},
			fetchUserInfo: func(context.Context, string) (teslaauth.UserInfo, error) {
				return teslaauth.UserInfo{Sub: "tsub-9", Email: "ada@tesla.example"}, nil
			},
			hook:   hook,
			logger: testLogger(),
		}
		if err := link.UpdateTeslaToken(ctx, "cuser1", "acc", "ref", 123); err != nil {
			t.Fatalf("UpdateTeslaToken: %v", err)
		}
		if prov.calls != 1 {
			t.Fatalf("provision calls = %d, want 1", prov.calls)
		}
		if prov.last.ProviderAccountID != "tsub-9" {
			t.Errorf("providerAccountID = %q, want tsub-9", prov.last.ProviderAccountID)
		}
		if prov.last.UserID != "cuser1" || prov.last.AccessToken != "acc" || prov.last.ExpiresAt != 123 {
			t.Errorf("unexpected provision input: %+v", prov.last)
		}
		if prov.last.Email != "ada@apple.example" {
			t.Errorf("email = %q, want apple profile email preferred", prov.last.Email)
		}
		if hook.calls != 1 {
			t.Errorf("hook calls = %d, want 1", hook.calls)
		}
	})

	t.Run("converged link passes CANONICAL user to the stream hook, not caller", func(t *testing.T) {
		prov := &fakeProvisioner{canonical: "cowner-A"} // link converged onto A
		hook := &fakeHook{}
		link := &ownerLink{
			provisioner:   prov,
			profiles:      &fakeProfiles{},
			fetchUserInfo: func(context.Context, string) (teslaauth.UserInfo, error) { return teslaauth.UserInfo{Sub: "s"}, nil },
			hook:          hook,
			logger:        testLogger(),
		}
		if err := link.UpdateTeslaToken(ctx, "ccaller-B", "acc", "ref", 1); err != nil {
			t.Fatalf("UpdateTeslaToken: %v", err)
		}
		if hook.lastUserID != "cowner-A" {
			t.Errorf("hook userID = %q, want canonical cowner-A (not caller ccaller-B)", hook.lastUserID)
		}
	})

	t.Run("userinfo failure does not provision (no orphan) and does not fire hook", func(t *testing.T) {
		prov := &fakeProvisioner{}
		hook := &fakeHook{}
		link := &ownerLink{
			provisioner: prov,
			profiles:    &fakeProfiles{},
			fetchUserInfo: func(context.Context, string) (teslaauth.UserInfo, error) {
				return teslaauth.UserInfo{}, errors.New("userinfo 401")
			},
			hook:   hook,
			logger: testLogger(),
		}
		if err := link.UpdateTeslaToken(ctx, "cuser2", "acc", "ref", 1); err == nil {
			t.Fatal("expected error, got nil")
		}
		if prov.calls != 0 {
			t.Errorf("provision calls = %d, want 0 (no orphan before proof of ownership)", prov.calls)
		}
		if hook.calls != 0 {
			t.Errorf("hook calls = %d, want 0", hook.calls)
		}
	})

	t.Run("provision failure returns error and does not fire hook", func(t *testing.T) {
		prov := &fakeProvisioner{err: errors.New("db down")}
		hook := &fakeHook{}
		link := &ownerLink{
			provisioner:   prov,
			profiles:      &fakeProfiles{},
			fetchUserInfo: func(context.Context, string) (teslaauth.UserInfo, error) { return teslaauth.UserInfo{Sub: "s"}, nil },
			hook:          hook,
			logger:        testLogger(),
		}
		if err := link.UpdateTeslaToken(ctx, "cuser3", "acc", "ref", 1); err == nil {
			t.Fatal("expected error, got nil")
		}
		if hook.calls != 0 {
			t.Errorf("hook calls = %d, want 0", hook.calls)
		}
	})

	t.Run("empty Apple email is NOT replaced by the Tesla email (no unverified merge)", func(t *testing.T) {
		prov := &fakeProvisioner{}
		link := &ownerLink{
			provisioner: prov,
			profiles:    &fakeProfiles{name: "Ada", email: ""}, // Apple hidden relay
			fetchUserInfo: func(context.Context, string) (teslaauth.UserInfo, error) {
				return teslaauth.UserInfo{Sub: "s", Email: "tesla@tesla.example"}, nil
			},
			hook:   nil, // nil hook must be safe
			logger: testLogger(),
		}
		if err := link.UpdateTeslaToken(ctx, "cuser4", "acc", "ref", 1); err != nil {
			t.Fatalf("UpdateTeslaToken: %v", err)
		}
		if prov.last.Email != "" {
			t.Errorf("email = %q, want empty (Tesla email must never be used as the match/persist anchor)", prov.last.Email)
		}
	})
}
