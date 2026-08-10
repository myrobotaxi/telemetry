package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestVehicleRepo_ListFleetConfigCandidates backs the MYR-448 reconciler.
//
// The list IS the safety boundary: everything it returns will be pushed a
// fleet-telemetry config against a REAL car. So the properties that matter are
// symmetric — it must name every provisioned-but-quiet car (or a stranded
// owner never heals), and it must name NOTHING else, in particular no car the
// owner deliberately removed.
func TestVehicleRepo_ListFleetConfigCandidates(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	mustApplyOwnerSchema(t)
	cleanTables(t, testPool)
	cleanFleetCandidateTables(t)

	ctx := context.Background()
	const owner = "user_fcc"
	const ownerNoTesla = "user_fcc_notesla"

	seedFCCAccount(t, "acct_fcc", owner, "tesla-sub-1")
	seedFCCAccount(t, "acct_fcc_google", ownerNoTesla, "google-sub-1", "google")

	// "Vehicle"."lastUpdated" is NOT NULL DEFAULT CURRENT_TIMESTAMP in prod, so
	// a car that has NEVER streamed does not read as NULL — it carries the
	// timestamp of its own provisioning INSERT and simply never moves again.
	// That is the real MYR-448 shape and what these fixtures reproduce.
	neverStreamed := time.Now().Add(-3 * time.Hour)
	wentSilent := time.Now().Add(-2 * time.Hour)
	justLinked := time.Now().Add(-1 * time.Minute)
	streaming := time.Now()

	// The MYR-448 shape: linked and provisioned 3h ago, never streamed since.
	seedFleetCandidateVehicle(t, "veh_never", owner, "7SAYGDED5TA736164", "tv_never", neverStreamed)
	// Streamed once at link time then went silent.
	seedFleetCandidateVehicle(t, "veh_stale", owner, "5YJ3E1EA1NF000001", "tv_stale", wentSilent)
	// Healthy, actively streaming.
	seedFleetCandidateVehicle(t, "veh_fresh", owner, "5YJ3E1EA1NF000002", "tv_fresh", streaming)
	// Linked a minute ago: its link-time push may still be in flight, so the
	// staleness grace window must keep the reconciler off it.
	seedFleetCandidateVehicle(t, "veh_justlinked", owner, "5YJ3E1EA1NF000005", "tv_just", justLinked)
	// Deliberately removed by its owner (MYR-261 tombstone-wins).
	seedFleetCandidateVehicle(t, "veh_tomb", owner, "5YJ3E1EA1NF000003", "tv_tomb", neverStreamed)
	seedFCCTombstone(t, owner, "tv_tomb", "5YJ3E1EA1NF000003")
	// Owner has no Tesla account — no token, so it can never be healed here.
	seedFleetCandidateVehicle(t, "veh_notesla", ownerNoTesla, "5YJ3E1EA1NF000004", "tv_notesla", neverStreamed)
	// Placeholder row seeded before Tesla supplied a VIN.
	seedFleetCandidateVehicle(t, "veh_novin", owner, "", "tv_novin", neverStreamed)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	cutoff := time.Now().Add(-30 * time.Minute)

	t.Run("names quiet cars and nothing else", func(t *testing.T) {
		rows, err := repo.ListFleetConfigCandidates(ctx, cutoff, time.Now(), time.Time{}, 100)
		if err != nil {
			t.Fatalf("ListFleetConfigCandidates: %v", err)
		}

		got := make(map[string]store.FleetConfigCandidate, len(rows))
		for _, r := range rows {
			got[r.VehicleID] = r
		}

		for _, want := range []string{"veh_never", "veh_stale"} {
			if _, ok := got[want]; !ok {
				t.Errorf("missing candidate %s — a stranded owner would never heal", want)
			}
		}
		for _, unwanted := range []string{"veh_fresh", "veh_justlinked", "veh_tomb", "veh_notesla", "veh_novin"} {
			if _, ok := got[unwanted]; ok {
				t.Errorf("%s must NOT be a candidate", unwanted)
			}
		}
		if len(rows) != 2 {
			t.Fatalf("len = %d (%v), want exactly 2", len(rows), keysOf(got))
		}
	})

	t.Run("carries the fields the reconciler needs", func(t *testing.T) {
		rows, err := repo.ListFleetConfigCandidates(ctx, cutoff, time.Now(), time.Time{}, 100)
		if err != nil {
			t.Fatalf("ListFleetConfigCandidates: %v", err)
		}
		for _, r := range rows {
			if r.VIN == "" || len(r.VIN) != 17 {
				t.Errorf("%s: VIN = %q, want a 17-char VIN", r.VehicleID, r.VIN)
			}
			if r.UserID == "" {
				t.Errorf("%s: UserID empty — the owner token cannot be resolved", r.VehicleID)
			}
		}
	})

	t.Run("longest-quiet car sorts first so it is never starved", func(t *testing.T) {
		rows, err := repo.ListFleetConfigCandidates(ctx, cutoff, time.Now(), time.Time{}, 1)
		if err != nil {
			t.Fatalf("ListFleetConfigCandidates: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("len = %d, want 1 — the cap bounds one pass", len(rows))
		}
		if rows[0].VehicleID != "veh_never" {
			t.Errorf("first candidate = %s, want veh_never (oldest lastUpdated first)", rows[0].VehicleID)
		}
		if rows[0].LastUpdated.IsZero() {
			t.Error("LastUpdated is zero; the staleness hint should be carried through")
		}
	})

	t.Run("non-positive limit refuses to scan", func(t *testing.T) {
		for _, limit := range []int{0, -1} {
			rows, err := repo.ListFleetConfigCandidates(ctx, cutoff, time.Now(), time.Time{}, limit)
			if err != nil {
				t.Fatalf("ListFleetConfigCandidates(limit=%d): %v", limit, err)
			}
			if len(rows) != 0 {
				t.Errorf("limit=%d returned %d rows, want 0", limit, len(rows))
			}
		}
	})
}

// H3: "Vehicle"."teslaVehicleId" is NULLABLE, and a NULL makes the tombstone
// equality NULL, which makes NOT EXISTS true and ADMITS the row. A guard that
// opens when its join key is missing is inverted — and the consequence is
// pushing config to a car its owner deliberately removed.
func TestFleetConfigCandidates_TombstoneMatchesOnVINWhenTeslaIDIsNull(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	mustApplyOwnerSchema(t)
	cleanTables(t, testPool)
	cleanFleetCandidateTables(t)

	ctx := context.Background()
	const owner = "user_tomb_null"
	seedFCCAccount(t, "acct_tomb_null", owner, "tesla-sub-tomb")

	quiet := time.Now().Add(-3 * time.Hour)
	const vin = "7SAYGDED5TA736164"

	// The Prisma-side sync can recreate a Vehicle row with no teslaVehicleId;
	// it knows nothing about go_removed_vehicles (CG-DL-9).
	_, err := testPool.Exec(ctx,
		`INSERT INTO "Vehicle" ("id", "userId", "vin", "teslaVehicleId", "name", "status", "lastUpdated")
		 VALUES ('veh_tomb_null', $1, $2, NULL, 'Tesla', 'offline', $3)`,
		owner, vin, quiet)
	if err != nil {
		t.Fatalf("seed vehicle: %v", err)
	}
	// Tombstone recorded under the id the car had when it was removed.
	seedFCCTombstone(t, owner, "tv_gone", vin)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	rows, err := repo.ListFleetConfigCandidates(ctx, time.Now().Add(-30*time.Minute), time.Now(), time.Time{}, 100)
	if err != nil {
		t.Fatalf("ListFleetConfigCandidates: %v", err)
	}
	for _, r := range rows {
		if r.VehicleID == "veh_tomb_null" {
			t.Fatal("a tombstoned car with a NULL teslaVehicleId was admitted — " +
				"the reconciler would resurrect a deliberately removed vehicle")
		}
	}
}

// M1: the "Account" unique index is (provider, providerAccountId), NOT
// (userId, provider) — one user can hold several tesla rows. A plain JOIN
// would emit the vehicle once per row, doubling the Tesla calls and eating
// the per-pass budget.
func TestFleetConfigCandidates_DuplicateTeslaAccountsDoNotFanOut(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	mustApplyOwnerSchema(t)
	cleanTables(t, testPool)
	cleanFleetCandidateTables(t)

	ctx := context.Background()
	const owner = "user_dupe"
	seedFCCAccount(t, "acct_dupe_1", owner, "tesla-sub-a")
	seedFCCAccount(t, "acct_dupe_2", owner, "tesla-sub-b")

	seedFleetCandidateVehicle(t, "veh_dupe", owner, "7SAYGDED5TA736164", "tv_dupe",
		time.Now().Add(-3*time.Hour))

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	rows, err := repo.ListFleetConfigCandidates(ctx, time.Now().Add(-30*time.Minute), time.Now(), time.Time{}, 100)
	if err != nil {
		t.Fatalf("ListFleetConfigCandidates: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len = %d, want 1 — two tesla Account rows must not duplicate the vehicle", len(rows))
	}
}

// C1: the schedule is what guarantees coverage. A car that was just attempted
// must drop out of the candidate set until its backoff elapses, otherwise the
// same rows fill the LIMIT on every pass and later owners are never reached.
func TestFleetConfigCandidates_RespectAttemptSchedule(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	mustApplyOwnerSchema(t)
	cleanTables(t, testPool)
	cleanFleetCandidateTables(t)

	ctx := context.Background()
	const owner = "user_sched"
	seedFCCAccount(t, "acct_sched", owner, "tesla-sub-sched")

	quiet := time.Now().Add(-3 * time.Hour)
	seedFleetCandidateVehicle(t, "veh_a", owner, "7SAYGDED5TA736164", "tv_a", quiet)
	seedFleetCandidateVehicle(t, "veh_b", owner, "5YJ3E1EA1NF000001", "tv_b", quiet)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	cutoff := time.Now().Add(-30 * time.Minute)

	rows, err := repo.ListFleetConfigCandidates(ctx, cutoff, time.Now(), time.Time{}, 100)
	if err != nil {
		t.Fatalf("ListFleetConfigCandidates: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2 before any attempt", len(rows))
	}
	for _, r := range rows {
		if r.AttemptCount != 0 {
			t.Errorf("%s: AttemptCount = %d, want 0 for a never-attempted car", r.VehicleID, r.AttemptCount)
		}
	}

	now := time.Now()
	if err := repo.RecordFleetConfigAttempt(ctx, "veh_a", now, now.Add(time.Hour), "awaiting_virtual_key"); err != nil {
		t.Fatalf("RecordFleetConfigAttempt: %v", err)
	}

	rows, err = repo.ListFleetConfigCandidates(ctx, cutoff, time.Now(), time.Time{}, 100)
	if err != nil {
		t.Fatalf("ListFleetConfigCandidates: %v", err)
	}
	if len(rows) != 1 || rows[0].VehicleID != "veh_b" {
		t.Fatalf("got %v, want only veh_b — veh_a is backed off until next_attempt_at", candidateIDs(rows))
	}

	// Once the backoff elapses the car is due again, carrying its count so the
	// next gap is longer.
	past := time.Now().Add(-time.Minute)
	if err := repo.RecordFleetConfigAttempt(ctx, "veh_a", past, past, "awaiting_virtual_key"); err != nil {
		t.Fatalf("RecordFleetConfigAttempt: %v", err)
	}
	rows, err = repo.ListFleetConfigCandidates(ctx, cutoff, time.Now(), time.Time{}, 100)
	if err != nil {
		t.Fatalf("ListFleetConfigCandidates: %v", err)
	}
	var vehA *store.FleetConfigCandidate
	for i := range rows {
		if rows[i].VehicleID == "veh_a" {
			vehA = &rows[i]
		}
	}
	if vehA == nil {
		t.Fatalf("veh_a should be due again, got %v", candidateIDs(rows))
	}
	if vehA.AttemptCount != 2 {
		t.Errorf("AttemptCount = %d, want 2 — the count must accumulate to drive backoff", vehA.AttemptCount)
	}

	// A successful push clears the schedule entirely.
	if err := repo.ClearFleetConfigAttempts(ctx, "veh_a"); err != nil {
		t.Fatalf("ClearFleetConfigAttempts: %v", err)
	}
	rows, err = repo.ListFleetConfigCandidates(ctx, cutoff, time.Now(), time.Time{}, 100)
	if err != nil {
		t.Fatalf("ListFleetConfigCandidates: %v", err)
	}
	for _, r := range rows {
		if r.VehicleID == "veh_a" && r.AttemptCount != 0 {
			t.Errorf("AttemptCount = %d after clear, want 0", r.AttemptCount)
		}
	}
}

func candidateIDs(rows []store.FleetConfigCandidate) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.VehicleID)
	}
	return out
}

func keysOf(m map[string]store.FleetConfigCandidate) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustApplyOwnerSchema(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), ownerSchemaSQL); err != nil {
		t.Fatalf("apply owner schema: %v", err)
	}
}

func cleanFleetCandidateTables(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM go_removed_vehicles`,
		`DELETE FROM "Account"`,
	} {
		if _, err := testPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("clean (%s): %v", stmt, err)
		}
	}
}

// seedFleetCandidateVehicle inserts a Vehicle row with an explicit
// "lastUpdated" (nil == never written, the MYR-448 shape).
func seedFleetCandidateVehicle(
	t *testing.T,
	id, userID, vin, teslaVehicleID string,
	lastUpdated time.Time,
) {
	t.Helper()
	// "" VIN would collide on the UNIQUE index across rows, so only one such
	// placeholder row is seeded per test.
	var vinArg any = vin
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO "Vehicle" ("id", "userId", "vin", "teslaVehicleId", "name", "status", "lastUpdated")
		 VALUES ($1, $2, $3, $4, 'Tesla', 'offline', $5)`,
		id, userID, vinArg, teslaVehicleID, lastUpdated,
	)
	if err != nil {
		t.Fatalf("seedFleetCandidateVehicle(%s): %v", id, err)
	}
}

func seedFCCAccount(t *testing.T, id, userID, providerAccountID string, provider ...string) {
	t.Helper()
	p := "tesla"
	if len(provider) > 0 {
		p = provider[0]
	}
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO "Account" ("id", "userId", "type", "provider", "providerAccountId")
		 VALUES ($1, $2, 'oauth', $3, $4)`,
		id, userID, p, providerAccountID,
	)
	if err != nil {
		t.Fatalf("seedFCCAccount(%s): %v", id, err)
	}
}

func seedFCCTombstone(t *testing.T, userID, teslaVehicleID, vin string) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_removed_vehicles (user_id, tesla_vehicle_id, vin)
		 VALUES ($1, $2, $3)`,
		userID, teslaVehicleID, vin,
	)
	if err != nil {
		t.Fatalf("seedFCCTombstone(%s): %v", teslaVehicleID, err)
	}
}
