package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

func newTestPushDeviceRepo() *store.PushDeviceRepo {
	return store.NewPushDeviceRepo(testPool, testLogger())
}

func cleanPushDevices(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM go_push_devices`); err != nil {
		t.Fatalf("clean go_push_devices: %v", err)
	}
}

// setupPushDevices prepares the schema and an empty table for a registry test.
func setupPushDevices(t *testing.T) *store.PushDeviceRepo {
	t.Helper()
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	mustApplyGoMigrations(t)
	cleanPushDevices(t)
	return newTestPushDeviceRepo()
}

// tokensFor returns just the device tokens registered to a person.
func tokensFor(t *testing.T, repo *store.PushDeviceRepo, userID string) []string {
	t.Helper()
	devices, err := repo.DevicesForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("DevicesForUser(%s): %v", userID, err)
	}
	out := make([]string, 0, len(devices))
	for _, d := range devices {
		out = append(out, d.DeviceToken)
	}
	return out
}

// TestPushDeviceRepo_RegisterIsIdempotent covers the plain re-registration an
// app performs on every launch: the same token must not accumulate rows.
func TestPushDeviceRepo_RegisterIsIdempotent(t *testing.T) {
	repo := setupPushDevices(t)
	ctx := context.Background()
	const uid, token = "cpushuser001", "token-aaaa-0001"

	for range 3 {
		if err := repo.RegisterDevice(ctx, uid, token, false); err != nil {
			t.Fatalf("RegisterDevice: %v", err)
		}
	}

	got := tokensFor(t, repo, uid)
	if len(got) != 1 {
		t.Fatalf("devices = %v, want exactly one row after three registrations", got)
	}
}

// TestPushDeviceRepo_RegisterReparents is the phone-handover case: a token that
// moves to a new signed-in person must stop reaching the previous one. Two rows
// would mean the old occupant keeps receiving the new occupant's ride pushes.
func TestPushDeviceRepo_RegisterReparents(t *testing.T) {
	repo := setupPushDevices(t)
	ctx := context.Background()
	const first, second, token = "cpushuser002", "cpushuser003", "token-bbbb-0002"

	if err := repo.RegisterDevice(ctx, first, token, false); err != nil {
		t.Fatalf("RegisterDevice(first): %v", err)
	}
	if err := repo.RegisterDevice(ctx, second, token, true); err != nil {
		t.Fatalf("RegisterDevice(second): %v", err)
	}

	if got := tokensFor(t, repo, first); len(got) != 0 {
		t.Errorf("previous owner devices = %v, want none (row must re-parent)", got)
	}
	devices, err := repo.DevicesForUser(ctx, second)
	if err != nil {
		t.Fatalf("DevicesForUser(second): %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("new owner devices = %d, want 1", len(devices))
	}
	if !devices[0].Sandbox {
		t.Error("sandbox = false, want true (re-registration must refresh the gateway flag)")
	}
}

// TestPushDeviceRepo_UnregisterIsCallerScoped proves the cross-user isolation
// of sign-out: the endpoint takes only a token, so without the user_id
// predicate anyone could unregister anyone else's phone.
func TestPushDeviceRepo_UnregisterIsCallerScoped(t *testing.T) {
	repo := setupPushDevices(t)
	ctx := context.Background()
	const owner, stranger, token = "cpushuser004", "cpushuser005", "token-cccc-0003"

	if err := repo.RegisterDevice(ctx, owner, token, false); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	tests := []struct {
		name        string
		callerID    string
		wantDeleted bool
		wantRemains int
	}{
		{name: "stranger cannot unregister", callerID: stranger, wantRemains: 1},
		{name: "owner can unregister", callerID: owner, wantDeleted: true},
		{name: "second unregister is a clean no-op", callerID: owner},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deleted, err := repo.UnregisterDevice(ctx, tt.callerID, token)
			if err != nil {
				t.Fatalf("UnregisterDevice: %v", err)
			}
			if deleted != tt.wantDeleted {
				t.Errorf("deleted = %v, want %v", deleted, tt.wantDeleted)
			}
			if got := tokensFor(t, repo, owner); len(got) != tt.wantRemains {
				t.Errorf("owner devices = %v, want %d remaining", got, tt.wantRemains)
			}
		})
	}
}

// TestPushDeviceRepo_DeleteDeviceTokenIgnoresOwnership pins the APNs-feedback
// path: a 410 Unregistered is a verdict on the installation, so the row goes
// whoever currently owns it.
func TestPushDeviceRepo_DeleteDeviceTokenIgnoresOwnership(t *testing.T) {
	repo := setupPushDevices(t)
	ctx := context.Background()
	const uid, token = "cpushuser006", "token-dddd-0004"

	if err := repo.RegisterDevice(ctx, uid, token, false); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	if err := repo.DeleteDeviceToken(ctx, token); err != nil {
		t.Fatalf("DeleteDeviceToken: %v", err)
	}
	if got := tokensFor(t, repo, uid); len(got) != 0 {
		t.Errorf("devices = %v, want none after the APNs 410 delete", got)
	}
	// Idempotent: a second gateway rejection for a token already gone is fine.
	if err := repo.DeleteDeviceToken(ctx, token); err != nil {
		t.Errorf("second DeleteDeviceToken: %v", err)
	}
}

// TestPushDeviceRepo_DevicesForUserIsolation checks the audience fan-out only
// ever returns the requested person's phones — the read that decides who a
// notification reaches.
func TestPushDeviceRepo_DevicesForUserIsolation(t *testing.T) {
	repo := setupPushDevices(t)
	ctx := context.Background()
	const alice, bob = "cpushuser007", "cpushuser008"

	for _, token := range []string{"token-eeee-0005", "token-eeee-0006"} {
		if err := repo.RegisterDevice(ctx, alice, token, false); err != nil {
			t.Fatalf("RegisterDevice(alice): %v", err)
		}
	}
	if err := repo.RegisterDevice(ctx, bob, "token-ffff-0007", true); err != nil {
		t.Fatalf("RegisterDevice(bob): %v", err)
	}

	if got := tokensFor(t, repo, alice); len(got) != 2 {
		t.Errorf("alice devices = %v, want 2", got)
	}
	got := tokensFor(t, repo, bob)
	if len(got) != 1 || got[0] != "token-ffff-0007" {
		t.Errorf("bob devices = %v, want exactly his own token", got)
	}
	if empty := tokensFor(t, repo, "cpushuser-nobody"); len(empty) != 0 {
		t.Errorf("unknown user devices = %v, want empty (not an error)", empty)
	}
}

// TestPushDeviceRepo_RejectsEmptyInputs guards the argument validation; the
// error strings must never echo the P1 token value.
func TestPushDeviceRepo_RejectsEmptyInputs(t *testing.T) {
	repo := setupPushDevices(t)
	ctx := context.Background()

	// A distinctive sentinel so the leak assertion below cannot match an
	// ordinary word in the error text.
	const sentinel = "zqSENTINELqz"

	tests := []struct {
		name string
		run  func() error
	}{
		{name: "register empty user", run: func() error { return repo.RegisterDevice(ctx, "", sentinel, false) }},
		{name: "register empty token", run: func() error { return repo.RegisterDevice(ctx, "u", "  ", false) }},
		{name: "unregister empty user", run: func() error {
			_, err := repo.UnregisterDevice(ctx, "", sentinel)
			return err
		}},
		{name: "unregister empty token", run: func() error {
			_, err := repo.UnregisterDevice(ctx, "u", "")
			return err
		}},
		{name: "delete empty token", run: func() error { return repo.DeleteDeviceToken(ctx, "") }},
		{name: "list empty user", run: func() error {
			_, err := repo.DevicesForUser(ctx, "")
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			if err == nil {
				t.Fatal("error = nil, want a validation error")
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Errorf("error %q echoes the P1 device token", err)
			}
		})
	}
}
