package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestControlState_MYR320DetailsPersistThenSnapshot is the MYR-320 hydration
// round-trip: persist on the REST-backfill path, then read back later with no
// writer and no socket in between — exactly what a /snapshot for a car sitting
// offline at a service centre does.
//
// The pairs are the point. trim_label must NOT displace trim, and fsd_version
// must NOT displace software_version: each pair holds two values that move
// independently and neither of which can be derived from the other (a raw badge
// code vs a human label; a firmware build vs an FSD designation). A round-trip
// that returned one where the other was expected would be silently wrong on the
// details sheet rather than loudly broken.
func TestControlState_MYR320DetailsPersistThenSnapshot(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureControlMigration(t)

	tests := []struct {
		name        string
		vehID       string
		vin         string
		persist     *store.ControlStateUpdate // nil = never-read row
		wantTrim    *string
		wantLabel   *string
		wantVersion *string
		wantFSD     *string
	}{
		{
			name:  "all four detail strings coexist",
			vehID: "veh_ctl_d_all",
			vin:   "5YJ3E1EA1NF00D001",
			persist: &store.ControlStateUpdate{
				Trim:            sp("p74d"),
				TrimLabel:       sp("Performance"),
				SoftwareVersion: sp("2026.20.1 9a8b7c6"),
				FSDVersion:      sp("FSD (Supervised) v14.3.5"),
			},
			wantTrim:    sp("p74d"),
			wantLabel:   sp("Performance"),
			wantVersion: sp("2026.20.1 9a8b7c6"),
			wantFSD:     sp("FSD (Supervised) v14.3.5"),
		},
		{
			// A car whose vehicle_config carries no performance designation, and
			// whose release_notes we have not read yet. The siblings still land.
			name:  "label and FSD absent while their siblings land",
			vehID: "veh_ctl_d_partial",
			vin:   "5YJ3E1EA1NF00D002",
			persist: &store.ControlStateUpdate{
				Trim:            sp("p74d"),
				SoftwareVersion: sp("2026.20.1 9a8b7c6"),
			},
			wantTrim:    sp("p74d"),
			wantVersion: sp("2026.20.1 9a8b7c6"),
		},
		{
			// fsdVersion arriving on its own is the ordinary shape when the
			// release_notes read succeeds but vehicle_config says nothing new.
			name:  "FSD version alone",
			vehID: "veh_ctl_d_fsdonly",
			vin:   "5YJ3E1EA1NF00D003",
			persist: &store.ControlStateUpdate{
				FSDVersion: sp("FSD (Supervised) v14.3.5"),
			},
			wantFSD: sp("FSD (Supervised) v14.3.5"),
		},
		{
			// Never read. NULL is the honest unknown — for fsdVersion it is
			// emphatically NOT "this car has no FSD".
			name:    "never-read row is nil for both",
			vehID:   "veh_ctl_d_never",
			vin:     "5YJ3E1EA1NF00D004",
			persist: nil,
		},
	}

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanTables(t, testPool)
			cleanControlState(t)
			seedVehicle(t, testPool, tt.vehID, tt.vin)

			if tt.persist != nil {
				if err := repo.UpsertControlState(ctx, tt.vehID, *tt.persist); err != nil {
					t.Fatalf("UpsertControlState: %v", err)
				}
			}

			// The socket gap: no writer, no live stream — just a later read.
			v, err := repo.GetByID(ctx, tt.vehID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			wantStrPtr(t, "Trim", tt.wantTrim, v.Trim)
			wantStrPtr(t, "TrimLabel", tt.wantLabel, v.TrimLabel)
			wantStrPtr(t, "SoftwareVersion", tt.wantVersion, v.SoftwareVersion)
			wantStrPtr(t, "FSDVersion", tt.wantFSD, v.FSDVersion)
		})
	}
}

// TestControlState_MYR320DetailsSurviveALaterBlankFrame is the divergence from
// the MYR-303 media text fields, and it is deliberate. An empty media title
// means "the track ended" — a real observation that MUST overwrite. An empty
// trim label or FSD version means "we learned nothing this time", so the stored
// value has to survive: a partial vehicle_config, or an unreadable release_notes
// endpoint, must never blank a details row the owner is looking at.
//
// The rule is enforced in two places and this test covers the store half: the
// mapper never turns an empty string into a non-nil pointer (see
// TestMapControlStrings_MYR320 in control_state_details_test.go), so what
// actually reaches the upsert after a blank read is a NIL — and the COALESCE
// must keep the stored value rather than writing NULL over it.
func TestControlState_MYR320DetailsSurviveALaterBlankFrame(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureControlMigration(t)
	cleanTables(t, testPool)
	cleanControlState(t)

	const (
		vehID = "veh_ctl_d_empty"
		vin   = "5YJ3E1EA1NF00D005"
	)
	seedVehicle(t, testPool, vehID, vin)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	// Frame 1: a good read.
	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{
		TrimLabel:  sp("Performance"),
		FSDVersion: sp("FSD (Supervised) v14.3.5"),
	}); err != nil {
		t.Fatalf("UpsertControlState (known): %v", err)
	}

	// Frame 2: the shape a blank read produces — both detail pointers nil, some
	// other field present so the upsert actually runs.
	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{
		IsLocked: bp(true),
	}); err != nil {
		t.Fatalf("UpsertControlState (blank details): %v", err)
	}

	v, err := repo.GetByID(ctx, vehID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	wantStrPtr(t, "TrimLabel", sp("Performance"), v.TrimLabel)
	wantStrPtr(t, "FSDVersion", sp("FSD (Supervised) v14.3.5"), v.FSDVersion)
}
