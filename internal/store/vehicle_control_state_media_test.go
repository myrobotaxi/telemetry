package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestControlState_MediaNowPlayingPersistThenSnapshotAcrossSocketGap is the
// MYR-303 hydration round-trip: persist on the live path, then read back later
// with no writer and no socket in between, exactly as a /snapshot for a sleeping
// car does.
//
// The third and fourth cases are the point of the whole issue: an EMPTY string
// and a NEVER-READ column must round-trip as two DIFFERENT values ("" vs NULL),
// because they mean different things to the client — "nothing is playing" versus
// "we have never heard".
func TestControlState_MediaNowPlayingPersistThenSnapshotAcrossSocketGap(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureControlMigration(t)

	tests := []struct {
		name         string
		vehID        string
		vin          string
		persist      *store.ControlStateUpdate // nil = never-read row
		wantTitle    *string
		wantArtist   *string
		wantStation  *string
		wantDuration *int64
		wantVolMax   *float64
	}{
		{
			name:  "full now-playing block",
			vehID: "veh_ctl_np_full",
			vin:   "5YJ3E1EA1NF00NP01",
			persist: &store.ControlStateUpdate{
				MediaNowPlayingTitle:    sp("Summertime Friends"),
				MediaNowPlayingArtist:   sp("The Chainsmokers"),
				MediaNowPlayingStation:  sp("Alt Nation"),
				MediaNowPlayingDuration: i64p(214000),
				MediaVolumeMax:          fp(11),
			},
			wantTitle:    sp("Summertime Friends"),
			wantArtist:   sp("The Chainsmokers"),
			wantStation:  sp("Alt Nation"),
			wantDuration: i64p(214000),
			wantVolMax:   fp(11),
		},
		{
			name:  "radio: 18000000ms sentinel survives verbatim",
			vehID: "veh_ctl_np_radio",
			vin:   "5YJ3E1EA1NF00NP02",
			persist: &store.ControlStateUpdate{
				MediaNowPlayingStation:  sp("Alt Nation"),
				MediaNowPlayingDuration: i64p(18000000),
			},
			wantStation:  sp("Alt Nation"),
			wantDuration: i64p(18000000),
		},
		{
			// Observed-and-cleared. Must come back as "" — NOT null, and NOT the
			// previously known title.
			name:  "EMPTY string round-trips as empty, not null",
			vehID: "veh_ctl_np_empty",
			vin:   "5YJ3E1EA1NF00NP03",
			persist: &store.ControlStateUpdate{
				MediaNowPlayingTitle:  sp(""),
				MediaNowPlayingArtist: sp(""),
			},
			wantTitle:  sp(""),
			wantArtist: sp(""),
		},
		{
			// Never observed. Must come back as null — the honest unknown.
			name:    "never-read row is nil, distinct from empty",
			vehID:   "veh_ctl_np_never",
			vin:     "5YJ3E1EA1NF00NP04",
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
			wantStrPtr(t, "MediaNowPlayingTitle", tt.wantTitle, v.MediaNowPlayingTitle)
			wantStrPtr(t, "MediaNowPlayingArtist", tt.wantArtist, v.MediaNowPlayingArtist)
			wantStrPtr(t, "MediaNowPlayingStation", tt.wantStation, v.MediaNowPlayingStation)
			wantInt64Ptr(t, "MediaNowPlayingDuration", tt.wantDuration, v.MediaNowPlayingDuration)
			wantFloatPtr(t, "MediaVolumeMax", tt.wantVolMax, v.MediaVolumeMax)
		})
	}
}

// TestControlState_MediaEmptyOverwritesKnownTitle is the behaviour MYR-303 exists
// for, proven end-to-end through the real COALESCE upsert: a track plays, then
// stops, and the stored title must become "" rather than staying pinned to the
// finished song. This is precisely where the MYR-298 "Unknown is omitted" rule
// would have produced the wrong answer.
func TestControlState_MediaEmptyOverwritesKnownTitle(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureControlMigration(t)
	cleanTables(t, testPool)
	cleanControlState(t)

	const (
		vehID = "veh_ctl_np_clear"
		vin   = "5YJ3E1EA1NF00NP05"
	)
	seedVehicle(t, testPool, vehID, vin)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	// A track is playing.
	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{
		MediaNowPlayingTitle:   sp("Summertime Friends"),
		MediaNowPlayingArtist:  sp("The Chainsmokers"),
		MediaNowPlayingElapsed: i64p(42000),
	}); err != nil {
		t.Fatalf("UpsertControlState (playing): %v", err)
	}

	// Playback stops: the car emits empty strings.
	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{
		MediaNowPlayingTitle:  sp(""),
		MediaNowPlayingArtist: sp(""),
	}); err != nil {
		t.Fatalf("UpsertControlState (cleared): %v", err)
	}

	v, err := repo.GetByID(ctx, vehID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if v.MediaNowPlayingTitle == nil || *v.MediaNowPlayingTitle != "" {
		t.Errorf("MediaNowPlayingTitle = %v, want \"\" — a finished track must not stay pinned",
			v.MediaNowPlayingTitle)
	}
	if v.MediaNowPlayingArtist == nil || *v.MediaNowPlayingArtist != "" {
		t.Errorf("MediaNowPlayingArtist = %v, want \"\"", v.MediaNowPlayingArtist)
	}
	// Per-field COALESCE: the untouched elapsed value is preserved.
	if v.MediaNowPlayingElapsed == nil || *v.MediaNowPlayingElapsed != 42000 {
		t.Errorf("MediaNowPlayingElapsed = %v, want 42000 preserved (absent from the second frame)",
			v.MediaNowPlayingElapsed)
	}
}

// TestControlState_SeatCoolingCapableRoundTrip covers MYR-308 persistence,
// including the three-way distinction the contract depends on: true, an
// AUTHORITATIVE false, and a never-read NULL that clients must read as unknown.
func TestControlState_SeatCoolingCapableRoundTrip(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureControlMigration(t)

	tests := []struct {
		name    string
		vehID   string
		vin     string
		persist *store.ControlStateUpdate
		want    *bool
	}{
		{
			name:    "true (the client's car)",
			vehID:   "veh_ctl_scc_true",
			vin:     "5YJ3E1EA1NF00SCC1",
			persist: &store.ControlStateUpdate{SeatCoolingCapable: bp(true)},
			want:    bp(true),
		},
		{
			name:    "false is authoritative and persists",
			vehID:   "veh_ctl_scc_false",
			vin:     "5YJ3E1EA1NF00SCC2",
			persist: &store.ControlStateUpdate{SeatCoolingCapable: bp(false)},
			want:    bp(false),
		},
		{
			name:    "never read is nil, NOT false",
			vehID:   "veh_ctl_scc_never",
			vin:     "5YJ3E1EA1NF00SCC3",
			persist: nil,
			want:    nil,
		},
		{
			// A car whose row exists for other reasons must still report the
			// capability as unknown rather than inheriting a default.
			name:    "row exists via another field, capability still nil",
			vehID:   "veh_ctl_scc_other",
			vin:     "5YJ3E1EA1NF00SCC4",
			persist: &store.ControlStateUpdate{SeatVentEnabled: bp(true)},
			want:    nil,
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
			v, err := repo.GetByID(ctx, tt.vehID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			wantBoolPtr(t, "SeatCoolingCapable", tt.want, v.SeatCoolingCapable)
		})
	}
}

// TestControlState_MediaPerFieldLastWriterWins proves the widened COALESCE upsert
// still updates only the columns a frame carries — a nine-column addition is
// exactly where a mis-numbered placeholder would silently null unrelated fields.
func TestControlState_MediaPerFieldLastWriterWins(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureControlMigration(t)
	cleanTables(t, testPool)
	cleanControlState(t)

	const (
		vehID = "veh_ctl_np_lww"
		vin   = "5YJ3E1EA1NF00NP06"
	)
	seedVehicle(t, testPool, vehID, vin)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	// Frame 1: a broad mix spanning older issues and the new columns.
	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{
		IsLocked:               bp(true),
		SeatVentEnabled:        bp(true),
		MediaPlaybackStatus:    sp("Playing"),
		MediaVolume:            fp(7.5),
		Trim:                   sp("Performance"),
		MediaNowPlayingTitle:   sp("Summertime Friends"),
		MediaPlaybackSource:    sp("Spotify"),
		MediaNowPlayingElapsed: i64p(1000),
		MediaVolumeMax:         fp(11),
		SeatCoolingCapable:     bp(true),
	}); err != nil {
		t.Fatalf("UpsertControlState (frame 1): %v", err)
	}

	// Frame 2: only the playback position moved.
	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{
		MediaNowPlayingElapsed: i64p(45000),
	}); err != nil {
		t.Fatalf("UpsertControlState (frame 2): %v", err)
	}

	v, err := repo.GetByID(ctx, vehID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if v.MediaNowPlayingElapsed == nil || *v.MediaNowPlayingElapsed != 45000 {
		t.Errorf("MediaNowPlayingElapsed = %v, want 45000", v.MediaNowPlayingElapsed)
	}
	wantStrPtr(t, "MediaNowPlayingTitle", sp("Summertime Friends"), v.MediaNowPlayingTitle)
	wantStrPtr(t, "MediaPlaybackSource", sp("Spotify"), v.MediaPlaybackSource)
	wantFloatPtr(t, "MediaVolumeMax", fp(11), v.MediaVolumeMax)
	wantBoolPtr(t, "SeatCoolingCapable", bp(true), v.SeatCoolingCapable)
	// Columns owned by earlier issues must be untouched by the widened upsert.
	wantBoolPtr(t, "IsLocked", bp(true), v.IsLocked)
	wantBoolPtr(t, "SeatVentEnabled", bp(true), v.SeatVentEnabled)
	wantStrPtr(t, "MediaPlaybackStatus", sp("Playing"), v.MediaPlaybackStatus)
	wantFloatPtr(t, "MediaVolume", fp(7.5), v.MediaVolume)
	wantStrPtr(t, "Trim", sp("Performance"), v.Trim)
}

func i64p(i int64) *int64 { return &i }

func wantInt64Ptr(t *testing.T, field string, want, got *int64) {
	t.Helper()
	switch {
	case want == nil && got == nil:
		return
	case want == nil || got == nil:
		t.Errorf("%s: want %v, got %v", field, want, got)
	case *want != *got:
		t.Errorf("%s: want %d, got %d", field, *want, *got)
	}
}
