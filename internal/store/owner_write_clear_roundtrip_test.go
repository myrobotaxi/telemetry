package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-351 — the SET → CLEAR → RE-READ round-trips, through the PROJECTION
// QUERIES rather than through raw column reads.
//
// WHY THESE EXIST. Three TestFlight reports said an owner write reverted
// seconds after it was made: a cleared service completion date came back, and a
// paused ride-share switch turned itself on again. The root cause turned out to
// be entirely client-side (the iOS executor re-applied the last snapshot's
// snapshot-only fields on every telemetry frame), and the server was correct
// throughout — the clear really did persist and the pause really did stick.
//
// But NOTHING HERE PROVED THAT, which is why the investigation had to span two
// repos to rule the server out. The coverage that existed stopped one layer
// short in each direction:
//
//   - the service-window clear was asserted only by reading the raw columns
//     back (`SELECT service_etc, service_expected_end_at`), never through
//     `GetByID` / `ListSummariesByUser` — so the projection's
//     `COALESCE(service_etc, service_expected_end_at)` resolution was untested
//     against a cleared row, and it is precisely the expression that could
//     resurrect a value a clear had just removed;
//   - `TestVehicleRepo_ServiceWindowSurfacesOnReads` set both columns and read
//     them back, but never cleared them, so "surfaces after a SET" was pinned
//     and "surfaces as NULL after a CLEAR" was not.
//
// A regression test whose only job is to fail loudly if the server ever DOES
// acquire the defect it was suspected of. The prime suspect named in the
// investigation was the two-column asymmetry: an owner clear writes NULL to
// `service_expected_end_at` alone, so a stale `service_etc` left behind by
// Tesla would be resurrected by the read-side COALESCE. Sub-test 2 pins exactly
// that boundary — which is a FEATURE, not a bug, and has to stay one.

// TestVehicleRepo_ServiceWindowClearRoundTripsThroughTheProjections is the
// service-window half: set, clear, and re-read through both projection queries.
func TestVehicleRepo_ServiceWindowClearRoundTripsThroughTheProjections(t *testing.T) {
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)

	const (
		ownerID = "user_sw_clear"
		vehicle = "veh_sw_clear_1"
	)
	seedVehicleSummaryRow(t, vehicle, ownerID, "5YJ3E1EA1NF000S21", "AtShop", "Model 3", 2024, "Blue", store.VehicleStatusInService, 44, 150)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	// Read the OWNER column back through both projections at once, so a fix that
	// repaired one surface and not the other cannot pass.
	readBoth := func(t *testing.T) (store.Vehicle, store.VehicleSummary) {
		t.Helper()
		v, err := repo.GetByID(ctx, vehicle)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		rows, err := repo.ListSummariesByUser(ctx, ownerID)
		if err != nil {
			t.Fatalf("ListSummariesByUser: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("catalog rows = %d, want 1", len(rows))
		}
		return v, rows[0]
	}

	t.Run("the owner value surfaces on both projections after a set", func(t *testing.T) {
		if err := repo.SetServiceExpectedEndAt(ctx, vehicle, &ownerEnd); err != nil {
			t.Fatalf("SetServiceExpectedEndAt: %v", err)
		}
		snap, cat := readBoth(t)
		if snap.ServiceExpectedEndAt == nil || !snap.ServiceExpectedEndAt.Equal(ownerEnd) {
			t.Errorf("snapshot ServiceExpectedEndAt = %v, want %v", snap.ServiceExpectedEndAt, ownerEnd)
		}
		if cat.ServiceExpectedEndAt == nil || !cat.ServiceExpectedEndAt.Equal(ownerEnd) {
			t.Errorf("catalog ServiceExpectedEndAt = %v, want %v", cat.ServiceExpectedEndAt, ownerEnd)
		}
	})

	// THE CLIENT'S REPORT, at the store layer. The owner retracts the estimate;
	// both projections must go NULL and STAY null.
	t.Run("a clear surfaces as null on both projections", func(t *testing.T) {
		if err := repo.SetServiceExpectedEndAt(ctx, vehicle, nil); err != nil {
			t.Fatalf("SetServiceExpectedEndAt(nil): %v", err)
		}
		snap, cat := readBoth(t)
		if snap.ServiceExpectedEndAt != nil {
			t.Errorf("snapshot resurrected a cleared owner value: %v", snap.ServiceExpectedEndAt)
		}
		if cat.ServiceExpectedEndAt != nil {
			t.Errorf("catalog resurrected a cleared owner value: %v", cat.ServiceExpectedEndAt)
		}
		if snap.ServiceETC != nil {
			t.Errorf("snapshot ServiceETC = %v, want nil (Tesla never wrote one)", snap.ServiceETC)
		}
	})

	// THE PRIME SUSPECT, pinned as the INTENDED behaviour it actually is.
	//
	// The two sources are separate columns and an owner clear touches only their
	// own. So when Tesla HAS an estimate, clearing the owner's entry leaves
	// Tesla's standing — the read-side COALESCE(tesla, owner) then emits Tesla's
	// value, and an owner who cleared "their" date still sees a date.
	//
	// That is correct: Tesla's estimate outranks the owner's by design (§7.16),
	// so it was never the owner's to retract. It is written down here because it
	// LOOKS exactly like the reverting-clear bug from the outside, and the next
	// person to investigate one should find the distinction already made rather
	// than have to re-derive it.
	t.Run("a clear does not touch Tesla's own estimate", func(t *testing.T) {
		if err := repo.SetServiceETC(ctx, vehicle, &teslaETC); err != nil {
			t.Fatalf("SetServiceETC: %v", err)
		}
		if err := repo.SetServiceExpectedEndAt(ctx, vehicle, &ownerEnd); err != nil {
			t.Fatalf("SetServiceExpectedEndAt: %v", err)
		}
		if err := repo.SetServiceExpectedEndAt(ctx, vehicle, nil); err != nil {
			t.Fatalf("SetServiceExpectedEndAt(nil): %v", err)
		}

		snap, cat := readBoth(t)
		if snap.ServiceExpectedEndAt != nil {
			t.Errorf("the owner clear did not take: %v", snap.ServiceExpectedEndAt)
		}
		if snap.ServiceETC == nil || !snap.ServiceETC.Equal(teslaETC) {
			t.Errorf("the owner clear erased Tesla's estimate: %v", snap.ServiceETC)
		}
		if cat.ServiceETC == nil || !cat.ServiceETC.Equal(teslaETC) {
			t.Errorf("catalog lost Tesla's estimate after an owner clear: %v", cat.ServiceETC)
		}
	})

	// The monitor's clear-on-exit drops BOTH, which is the only path that may.
	t.Run("leaving service clears both sources on both projections", func(t *testing.T) {
		cleared, err := repo.ClearServiceWindow(ctx, vehicle)
		if err != nil {
			t.Fatalf("ClearServiceWindow: %v", err)
		}
		if !cleared {
			t.Error("ClearServiceWindow reported nothing cleared, but Tesla's estimate was set")
		}
		snap, cat := readBoth(t)
		if snap.ServiceETC != nil || snap.ServiceExpectedEndAt != nil {
			t.Errorf("snapshot kept a window after the visit ended: etc=%v owner=%v",
				snap.ServiceETC, snap.ServiceExpectedEndAt)
		}
		if cat.ServiceETC != nil || cat.ServiceExpectedEndAt != nil {
			t.Errorf("catalog kept a window after the visit ended: etc=%v owner=%v",
				cat.ServiceETC, cat.ServiceExpectedEndAt)
		}
	})
}

// TestVehicleRepo_RideSharePauseSurvivesRepeatedReads is the ride-share half.
//
// The existing coverage proves ONE read after a pause. The client's report was
// that the pause came back on after "a few seconds" — i.e. that a LATER read
// disagreed with an earlier one — so the property worth pinning is that nothing
// about merely reading, or about time passing between reads, changes the answer.
func TestVehicleRepo_RideSharePauseSurvivesRepeatedReads(t *testing.T) {
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)

	const (
		ownerID = "user_rs_stable"
		vehicle = "veh_rs_stable_1"
	)
	seedVehicleSummaryRow(t, vehicle, ownerID, "5YJ3E1EA1NF000S31", "Paused", "Model Y", 2026, "Quicksilver", store.VehicleStatusParked, 61, 166)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	if err := repo.SetRideShareEnabled(ctx, vehicle, false); err != nil {
		t.Fatalf("SetRideShareEnabled(false): %v", err)
	}

	// Five rounds of every read path there is. A single read proves persistence;
	// repeated reads prove the answer is not a function of how often it is asked.
	for i := range 5 {
		v, err := repo.GetByID(ctx, vehicle)
		if err != nil {
			t.Fatalf("round %d GetByID: %v", i, err)
		}
		if v.RideShareEnabled {
			t.Fatalf("round %d: the snapshot read un-paused the car", i)
		}

		rows, err := repo.ListSummariesByUser(ctx, ownerID)
		if err != nil {
			t.Fatalf("round %d ListSummariesByUser: %v", i, err)
		}
		if len(rows) != 1 {
			t.Fatalf("round %d: catalog rows = %d, want 1", i, len(rows))
		}
		if rows[0].RideShareEnabled {
			t.Fatalf("round %d: the catalog read un-paused the car", i)
		}

		// The scalar gate the ride-request create path and the reservation
		// sweeper both use — a third read surface, and the one that decides
		// whether a rider is refused.
		enabled, err := repo.RideShareEnabled(ctx, vehicle)
		if err != nil {
			t.Fatalf("round %d RideShareEnabled: %v", i, err)
		}
		if enabled {
			t.Fatalf("round %d: the create gate un-paused the car", i)
		}
	}
}
