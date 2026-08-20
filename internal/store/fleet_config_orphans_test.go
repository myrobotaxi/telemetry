package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// These are integration tests: internal/store has no SQL-level fake, so the
// package's tests run against the shared Postgres container TestMain builds and
// skip when docker is unavailable — the same gate vehicle_setup_schedule_test.go
// uses.

// orphanFixture resets every table these tests touch and returns the repo.
func orphanFixture(t *testing.T) *store.FleetConfigOrphanRepo {
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
	return store.NewFleetConfigOrphanRepo(testPool)
}

func seedOrphanAttempt(t *testing.T, vehicleID string) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_fleet_config_attempts (vehicle_id, attempt_count, last_outcome)
		 VALUES ($1, 1, 'push_failed')`, vehicleID)
	if err != nil {
		t.Fatalf("seed go_fleet_config_attempts(%s): %v", vehicleID, err)
	}
}

const (
	orphanVINGone  = "5YJSA1E26MF000601" // tombstoned, no Vehicle row
	orphanVINAlive = "5YJSA1E26MF000602" // tombstoned AND re-added
)

// A tombstone is only an orphan while nobody claims its VIN. A re-added car has
// a live "Vehicle" row and a live config that this sweep must never touch, so
// the re-add case is the one that matters most here.
func TestListTombstonedOrphanVINs(t *testing.T) {
	repo := orphanFixture(t)
	ctx := context.Background()

	seedFCCTombstone(t, "user_orphan", "tv_gone", orphanVINGone)
	seedFCCTombstone(t, "user_orphan", "tv_alive", orphanVINAlive)
	seedFCCTombstone(t, "user_orphan", "tv_novin", "")
	// The re-added car: same VIN as a tombstone, but a live row exists. Note
	// the owner differs from the tombstone's, to prove the NOT EXISTS is global
	// and not per-owner.
	seedFleetCandidateVehicle(t, "veh_alive", "user_other", orphanVINAlive, "tv_alive", time.Now())

	tests := []struct {
		name  string
		limit int
		want  []string
	}{
		{name: "unclaimed vin only", limit: 10, want: []string{orphanVINGone}},
		{name: "non-positive limit reads nothing", limit: 0, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := repo.ListTombstonedOrphanVINs(ctx, tt.limit)
			if err != nil {
				t.Fatalf("ListTombstonedOrphanVINs: %v", err)
			}
			got := make([]string, 0, len(rows))
			for _, r := range rows {
				got = append(got, r.VIN)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("vins = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("vins = %v, want %v", got, tt.want)
				}
			}
		})
	}

	t.Run("carries the owner handle", func(t *testing.T) {
		rows, err := repo.ListTombstonedOrphanVINs(ctx, 10)
		if err != nil {
			t.Fatalf("ListTombstonedOrphanVINs: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("len(rows) = %d, want 1", len(rows))
		}
		if rows[0].UserID != "user_orphan" || rows[0].TeslaVehicleID != "tv_gone" {
			t.Fatalf("row = %+v, want user_orphan/tv_gone", rows[0])
		}
		if rows[0].RemovedAt.IsZero() {
			t.Fatal("RemovedAt is zero; the tombstone's default should have stamped it")
		}
	})
}

func TestListTeslaLinkedUserIDsAndVINs(t *testing.T) {
	repo := orphanFixture(t)
	ctx := context.Background()

	seedFCCAccount(t, "acct_t1", "user_linked", "tesla-sub-1")
	// A second tesla row for the SAME user (a relink) must not double-count.
	seedFCCAccount(t, "acct_t2", "user_linked", "tesla-sub-2")
	// A non-tesla provider must not appear at all.
	seedFCCAccount(t, "acct_apple", "user_apple", "apple-sub", "apple")
	seedFleetCandidateVehicle(t, "veh_linked", "user_linked", orphanVINGone, "tv_linked", time.Now())

	t.Run("distinct tesla-linked users only", func(t *testing.T) {
		ids, err := repo.ListTeslaLinkedUserIDs(ctx)
		if err != nil {
			t.Fatalf("ListTeslaLinkedUserIDs: %v", err)
		}
		if len(ids) != 1 || ids[0] != "user_linked" {
			t.Fatalf("ids = %v, want [user_linked]", ids)
		}
	})

	t.Run("vins by user", func(t *testing.T) {
		vins, err := repo.ListVehicleVINsByUser(ctx, "user_linked")
		if err != nil {
			t.Fatalf("ListVehicleVINsByUser: %v", err)
		}
		if len(vins) != 1 || vins[0] != orphanVINGone {
			t.Fatalf("vins = %v, want [%s]", vins, orphanVINGone)
		}
	})

	t.Run("a user with no vehicles reads empty, not an error", func(t *testing.T) {
		vins, err := repo.ListVehicleVINsByUser(ctx, "user_apple")
		if err != nil {
			t.Fatalf("ListVehicleVINsByUser: %v", err)
		}
		if len(vins) != 0 {
			t.Fatalf("vins = %v, want none", vins)
		}
	})
}

// The DELETE re-proves the orphan predicate, so passing it a LIVE vehicle's id
// must remove nothing. That guard is the reason an operator tool is allowed to
// write to this table at all.
func TestOrphanFleetConfigAttempts(t *testing.T) {
	repo := orphanFixture(t)
	ctx := context.Background()

	seedFleetCandidateVehicle(t, "veh_live", "user_att", orphanVINGone, "tv_live", time.Now())
	seedOrphanAttempt(t, "veh_live")
	seedOrphanAttempt(t, "veh_deleted_1")
	seedOrphanAttempt(t, "veh_deleted_2")

	t.Run("lists only rows whose vehicle is gone", func(t *testing.T) {
		ids, err := repo.ListOrphanFleetConfigAttempts(ctx)
		if err != nil {
			t.Fatalf("ListOrphanFleetConfigAttempts: %v", err)
		}
		want := []string{"veh_deleted_1", "veh_deleted_2"}
		if len(ids) != len(want) {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
		for i := range ids {
			if ids[i] != want[i] {
				t.Fatalf("ids = %v, want %v", ids, want)
			}
		}
	})

	t.Run("empty slice is a no-op success", func(t *testing.T) {
		n, err := repo.DeleteFleetConfigAttempts(ctx, nil)
		if err != nil || n != 0 {
			t.Fatalf("DeleteFleetConfigAttempts(nil) = %d, %v; want 0, nil", n, err)
		}
	})

	t.Run("a live vehicle's id is refused by the delete itself", func(t *testing.T) {
		n, err := repo.DeleteFleetConfigAttempts(ctx, []string{"veh_live"})
		if err != nil {
			t.Fatalf("DeleteFleetConfigAttempts: %v", err)
		}
		if n != 0 {
			t.Fatalf("deleted %d rows for a live vehicle, want 0", n)
		}
	})

	t.Run("deletes the orphans", func(t *testing.T) {
		n, err := repo.DeleteFleetConfigAttempts(ctx, []string{"veh_deleted_1", "veh_deleted_2", "veh_live"})
		if err != nil {
			t.Fatalf("DeleteFleetConfigAttempts: %v", err)
		}
		if n != 2 {
			t.Fatalf("deleted %d rows, want 2 (veh_live must survive)", n)
		}
		ids, err := repo.ListOrphanFleetConfigAttempts(ctx)
		if err != nil {
			t.Fatalf("ListOrphanFleetConfigAttempts: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("orphans remain: %v", ids)
		}
	})
}

// --- MYR-596: the legacy orphaned-tombstone purge --------------------------

// tombstonePurgeFixture builds on orphanFixture and additionally clears the
// three identity sources plus the convergence graph, because "orphaned" is
// defined by the ABSENCE of a row in any of them.
func tombstonePurgeFixture(t *testing.T) *store.FleetConfigOrphanRepo {
	t.Helper()
	repo := orphanFixture(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM go_identity_convergence`,
		`DELETE FROM go_identity_apple`,
		`DELETE FROM go_users`,
		`DELETE FROM "User"`,
	} {
		if _, err := testPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("clean (%s): %v", stmt, err)
		}
	}
	return repo
}

func seedPurgeTombstone(t *testing.T, userID, teslaVehicleID, vin string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO go_removed_vehicles (user_id, tesla_vehicle_id, vin, removed_at)
		VALUES ($1, $2, NULLIF($3, ''), NOW())`, userID, teslaVehicleID, vin); err != nil {
		t.Fatalf("seed tombstone %s/%s: %v", userID, teslaVehicleID, err)
	}
}

// TestFleetConfigOrphanRepo_OrphanedTombstonePurge is the predicate test, and
// the arms that matter are the ones it must NOT delete.
//
// A tombstone is orphaned only when its user_id resolves nowhere. Each of the
// four identity probes gets an arm here, because dropping any one of them turns
// a live person's tombstone into a purge target — and a purged tombstone lets
// that person's next Tesla sync resurrect a car they deliberately removed,
// which is the MYR-261 bug arriving by a new road.
func TestFleetConfigOrphanRepo_OrphanedTombstonePurge(t *testing.T) {
	repo := tombstonePurgeFixture(t)
	ctx := context.Background()

	// The target: an account deleted before MYR-596 shipped. No identity row of
	// any kind, no convergence edge, just the tombstones it left behind.
	seedPurgeTombstone(t, "cghost0000000000000000001", "tesla_ghost_a", "5YJ3E1EA1JF000101")
	seedPurgeTombstone(t, "cghost0000000000000000001", "tesla_ghost_b", "5YJ3E1EA1JF000102")

	// Four survivors, one per identity probe.
	seedPurgeTombstone(t, "clegacy000000000000000001", "tesla_legacy", "5YJ3E1EA1JF000103")
	if _, err := testPool.Exec(ctx,
		`INSERT INTO "User" ("id", "updatedAt") VALUES ($1, NOW())`,
		"clegacy000000000000000001"); err != nil {
		t.Fatalf("seed User: %v", err)
	}

	seedPurgeTombstone(t, "capple0000000000000000001", "tesla_apple", "5YJ3E1EA1JF000104")
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_users (id) VALUES ($1)`, "capple0000000000000000001"); err != nil {
		t.Fatalf("seed go_users: %v", err)
	}

	seedPurgeTombstone(t, "cbound0000000000000000001", "tesla_bound", "5YJ3E1EA1JF000105")
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_identity_apple (apple_sub, user_id) VALUES ($1, $2)`,
		"apple-sub-bound", "cbound0000000000000000001"); err != nil {
		t.Fatalf("seed go_identity_apple: %v", err)
	}

	// THE IMPORTANT ONE. A converged person's ABANDONED cuid: their Apple
	// binding and go_users row moved to the canonical id, so this id carries no
	// identity row of its own — and its tombstones are still a LIVE person's
	// (MYR-452). Only the convergence edge distinguishes it from the ghost above.
	seedPurgeTombstone(t, "cabandoned000000000000001", "tesla_abandoned", "5YJ3E1EA1JF000106")
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_identity_convergence (from_user_id, to_user_id) VALUES ($1, $2)`,
		"cabandoned000000000000001", "ccanonical00000000000001"); err != nil {
		t.Fatalf("seed go_identity_convergence: %v", err)
	}

	n, err := repo.CountOrphanedTombstones(ctx)
	if err != nil {
		t.Fatalf("CountOrphanedTombstones: %v", err)
	}
	if n != 2 {
		t.Fatalf("counted %d orphaned tombstone(s), want 2 (the ghost's pair only)", n)
	}

	deleted, err := repo.PurgeOrphanedTombstones(ctx)
	if err != nil {
		t.Fatalf("PurgeOrphanedTombstones: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("purged %d row(s), want 2", deleted)
	}

	for _, survivor := range []string{
		"clegacy000000000000000001",
		"capple0000000000000000001",
		"cbound0000000000000000001",
		"cabandoned000000000000001",
	} {
		if got := countQuery(t,
			`SELECT count(*) FROM go_removed_vehicles WHERE user_id = $1`, survivor); got != 1 {
			t.Errorf("tombstone for %s was purged (%d left, want 1)", survivor, got)
		}
	}
	if got := countQuery(t,
		`SELECT count(*) FROM go_removed_vehicles WHERE user_id = $1`,
		"cghost0000000000000000001"); got != 0 {
		t.Errorf("%d ghost tombstone(s) survived the purge", got)
	}

	// Idempotent: a second pass finds none and removes none.
	again, err := repo.PurgeOrphanedTombstones(ctx)
	if err != nil {
		t.Fatalf("re-purge: %v", err)
	}
	if again != 0 {
		t.Fatalf("re-purge removed %d row(s), want 0", again)
	}
	if n, err := repo.CountOrphanedTombstones(ctx); err != nil || n != 0 {
		t.Fatalf("re-count = %d (err %v), want 0", n, err)
	}
}
