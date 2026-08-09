package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

const setupStateVIN = "5YJSA1E26MF000491"

// setupScheduleFixture seeds one owned, quiet vehicle and returns a repo.
// Deliberately reuses the fleet-candidate seeders so this test's vehicle is
// shaped exactly like the ones the reconciler actually operates on.
func setupScheduleFixture(t *testing.T, owner string) *store.VehicleRepo {
	t.Helper()
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	mustApplyOwnerSchema(t)
	cleanTables(t, testPool)
	cleanFleetCandidateTables(t)
	if _, err := testPool.Exec(context.Background(), `DELETE FROM go_fleet_config_attempts`); err != nil {
		t.Fatalf("clean go_fleet_config_attempts: %v", err)
	}
	seedFCCAccount(t, "acct_setup", owner, "tesla-sub-setup")
	seedFleetCandidateVehicle(t, "veh_setup", owner, setupStateVIN, "tv_setup", time.Now().Add(-3*time.Hour))
	return store.NewVehicleRepo(testPool, store.NoopMetrics{})
}

// The reads are the contract: a car with no schedule row must project ABSENCE,
// not a zero-valued row, because "no row" is the signal that means "make no
// claim". Getting this wrong at the SQL layer would put a setup card on every
// healthy vehicle in the fleet.
func TestVehicleReadsProjectTheSetupSchedule(t *testing.T) {
	const owner = "user_setup"
	repo := setupScheduleFixture(t, owner)
	ctx := context.Background()

	t.Run("no schedule row projects absence on the snapshot", func(t *testing.T) {
		v, err := repo.GetByID(ctx, "veh_setup")
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if v.SetupSchedule.Present {
			t.Fatalf("SetupSchedule = %+v, want Present false", v.SetupSchedule)
		}
	})

	t.Run("no schedule row projects absence on the catalog", func(t *testing.T) {
		rows, err := repo.ListSummariesByUser(ctx, owner)
		if err != nil {
			t.Fatalf("ListSummariesByUser: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("len(rows) = %d, want 1", len(rows))
		}
		if rows[0].SetupSchedule.Present {
			t.Fatalf("SetupSchedule = %+v, want Present false", rows[0].SetupSchedule)
		}
	})

	attemptedAt := time.Now().Add(-90 * time.Minute).UTC().Truncate(time.Millisecond)
	if err := repo.RecordFleetConfigAttempt(ctx, "veh_setup",
		attemptedAt, time.Now().Add(time.Hour), store.SetupOutcomeAwaitingVirtualKey); err != nil {
		t.Fatalf("RecordFleetConfigAttempt: %v", err)
	}

	t.Run("a real row reaches the snapshot with its outcome and timestamp", func(t *testing.T) {
		v, err := repo.GetByID(ctx, "veh_setup")
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		s := v.SetupSchedule
		if !s.Present || s.LastOutcome != store.SetupOutcomeAwaitingVirtualKey {
			t.Fatalf("SetupSchedule = %+v, want Present with outcome %q", s, store.SetupOutcomeAwaitingVirtualKey)
		}
		if !s.LastAttemptAt.UTC().Truncate(time.Millisecond).Equal(attemptedAt) {
			t.Errorf("LastAttemptAt = %v, want %v", s.LastAttemptAt.UTC(), attemptedAt)
		}
		// Never observed, and that must arrive as the zero Time rather than as
		// a scan error or a fabricated instant.
		if !s.SignedCommandAt.IsZero() || !s.ForcedRepushAt.IsZero() {
			t.Errorf("epoch stamps = %v / %v, want both zero", s.SignedCommandAt, s.ForcedRepushAt)
		}
	})

	t.Run("the same row reaches the catalog row", func(t *testing.T) {
		rows, err := repo.ListSummariesByUser(ctx, owner)
		if err != nil {
			t.Fatalf("ListSummariesByUser: %v", err)
		}
		if len(rows) != 1 || !rows[0].SetupSchedule.Present {
			t.Fatalf("rows = %+v, want one row carrying the schedule", rows)
		}
		if rows[0].SetupSchedule.LastOutcome != store.SetupOutcomeAwaitingVirtualKey {
			t.Errorf("LastOutcome = %q, want %q",
				rows[0].SetupSchedule.LastOutcome, store.SetupOutcomeAwaitingVirtualKey)
		}
	})

	t.Run("the epoch stamps survive the join", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		if _, _, err := repo.ResetFleetConfigScheduleOnPairing(ctx, setupStateVIN, now, now); err != nil {
			t.Fatalf("ResetFleetConfigScheduleOnPairing: %v", err)
		}
		v, err := repo.GetByID(ctx, "veh_setup")
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if !v.SetupSchedule.SignedCommandAt.UTC().Truncate(time.Millisecond).Equal(now) {
			t.Errorf("SignedCommandAt = %v, want %v", v.SetupSchedule.SignedCommandAt.UTC(), now)
		}
	})
}

// The link-time seed is what makes the card render for a brand-new owner
// instead of after the reconciler's first pass, and its whole design constraint
// is that it must be invisible to MYR-489's scheduling.
func TestSeedFleetConfigAwaitingKey(t *testing.T) {
	const owner = "user_seed"
	repo := setupScheduleFixture(t, owner)
	ctx := context.Background()
	prov := newTestProvisioner(t)

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := prov.SeedFleetConfigAwaitingKey(ctx, setupStateVIN, now); err != nil {
		t.Fatalf("SeedFleetConfigAwaitingKey: %v", err)
	}

	t.Run("the seeded outcome round-trips to the snapshot read", func(t *testing.T) {
		v, err := repo.GetByID(ctx, "veh_setup")
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		s := v.SetupSchedule
		if !s.Present || s.LastOutcome != store.SetupOutcomeAwaitingVirtualKey {
			t.Fatalf("SetupSchedule = %+v, want the awaiting-key seed", s)
		}
	})

	// THE INVARIANT THAT PROTECTS MYR-489. A seeded row must leave the
	// reconciler's schedule exactly as it found it: due now, zero attempts.
	t.Run("the seed is scheduling-neutral", func(t *testing.T) {
		var count int
		var next time.Time
		err := testPool.QueryRow(ctx,
			`SELECT attempt_count, next_attempt_at FROM go_fleet_config_attempts WHERE vehicle_id = $1`,
			"veh_setup").Scan(&count, &next)
		if err != nil {
			t.Fatalf("read schedule: %v", err)
		}
		if count != 0 {
			t.Errorf("attempt_count = %d, want 0 — a seed must not spend a retry", count)
		}
		if next.After(now) {
			t.Errorf("next_attempt_at = %v, want no later than %v — a seed must not delay the first attempt", next, now)
		}

		rows, err := repo.ListFleetConfigCandidates(ctx, time.Now().Add(-30*time.Minute), time.Now(), 100)
		if err != nil {
			t.Fatalf("ListFleetConfigCandidates: %v", err)
		}
		if len(rows) != 1 || rows[0].VehicleID != "veh_setup" {
			t.Fatalf("candidates = %+v, want the seeded car still due immediately", rows)
		}
	})

	// A re-link must never clobber a live schedule — that would disarm a
	// pending escalation and reset a hard-earned backoff.
	t.Run("a second seed leaves an existing schedule untouched", func(t *testing.T) {
		attemptedAt := time.Now().Add(-10 * time.Minute)
		if err := repo.RecordFleetConfigAttempt(ctx, "veh_setup",
			attemptedAt, time.Now().Add(8*time.Hour), "synced_not_streaming"); err != nil {
			t.Fatalf("RecordFleetConfigAttempt: %v", err)
		}

		if err := prov.SeedFleetConfigAwaitingKey(ctx, setupStateVIN, time.Now()); err != nil {
			t.Fatalf("re-seed: %v", err)
		}

		var outcome string
		var count int
		err := testPool.QueryRow(ctx,
			`SELECT last_outcome, attempt_count FROM go_fleet_config_attempts WHERE vehicle_id = $1`,
			"veh_setup").Scan(&outcome, &count)
		if err != nil {
			t.Fatalf("read schedule: %v", err)
		}
		if outcome != "synced_not_streaming" {
			t.Errorf("last_outcome = %q, want synced_not_streaming preserved — a re-seed must not "+
				"overwrite the outcome the escalation gate reads", outcome)
		}
		if count != 1 {
			t.Errorf("attempt_count = %d, want 1 preserved — a re-seed must not reset the backoff", count)
		}
	})

	t.Run("an unknown VIN is a silent no-op", func(t *testing.T) {
		if err := prov.SeedFleetConfigAwaitingKey(ctx, "5YJSA1E26MF999999", time.Now()); err != nil {
			t.Fatalf("SeedFleetConfigAwaitingKey(unknown VIN) = %v, want nil", err)
		}
	})
}
