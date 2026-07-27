package store

import (
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// TestMapTelemetryToControlState_MediaNowPlaying covers the MYR-303 derivation,
// with the empty-string divergence from MYR-298 as the centrepiece.
func TestMapTelemetryToControlState_MediaNowPlaying(t *testing.T) {
	t.Run("text fields map through", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldMediaTitle):          {StringVal: strPtr("Summertime Friends")},
			string(telemetry.FieldMediaArtist):         {StringVal: strPtr("The Chainsmokers")},
			string(telemetry.FieldMediaAlbum):          {StringVal: strPtr("Summertime Friends")},
			string(telemetry.FieldMediaStation):        {StringVal: strPtr("Alt Nation")},
			string(telemetry.FieldMediaPlaybackSource): {StringVal: strPtr("Spotify")},
		})
		if got == nil {
			t.Fatal("expected a non-nil update")
		}
		checks := map[string]struct {
			got  *string
			want string
		}{
			"Title":   {got.MediaNowPlayingTitle, "Summertime Friends"},
			"Artist":  {got.MediaNowPlayingArtist, "The Chainsmokers"},
			"Album":   {got.MediaNowPlayingAlbum, "Summertime Friends"},
			"Station": {got.MediaNowPlayingStation, "Alt Nation"},
			"Source":  {got.MediaPlaybackSource, "Spotify"},
		}
		for name, c := range checks {
			if c.got == nil || *c.got != c.want {
				t.Errorf("%s = %v, want %q", name, c.got, c.want)
			}
		}
	})

	// THE key MYR-303 behaviour. Contrast TestMapTelemetryToControlState_
	// SeatVentMedia's "mediaPlaybackStatus empty is omitted": that field is an
	// enum whose empty/Unknown means "could not read", whereas an empty title
	// means the track ENDED. Dropping it would pin a finished song forever.
	t.Run("empty text is an observation and is KEPT (track ended)", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldMediaTitle): {StringVal: strPtr("")},
		})
		if got == nil {
			t.Fatal("an empty title is a real observation — expected a non-nil update, not nil")
		}
		if got.MediaNowPlayingTitle == nil {
			t.Fatal("empty title must be KEPT as \"\", not dropped to nil — otherwise a stale " +
				"track stays pinned after playback stops")
		}
		if *got.MediaNowPlayingTitle != "" {
			t.Errorf("Title = %q, want empty string", *got.MediaNowPlayingTitle)
		}
	})

	t.Run("empty applies to all five text fields uniformly", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldMediaTitle):          {StringVal: strPtr("")},
			string(telemetry.FieldMediaArtist):         {StringVal: strPtr("")},
			string(telemetry.FieldMediaAlbum):          {StringVal: strPtr("")},
			string(telemetry.FieldMediaStation):        {StringVal: strPtr("")},
			string(telemetry.FieldMediaPlaybackSource): {StringVal: strPtr("")},
		})
		if got == nil {
			t.Fatal("expected a non-nil update")
		}
		for name, p := range map[string]*string{
			"Title": got.MediaNowPlayingTitle, "Artist": got.MediaNowPlayingArtist,
			"Album": got.MediaNowPlayingAlbum, "Station": got.MediaNowPlayingStation,
			"Source": got.MediaPlaybackSource,
		} {
			if p == nil || *p != "" {
				t.Errorf("%s = %v, want a kept empty string", name, p)
			}
		}
	})

	t.Run("absent field stays nil (never observed)", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldMediaTitle): {StringVal: strPtr("Track")},
		})
		if got == nil {
			t.Fatal("expected a non-nil update")
		}
		if got.MediaNowPlayingArtist != nil {
			t.Errorf("absent artist must stay nil, got %v", *got.MediaNowPlayingArtist)
		}
	})

	t.Run("ms counters accept int and rounded float", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldMediaDurationMs): {IntVal: int64Ptr(214000)},
			string(telemetry.FieldMediaElapsedMs):  {FloatVal: floatPtr(41999.6)},
		})
		if got == nil || got.MediaNowPlayingDuration == nil || *got.MediaNowPlayingDuration != 214000 {
			t.Errorf("Duration = %v, want 214000", got.MediaNowPlayingDuration)
		}
		if got.MediaNowPlayingElapsed == nil || *got.MediaNowPlayingElapsed != 42000 {
			t.Errorf("Elapsed = %v, want 42000 (rounded)", got.MediaNowPlayingElapsed)
		}
	})

	// Tesla's radio placeholder. Storing it verbatim is what preserves the
	// client's ability to distinguish "radio" from "never observed"; a server
	// that nulled it would collapse the two.
	t.Run("18000000ms radio sentinel is stored verbatim", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldMediaDurationMs): {IntVal: int64Ptr(18000000)},
		})
		if got == nil || got.MediaNowPlayingDuration == nil || *got.MediaNowPlayingDuration != 18000000 {
			t.Errorf("Duration = %v, want the 18000000 sentinel stored as-is", got.MediaNowPlayingDuration)
		}
	})

	t.Run("zero elapsed is a real observation", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldMediaElapsedMs): {IntVal: int64Ptr(0)},
		})
		if got == nil || got.MediaNowPlayingElapsed == nil || *got.MediaNowPlayingElapsed != 0 {
			t.Errorf("Elapsed = %v, want a kept 0 (track just started)", got.MediaNowPlayingElapsed)
		}
	})

	t.Run("mediaVolumeMax stays fractional", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldMediaVolumeMax): {FloatVal: floatPtr(10.5)},
		})
		if got == nil || got.MediaVolumeMax == nil || *got.MediaVolumeMax != 10.5 {
			t.Errorf("MediaVolumeMax = %v, want 10.5 unrounded (contract types it `number`)",
				got.MediaVolumeMax)
		}
	})

	t.Run("invalid media fields are ignored", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldMediaTitle):      {StringVal: strPtr("Track"), Invalid: true},
			string(telemetry.FieldMediaDurationMs): {IntVal: int64Ptr(1000), Invalid: true},
		})
		if got != nil {
			t.Fatalf("invalid-only frame should map to nil, got %+v", got)
		}
	})
}

// TestMapTelemetryToControlState_SeatCoolingCapable covers MYR-308. A false is an
// authoritative answer, not an absence — that is the asymmetry with the MYR-279
// trim string, whose empty value IS dropped.
func TestMapTelemetryToControlState_SeatCoolingCapable(t *testing.T) {
	for _, want := range []bool{true, false} {
		t.Run("capability persists", func(t *testing.T) {
			got := mapTelemetryToControlState(map[string]events.TelemetryValue{
				string(telemetry.FieldSeatCoolingCapable): {BoolVal: boolPtr(want)},
			})
			if got == nil || got.SeatCoolingCapable == nil || *got.SeatCoolingCapable != want {
				t.Fatalf("SeatCoolingCapable = %v, want %v", got, want)
			}
		})
	}

	t.Run("absent stays nil (unknown, not 'no seat cooling')", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldSeatVentEnabled): {BoolVal: boolPtr(true)},
		})
		if got == nil {
			t.Fatal("expected a non-nil update")
		}
		if got.SeatCoolingCapable != nil {
			t.Errorf("SeatCoolingCapable = %v, want nil", *got.SeatCoolingCapable)
		}
	})
}

// TestMergeControlState_MediaNowPlayingLastWriteWins proves the writer's
// coalescing buffer folds these per field — and critically that an empty string
// is treated as a present value, so "the track ended" survives the buffer and
// reaches the upsert instead of being swallowed before it ever gets there.
func TestMergeControlState_MediaNowPlayingLastWriteWins(t *testing.T) {
	dst := &ControlStateUpdate{
		MediaNowPlayingTitle:    strPtr("Old Track"),
		MediaNowPlayingArtist:   strPtr("Old Artist"),
		MediaNowPlayingDuration: int64Ptr(214000),
		MediaVolumeMax:          floatPtr(11),
		SeatCoolingCapable:      boolPtr(true),
	}
	src := &ControlStateUpdate{
		MediaNowPlayingTitle:   strPtr(""), // track ended — must overwrite
		MediaNowPlayingElapsed: int64Ptr(0),
	}
	mergeControlState(dst, src)

	if dst.MediaNowPlayingTitle == nil || *dst.MediaNowPlayingTitle != "" {
		t.Errorf("Title = %v, want the empty string to win (track ended)", dst.MediaNowPlayingTitle)
	}
	if dst.MediaNowPlayingArtist == nil || *dst.MediaNowPlayingArtist != "Old Artist" {
		t.Errorf("Artist = %v, want the prior value preserved (absent in src)", dst.MediaNowPlayingArtist)
	}
	if dst.MediaNowPlayingDuration == nil || *dst.MediaNowPlayingDuration != 214000 {
		t.Errorf("Duration = %v, want preserved", dst.MediaNowPlayingDuration)
	}
	if dst.MediaNowPlayingElapsed == nil || *dst.MediaNowPlayingElapsed != 0 {
		t.Errorf("Elapsed = %v, want 0 from src", dst.MediaNowPlayingElapsed)
	}
	if dst.MediaVolumeMax == nil || *dst.MediaVolumeMax != 11 {
		t.Errorf("MediaVolumeMax = %v, want preserved", dst.MediaVolumeMax)
	}
	if dst.SeatCoolingCapable == nil || !*dst.SeatCoolingCapable {
		t.Errorf("SeatCoolingCapable = %v, want preserved", dst.SeatCoolingCapable)
	}
}

// TestControlStateUpdate_HasAnyMediaAndCapability proves the writer does not skip
// the side-table upsert for a frame carrying only a MYR-303/308 field — including
// the empty-string "track ended" case, which is exactly the frame most at risk of
// being mistaken for "no fields present".
func TestControlStateUpdate_HasAnyMediaAndCapability(t *testing.T) {
	tests := []struct {
		name   string
		update ControlStateUpdate
	}{
		{"title only", ControlStateUpdate{MediaNowPlayingTitle: strPtr("Track")}},
		{"EMPTY title only (track ended)", ControlStateUpdate{MediaNowPlayingTitle: strPtr("")}},
		{"station only", ControlStateUpdate{MediaNowPlayingStation: strPtr("Alt Nation")}},
		{"source only", ControlStateUpdate{MediaPlaybackSource: strPtr("Bluetooth")}},
		{"duration only", ControlStateUpdate{MediaNowPlayingDuration: int64Ptr(1)}},
		{"zero elapsed only", ControlStateUpdate{MediaNowPlayingElapsed: int64Ptr(0)}},
		{"volume max only", ControlStateUpdate{MediaVolumeMax: floatPtr(11)}},
		{"capability true only", ControlStateUpdate{SeatCoolingCapable: boolPtr(true)}},
		{"capability false only", ControlStateUpdate{SeatCoolingCapable: boolPtr(false)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.update.HasAny() {
				t.Error("HasAny() = false — the writer would skip persisting this frame entirely")
			}
		})
	}

	if (&ControlStateUpdate{}).HasAny() {
		t.Error("an empty update must report HasAny() = false")
	}
}
