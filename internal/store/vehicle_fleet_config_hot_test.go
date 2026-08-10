package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestListFleetConfigCandidates_HotPairingEpoch is the MYR-529 store half, and
// it reproduces Joey Verrone's row exactly.
//
// His car linked at 01:22:08Z, proved its virtual key at 01:23:10Z, and was
// scheduled for 01:38. It was never returned by a single pass — not because the
// schedule was wrong but because `"Vehicle"."lastUpdated"` kept being written
// by ordinary provisioning and web-sync traffic (01:41:33Z), so the thirty
// minutes of silence the staleness arm demands never accumulated. A car with a
// LIVE PAIRING EPOCH is now admitted on its schedule alone.
//
// The exemption's narrowness is asserted alongside it: a car with a fresh row
// and NO epoch stays excluded, a car whose epoch has aged out stays excluded,
// and a hot car whose backoff has not elapsed stays excluded — the epoch waives
// staleness, never the schedule.
func TestListFleetConfigCandidates_HotPairingEpoch(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	mustApplyOwnerSchema(t)
	cleanTables(t, testPool)
	cleanFleetCandidateTables(t)

	ctx := context.Background()
	const owner = "user_hot"
	seedFCCAccount(t, "acct_hot", owner, "tesla-sub-hot")

	now := time.Now()
	// Every vehicle here is FRESH by the staleness rule — that is the whole
	// point. Only the epoch may let one through.
	fresh := now.Add(-time.Minute)

	seedFleetCandidateVehicle(t, "veh_joey", owner, "7SAYGDED5TA736164", "tv_joey", fresh)
	seedFleetCandidateVehicle(t, "veh_noepoch", owner, "5YJ3E1EA1NF000101", "tv_noepoch", fresh)
	seedFleetCandidateVehicle(t, "veh_cold", owner, "5YJ3E1EA1NF000102", "tv_cold", fresh)
	seedFleetCandidateVehicle(t, "veh_notdue", owner, "5YJ3E1EA1NF000103", "tv_notdue", fresh)

	// Joey: epoch a minute old, due now.
	seedHotSchedule(t, "veh_joey", now.Add(-time.Minute), now.Add(-10*time.Second))
	// No epoch at all, due now — the ordinary fresh car the staleness rule is
	// there to protect from a pointless Tesla read.
	seedHotSchedule(t, "veh_noepoch", time.Time{}, now.Add(-10*time.Second))
	// Epoch from an hour ago: long past the hot window a caller would pass.
	seedHotSchedule(t, "veh_cold", now.Add(-time.Hour), now.Add(-10*time.Second))
	// Hot epoch but its backoff has NOT elapsed. The exemption waives
	// staleness, never the schedule.
	seedHotSchedule(t, "veh_notdue", now.Add(-time.Minute), now.Add(10*time.Minute))

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	cutoff := now.Add(-30 * time.Minute)
	hotSince := now.Add(-15 * time.Minute)

	rows, err := repo.ListFleetConfigCandidates(ctx, cutoff, now, hotSince, 100)
	if err != nil {
		t.Fatalf("ListFleetConfigCandidates: %v", err)
	}

	got := make(map[string]store.FleetConfigCandidate, len(rows))
	for _, r := range rows {
		got[r.VehicleID] = r
	}

	if _, ok := got["veh_joey"]; !ok {
		t.Errorf("veh_joey missing: a car whose key was proven a minute ago must be examinable "+
			"even though its row was written recently (got %v)", hotCandidateIDs(got))
	}
	for _, id := range []string{"veh_noepoch", "veh_cold", "veh_notdue"} {
		if _, ok := got[id]; ok {
			t.Errorf("%s was admitted; the pairing-epoch exemption is wider than intended", id)
		}
	}

	// The free streaming check downstream reads `status` off this very row, so
	// it has to arrive.
	if got["veh_joey"].Status != "offline" {
		t.Errorf("Status = %q, want the seeded %q — the reconciler's zero-cost streaming check reads it",
			got["veh_joey"].Status, "offline")
	}
}

// TestListFleetConfigCandidates_ZeroHotSinceFailsClosed: a zero hotSince means
// "no exemption", and it must match NOTHING rather than everything. A zero
// time.Time is year 1, which as a lower bound would admit every row that has
// ever recorded a pairing epoch.
func TestListFleetConfigCandidates_ZeroHotSinceFailsClosed(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	mustApplyOwnerSchema(t)
	cleanTables(t, testPool)
	cleanFleetCandidateTables(t)

	ctx := context.Background()
	const owner = "user_hot0"
	seedFCCAccount(t, "acct_hot0", owner, "tesla-sub-hot0")

	now := time.Now()
	seedFleetCandidateVehicle(t, "veh_fresh_epoch", owner, "7SAYGDED5TA736164", "tv_h0", now.Add(-time.Minute))
	seedHotSchedule(t, "veh_fresh_epoch", now.Add(-time.Minute), now.Add(-10*time.Second))

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	rows, err := repo.ListFleetConfigCandidates(ctx, now.Add(-30*time.Minute), now, time.Time{}, 100)
	if err != nil {
		t.Fatalf("ListFleetConfigCandidates: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %d with a zero hotSince, want 0 — a disabled guard must fail closed", len(rows))
	}
}

func seedHotSchedule(t *testing.T, vehicleID string, signedCommandAt, nextAttemptAt time.Time) {
	t.Helper()
	var signed any
	if !signedCommandAt.IsZero() {
		signed = signedCommandAt
	}
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_fleet_config_attempts
		    (vehicle_id, attempt_count, last_attempt_at, next_attempt_at, last_outcome, signed_command_at)
		 VALUES ($1, 0, $2, $2, '', $3)`,
		vehicleID, nextAttemptAt, signed,
	)
	if err != nil {
		t.Fatalf("seedHotSchedule(%s): %v", vehicleID, err)
	}
}

func hotCandidateIDs(m map[string]store.FleetConfigCandidate) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
