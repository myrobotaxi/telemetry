package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

const pairingVIN = "7SAYGDED5TA736164"

// fccPairingFixture sets up one quiet, owned vehicle and returns the repo.
func fccPairingFixture(t *testing.T, owner string, quietSince time.Time) *store.VehicleRepo {
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

	seedFCCAccount(t, "acct_pair", owner, "tesla-sub-pair")
	seedFleetCandidateVehicle(t, "veh_pair", owner, pairingVIN, "tv_pair", quietSince)
	return store.NewVehicleRepo(testPool, store.NoopMetrics{})
}

// TestResetFleetConfigScheduleOnPairing is hole 2 at the SQL layer: an applied
// signed command must wipe a backoff that was earned entirely before the
// virtual key existed, and hand the caller the candidate to act on.
func TestResetFleetConfigScheduleOnPairing(t *testing.T) {
	const owner = "user_pair"
	repo := fccPairingFixture(t, owner, time.Now().Add(-3*time.Hour))
	ctx := context.Background()

	// Six pre-key attempts: exactly the state Nabil's car reached, which put
	// his next attempt eight hours out.
	deep := time.Now().Add(-time.Hour)
	for i := 0; i < 6; i++ {
		if err := repo.RecordFleetConfigAttempt(ctx, "veh_pair", deep, time.Now().Add(8*time.Hour), "awaiting_virtual_key"); err != nil {
			t.Fatalf("RecordFleetConfigAttempt: %v", err)
		}
	}

	t.Run("not due before the reset", func(t *testing.T) {
		rows, err := repo.ListFleetConfigCandidates(ctx, time.Now().Add(-30*time.Minute), time.Now(), 100)
		if err != nil {
			t.Fatalf("ListFleetConfigCandidates: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("len = %d, want 0 — the car is buried under its backoff", len(rows))
		}
	})

	now := time.Now()
	c, found, err := repo.ResetFleetConfigScheduleOnPairing(ctx, pairingVIN, now, now.Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("ResetFleetConfigScheduleOnPairing: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true — the car has a schedule row to reset")
	}

	t.Run("returns the candidate the caller must reconcile", func(t *testing.T) {
		if c.VehicleID != "veh_pair" || c.VIN != pairingVIN || c.UserID != owner {
			t.Errorf("candidate = %+v, want veh_pair/%s/%s", c, pairingVIN, owner)
		}
		if c.AttemptCount != 0 {
			t.Errorf("AttemptCount = %d, want 0", c.AttemptCount)
		}
		if c.LastOutcome != "awaiting_virtual_key" {
			t.Errorf("LastOutcome = %q, want awaiting_virtual_key", c.LastOutcome)
		}
		if c.SignedCommandAt.IsZero() {
			t.Error("SignedCommandAt is zero; the pairing epoch was not opened")
		}
	})

	t.Run("the car is immediately due again", func(t *testing.T) {
		rows, err := repo.ListFleetConfigCandidates(ctx, time.Now().Add(-30*time.Minute), time.Now(), 100)
		if err != nil {
			t.Fatalf("ListFleetConfigCandidates: %v", err)
		}
		if len(rows) != 1 || rows[0].VehicleID != "veh_pair" {
			t.Fatalf("candidates = %+v, want just veh_pair", rows)
		}
		if rows[0].AttemptCount != 0 {
			t.Errorf("AttemptCount = %d, want 0 — the pre-key attempts must not count", rows[0].AttemptCount)
		}
		if rows[0].SignedCommandAt.IsZero() {
			t.Error("SignedCommandAt did not survive to the candidate read")
		}
	})

	t.Run("last_attempt_at is not advanced by a command", func(t *testing.T) {
		// It records when we last asked TESLA about this car; the escalation
		// gate measures quiet time from it, so a command must not reset it.
		rows, err := repo.ListFleetConfigCandidates(ctx, time.Now().Add(-30*time.Minute), time.Now(), 100)
		if err != nil {
			t.Fatalf("ListFleetConfigCandidates: %v", err)
		}
		if got := rows[0].LastAttemptAt; got.After(now.Add(-time.Minute)) {
			t.Errorf("LastAttemptAt = %v, want it left at the old attempt time (~%v)", got, deep)
		}
	})

	t.Run("a burst is debounced", func(t *testing.T) {
		_, found, err := repo.ResetFleetConfigScheduleOnPairing(
			ctx, pairingVIN, time.Now(), time.Now().Add(-10*time.Minute))
		if err != nil {
			t.Fatalf("ResetFleetConfigScheduleOnPairing: %v", err)
		}
		if found {
			t.Error("found = true on an immediate repeat; a command burst would fire a reconcile per tap")
		}
	})
}

// TestResetFleetConfigScheduleOnPairing_NoRow: a healthy streaming car has no
// schedule row, and an applied command must not manufacture one — the table is
// bounded by "cars currently failing", not by command volume.
// TestResetFleetConfigScheduleOnPairing_NoRow is MYR-517 at the SQL layer. This
// used to assert the OPPOSITE — that a car with no schedule row was a silent
// no-op — and prod showed what that cost: Spencer White's car had no row, so
// his lock tap (the strongest evidence the system ever receives: a signed
// command Tesla APPLIED) stamped nothing and opened no pairing epoch. The write
// now creates the row and says it created it, so the caller can tell a car it
// was already tracking from one it had never recorded.
func TestResetFleetConfigScheduleOnPairing_NoRow(t *testing.T) {
	repo := fccPairingFixture(t, "user_pair_norow", time.Now())
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Millisecond)
	c, found, err := repo.ResetFleetConfigScheduleOnPairing(ctx, pairingVIN, now, now.Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("ResetFleetConfigScheduleOnPairing: %v", err)
	}
	if !found {
		t.Fatal("found = false for a car with no schedule row — the epoch must be recorded, not dropped")
	}
	if !c.ScheduleCreated {
		t.Error("ScheduleCreated = false; the caller cannot tell a seeded row from a reset one")
	}
	if c.VehicleID != "veh_pair" || c.VIN != pairingVIN {
		t.Errorf("candidate = %+v, want the seeded vehicle", c)
	}

	var count, attempts int
	var outcome string
	var signedAt time.Time
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM go_fleet_config_attempts`).Scan(&count); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if count != 1 {
		t.Fatalf("attempt rows = %d, want exactly 1 seeded row", count)
	}
	err = testPool.QueryRow(ctx,
		`SELECT attempt_count, last_outcome, signed_command_at FROM go_fleet_config_attempts WHERE vehicle_id = $1`,
		"veh_pair").Scan(&attempts, &outcome, &signedAt)
	if err != nil {
		t.Fatalf("read seeded row: %v", err)
	}
	// The created row is a SEED: it records the epoch and claims nothing else.
	// attempt_count 0 + last_outcome '' is scheduling- and wire-identical to no
	// row, which is what makes creating it safe on a car that is perfectly fine.
	if attempts != 0 {
		t.Errorf("attempt_count = %d, want 0 — a created row must not spend a retry", attempts)
	}
	if outcome != store.SetupOutcomeNone {
		t.Errorf("last_outcome = %q, want '' — a created row must claim nothing", outcome)
	}
	if !signedAt.UTC().Truncate(time.Millisecond).Equal(now) {
		t.Errorf("signed_command_at = %v, want %v — the epoch is the whole point", signedAt.UTC(), now)
	}
}

// A car that ALREADY has a schedule row must be reset in place, not reported as
// created — the caller uses that flag to decide whether an immediate Tesla
// round-trip is warranted, and a tracked-broken car is exactly the case where
// it is.
func TestResetFleetConfigScheduleOnPairing_ExistingRowIsNotReportedAsCreated(t *testing.T) {
	repo := fccPairingFixture(t, "user_pair_existing", time.Now().Add(-3*time.Hour))
	ctx := context.Background()

	if err := repo.RecordFleetConfigAttempt(ctx, "veh_pair",
		time.Now().Add(-time.Hour), time.Now().Add(8*time.Hour), "awaiting_virtual_key"); err != nil {
		t.Fatalf("RecordFleetConfigAttempt: %v", err)
	}

	now := time.Now()
	c, found, err := repo.ResetFleetConfigScheduleOnPairing(ctx, pairingVIN, now, now.Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("ResetFleetConfigScheduleOnPairing: %v", err)
	}
	if !found {
		t.Fatal("found = false for a car with a live schedule row")
	}
	if c.ScheduleCreated {
		t.Error("ScheduleCreated = true for a row that already existed")
	}
	if c.LastOutcome != "awaiting_virtual_key" {
		t.Errorf("LastOutcome = %q, want the existing outcome preserved", c.LastOutcome)
	}
}

// TestResetFleetConfigScheduleOnPairing_UnknownVIN: a VIN we do not own must be
// a clean no-op, not an error the command path has to interpret.
func TestResetFleetConfigScheduleOnPairing_UnknownVIN(t *testing.T) {
	repo := fccPairingFixture(t, "user_pair_unknown", time.Now().Add(-3*time.Hour))
	ctx := context.Background()

	now := time.Now()
	_, found, err := repo.ResetFleetConfigScheduleOnPairing(ctx, "5YJ3E1EA1NF099999", now, now.Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("ResetFleetConfigScheduleOnPairing: %v", err)
	}
	if found {
		t.Error("found = true for an unknown VIN")
	}
}

// TestRecordForcedFleetConfigRepush is the anti-loop bound at the SQL layer: a
// forced re-push must both increment the ordinary attempt count AND stamp the
// epoch budget, so the very next pass reads the budget as spent.
func TestRecordForcedFleetConfigRepush(t *testing.T) {
	repo := fccPairingFixture(t, "user_force", time.Now().Add(-3*time.Hour))
	ctx := context.Background()

	now := time.Now()
	if err := repo.RecordFleetConfigAttempt(ctx, "veh_pair", now.Add(-time.Hour), now.Add(-time.Minute), "synced_not_streaming"); err != nil {
		t.Fatalf("RecordFleetConfigAttempt: %v", err)
	}
	if err := repo.RecordForcedFleetConfigRepush(ctx, "veh_pair", now, now.Add(-time.Second), "forced_repush"); err != nil {
		t.Fatalf("RecordForcedFleetConfigRepush: %v", err)
	}

	rows, err := repo.ListFleetConfigCandidates(ctx, time.Now().Add(-30*time.Minute), time.Now(), 100)
	if err != nil {
		t.Fatalf("ListFleetConfigCandidates: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("candidates = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.AttemptCount != 2 {
		t.Errorf("AttemptCount = %d, want 2 — the forced path must keep incrementing the backoff", got.AttemptCount)
	}
	if got.LastOutcome != "forced_repush" {
		t.Errorf("LastOutcome = %q, want forced_repush", got.LastOutcome)
	}
	if got.ForcedRepushAt.IsZero() {
		t.Fatal("ForcedRepushAt is zero — the epoch budget was not stamped, so the force could repeat")
	}
	// The budget is spent relative to the epoch: no signed command has been
	// seen since, so the reconciler must decline a second force.
	if !got.SignedCommandAt.IsZero() {
		t.Errorf("SignedCommandAt = %v, want zero (no command recorded in this test)", got.SignedCommandAt)
	}
}

// TestRecordForcedFleetConfigRepush_PreservesPairingEpoch: the forced write and
// the pairing write touch the same row from different paths, and neither may
// clobber the other's column — the whole once-per-epoch rule is the comparison
// between them.
func TestRecordForcedFleetConfigRepush_PreservesPairingEpoch(t *testing.T) {
	repo := fccPairingFixture(t, "user_epoch", time.Now().Add(-3*time.Hour))
	ctx := context.Background()

	now := time.Now()
	if err := repo.RecordFleetConfigAttempt(ctx, "veh_pair", now.Add(-2*time.Hour), now.Add(-time.Hour), "awaiting_virtual_key"); err != nil {
		t.Fatalf("RecordFleetConfigAttempt: %v", err)
	}
	// A command applies: epoch opens.
	if _, found, err := repo.ResetFleetConfigScheduleOnPairing(ctx, pairingVIN, now, now.Add(-10*time.Minute)); err != nil || !found {
		t.Fatalf("ResetFleetConfigScheduleOnPairing: found=%v err=%v", found, err)
	}
	// The escalation fires and spends the budget.
	if err := repo.RecordForcedFleetConfigRepush(ctx, "veh_pair", now, now.Add(-time.Second), "forced_repush"); err != nil {
		t.Fatalf("RecordForcedFleetConfigRepush: %v", err)
	}
	// An ordinary attempt follows.
	if err := repo.RecordFleetConfigAttempt(ctx, "veh_pair", now, now.Add(-time.Second), "synced_not_streaming"); err != nil {
		t.Fatalf("RecordFleetConfigAttempt: %v", err)
	}

	rows, err := repo.ListFleetConfigCandidates(ctx, time.Now().Add(-30*time.Minute), time.Now(), 100)
	if err != nil {
		t.Fatalf("ListFleetConfigCandidates: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("candidates = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.SignedCommandAt.IsZero() {
		t.Error("SignedCommandAt was clobbered; the epoch would restart and re-arm the force")
	}
	if got.ForcedRepushAt.IsZero() {
		t.Error("ForcedRepushAt was clobbered by the ordinary attempt; the force could repeat every pass")
	}
	// The force happened at/after the epoch opened, which is precisely how the
	// reconciler reads "budget spent".
	if got.ForcedRepushAt.Before(got.SignedCommandAt) {
		t.Errorf("ForcedRepushAt (%v) predates SignedCommandAt (%v); the budget would read as unspent",
			got.ForcedRepushAt, got.SignedCommandAt)
	}
}
