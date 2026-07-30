package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-349 — go_push_prefs.
//
// The assertions worth having here are about the THREE shapes of "no answer"
// the read path has to collapse into one: no row at all, a row whose column was
// defaulted, and a key omitted from a partial write. All three must be
// indistinguishable from "on", because on the day this ships literally every
// account is in the first of them.

func newTestPushPrefsRepo() *store.PushPrefsRepo {
	return store.NewPushPrefsRepo(testPool, testLogger())
}

func cleanPushPrefs(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM go_push_prefs`); err != nil {
		t.Fatalf("clean go_push_prefs: %v", err)
	}
}

func setupPushPrefs(t *testing.T) *store.PushPrefsRepo {
	t.Helper()
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	mustApplyGoMigrations(t)
	cleanPushPrefs(t)
	return newTestPushPrefsRepo()
}

func boolptr(b bool) *bool { return &b }

// TestPushPrefs_MissingRowReadsAsAllOn is the migration-day guarantee, at the
// storage layer. Nobody has a row; everybody must still be reachable.
func TestPushPrefs_MissingRowReadsAsAllOn(t *testing.T) {
	repo := setupPushPrefs(t)

	got, err := repo.PrefsForUser(context.Background(), "cuser-never-wrote")
	if err != nil {
		t.Fatalf("PrefsForUser: %v — a missing row is the ORDINARY state, not an error", err)
	}
	if got != store.DefaultPushPrefs() {
		t.Errorf("PrefsForUser = %+v, want %+v — an account with no row must read as "+
			"everything on, or deploying MYR-349 silences the whole platform", got, store.DefaultPushPrefs())
	}
}

// TestPushPrefs_FirstWriteCreatesTheRowAndDefaultsTheRest covers the INSERT arm
// of the upsert: switching ONE category off for somebody who has never written
// must leave the other four ON, not at the zero value.
func TestPushPrefs_FirstWriteCreatesTheRowAndDefaultsTheRest(t *testing.T) {
	repo := setupPushPrefs(t)
	const user = "cuser-first-write"

	got, err := repo.UpdatePrefs(context.Background(), user, store.PushPrefsUpdate{
		ChargingComplete: boolptr(false),
	})
	if err != nil {
		t.Fatalf("UpdatePrefs: %v", err)
	}

	want := store.DefaultPushPrefs()
	want.ChargingComplete = false
	if got != want {
		t.Errorf("UpdatePrefs echo = %+v, want %+v — the INSERT arm must default every "+
			"OMITTED category to true, not to the Go zero value", got, want)
	}

	// And the echo must be what is actually stored.
	read, err := repo.PrefsForUser(context.Background(), user)
	if err != nil {
		t.Fatalf("PrefsForUser: %v", err)
	}
	if read != want {
		t.Errorf("stored = %+v, want %+v — the echo disagreed with the row", read, want)
	}
}

// TestPushPrefs_PartialUpdateLeavesOtherCategoriesAlone covers the UPDATE arm.
// This is the one that protects against two phones clobbering each other.
func TestPushPrefs_PartialUpdateLeavesOtherCategoriesAlone(t *testing.T) {
	repo := setupPushPrefs(t)
	const user = "cuser-partial"
	ctx := context.Background()

	if _, err := repo.UpdatePrefs(ctx, user, store.PushPrefsUpdate{
		DriveStarted: boolptr(false),
	}); err != nil {
		t.Fatalf("first UpdatePrefs: %v", err)
	}

	// A second write, from a different screen, touching a different category.
	got, err := repo.UpdatePrefs(ctx, user, store.PushPrefsUpdate{
		ViewerJoined: boolptr(false),
	})
	if err != nil {
		t.Fatalf("second UpdatePrefs: %v", err)
	}

	want := store.DefaultPushPrefs()
	want.DriveStarted = false
	want.ViewerJoined = false
	if got != want {
		t.Errorf("after two partial writes = %+v, want %+v — the second write resurrected "+
			"a category the first one switched off", got, want)
	}
}

// TestPushPrefs_TurningACategoryBackOnWorks — `true` is as real a value as
// `false`, and a COALESCE that treated it as "unset" would make a switch
// one-way.
func TestPushPrefs_TurningACategoryBackOnWorks(t *testing.T) {
	repo := setupPushPrefs(t)
	const user = "cuser-toggle-back"
	ctx := context.Background()

	if _, err := repo.UpdatePrefs(ctx, user, store.PushPrefsUpdate{RideLifecycle: boolptr(false)}); err != nil {
		t.Fatalf("switch off: %v", err)
	}
	got, err := repo.UpdatePrefs(ctx, user, store.PushPrefsUpdate{RideLifecycle: boolptr(true)})
	if err != nil {
		t.Fatalf("switch back on: %v", err)
	}
	if !got.RideLifecycle {
		t.Error("rideLifecycle stayed off after being explicitly set back to true — the " +
			"switch is one-way")
	}
}

// TestPushPrefs_EveryCategoryRoundTripsIndependently is the per-category
// matrix. Five near-identical boolean columns is exactly the shape where a
// copy-paste in the SQL writes the wrong one, and no single-category test would
// see it.
func TestPushPrefs_EveryCategoryRoundTripsIndependently(t *testing.T) {
	repo := setupPushPrefs(t)
	ctx := context.Background()

	tests := []struct {
		name   string
		update store.PushPrefsUpdate
		want   func(p *store.PushPrefs)
	}{
		{
			name:   "ride_lifecycle",
			update: store.PushPrefsUpdate{RideLifecycle: boolptr(false)},
			want:   func(p *store.PushPrefs) { p.RideLifecycle = false },
		},
		{
			name:   "drive_started",
			update: store.PushPrefsUpdate{DriveStarted: boolptr(false)},
			want:   func(p *store.PushPrefs) { p.DriveStarted = false },
		},
		{
			name:   "drive_completed",
			update: store.PushPrefsUpdate{DriveCompleted: boolptr(false)},
			want:   func(p *store.PushPrefs) { p.DriveCompleted = false },
		},
		{
			name:   "charging_complete",
			update: store.PushPrefsUpdate{ChargingComplete: boolptr(false)},
			want:   func(p *store.PushPrefs) { p.ChargingComplete = false },
		},
		{
			name:   "viewer_joined",
			update: store.PushPrefsUpdate{ViewerJoined: boolptr(false)},
			want:   func(p *store.PushPrefs) { p.ViewerJoined = false },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := "cuser-matrix-" + tt.name
			if _, err := repo.UpdatePrefs(ctx, user, tt.update); err != nil {
				t.Fatalf("UpdatePrefs: %v", err)
			}
			got, err := repo.PrefsForUser(ctx, user)
			if err != nil {
				t.Fatalf("PrefsForUser: %v", err)
			}

			want := store.DefaultPushPrefs()
			tt.want(&want)
			if got != want {
				t.Errorf("stored = %+v, want %+v — switching off %q changed the wrong column",
					got, want, tt.name)
			}
		})
	}
}

// TestPushPrefs_EmptyUpdateIsANoOpReadAfterWrite — §7.19's `{}` case.
func TestPushPrefs_EmptyUpdateIsANoOpReadAfterWrite(t *testing.T) {
	repo := setupPushPrefs(t)
	const user = "cuser-noop"
	ctx := context.Background()

	if _, err := repo.UpdatePrefs(ctx, user, store.PushPrefsUpdate{DriveCompleted: boolptr(false)}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := repo.UpdatePrefs(ctx, user, store.PushPrefsUpdate{})
	if err != nil {
		t.Fatalf("empty UpdatePrefs: %v", err)
	}
	want := store.DefaultPushPrefs()
	want.DriveCompleted = false
	if got != want {
		t.Errorf("empty update = %+v, want %+v — an all-nil update must change nothing", got, want)
	}
}

// TestPushPrefs_PrefsAreScopedToTheUser — one person's switches must never be
// visible in, or writable from, another's row.
func TestPushPrefs_PrefsAreScopedToTheUser(t *testing.T) {
	repo := setupPushPrefs(t)
	ctx := context.Background()

	if _, err := repo.UpdatePrefs(ctx, "cuser-a", store.PushPrefsUpdate{RideLifecycle: boolptr(false)}); err != nil {
		t.Fatalf("write for A: %v", err)
	}

	got, err := repo.PrefsForUser(ctx, "cuser-b")
	if err != nil {
		t.Fatalf("PrefsForUser(B): %v", err)
	}
	if got != store.DefaultPushPrefs() {
		t.Errorf("B reads %+v, want the all-on default — A's preferences leaked", got)
	}
}

// TestPushPrefs_EmptyUserIDIsRejected — an empty subject would collapse every
// anonymous caller onto one shared row.
func TestPushPrefs_EmptyUserIDIsRejected(t *testing.T) {
	repo := setupPushPrefs(t)
	ctx := context.Background()

	if _, err := repo.PrefsForUser(ctx, "  "); err == nil {
		t.Error("PrefsForUser(blank) returned no error")
	}
	if _, err := repo.UpdatePrefs(ctx, "", store.PushPrefsUpdate{}); err == nil {
		t.Error("UpdatePrefs(blank) returned no error")
	}
}
