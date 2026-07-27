package telemetry

import (
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

// TestMediaNowPlayingFieldConfig pins the MYR-303 fleet-config subscription.
// These eight fields are NEW subscriptions, so every already-linked VIN needs a
// fleet-config re-push before the car emits them at all — the pin exists so a
// later edit cannot silently drop one and leave the now-playing panel empty on
// every vehicle.
func TestMediaNowPlayingFieldConfig(t *testing.T) {
	t.Parallel()

	fields := DefaultFieldConfig()

	for _, name := range []string{
		FleetFieldMediaTitle,
		FleetFieldMediaArtist,
		FleetFieldMediaAlbum,
		FleetFieldMediaStation,
		FleetFieldMediaPlaybackSource,
		FleetFieldMediaDuration,
		FleetFieldMediaElapsed,
		FleetFieldMediaVolumeMax,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fc, ok := fields[name]
			if !ok {
				t.Fatalf("field %q missing from DefaultFieldConfig — the car will never emit it", name)
			}
			if fc.IntervalSeconds != 10 {
				t.Errorf("interval_seconds = %d, want 10 (matches the MYR-252 media siblings)",
					fc.IntervalSeconds)
			}
			// MYR-300 lesson: Tesla emits the Media group on change ONLY, so
			// without a resend a server that reconnects mid-track never
			// re-learns the now-playing block. mediaVolumeMax is the sharpest
			// case — a near-constant per-vehicle ceiling that may otherwise
			// never be re-emitted after the first connect.
			if fc.ResendIntervalSeconds == nil {
				t.Fatalf("field %q must set ResendIntervalSeconds so the value re-emits after "+
					"a reconnect (MYR-300)", name)
			}
			if *fc.ResendIntervalSeconds != 120 {
				t.Errorf("resend_interval_seconds = %d, want 120 (matches defaultStreamFreshness)",
					*fc.ResendIntervalSeconds)
			}
			// On-change semantics: no minimum-delta gate. A delta gate on a
			// text field is meaningless, and on the ms counters it would
			// suppress ordinary track progress.
			if fc.MinimumDelta != nil {
				t.Errorf("minimum_delta = %f, want nil (on-change semantics)", *fc.MinimumDelta)
			}
		})
	}
}

// TestMediaNowPlayingFieldMap pins the proto→internal-name decode mapping,
// including the two deliberate contractions. Each proto number is asserted
// directly so a re-vendored vehicle_data.proto that renumbered a field fails
// here rather than silently decoding a title into the artist slot.
func TestMediaNowPlayingFieldMap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		proto     tpb.Field
		protoNum  int32
		wantField FieldName
	}{
		{tpb.Field_MediaNowPlayingTitle, 248, FieldMediaTitle},
		{tpb.Field_MediaNowPlayingArtist, 247, FieldMediaArtist},
		{tpb.Field_MediaNowPlayingAlbum, 249, FieldMediaAlbum},
		{tpb.Field_MediaNowPlayingStation, 250, FieldMediaStation},
		{tpb.Field_MediaPlaybackSource, 243, FieldMediaPlaybackSource},
		// The `Ms` suffix is the contract's unit declaration, not decoration.
		{tpb.Field_MediaNowPlayingDuration, 245, FieldMediaDurationMs},
		{tpb.Field_MediaNowPlayingElapsed, 246, FieldMediaElapsedMs},
		// MediaAudioVolumeMax contracts to mediaVolumeMax exactly as
		// MediaAudioVolume (244) contracts to mediaVolume.
		{tpb.Field_MediaAudioVolumeMax, 252, FieldMediaVolumeMax},
	}

	for _, tt := range tests {
		t.Run(string(tt.wantField), func(t *testing.T) {
			t.Parallel()
			if int32(tt.proto) != tt.protoNum {
				t.Errorf("proto number = %d, want %d — vendored vehicle_data.proto renumbered?",
					int32(tt.proto), tt.protoNum)
			}
			got, ok := InternalFieldName(tt.proto)
			if !ok {
				t.Fatalf("proto %v is not tracked — it will be decoded and dropped", tt.proto)
			}
			if got != tt.wantField {
				t.Errorf("internal name = %q, want %q", got, tt.wantField)
			}
			if !IsTrackedField(tt.proto) {
				t.Error("IsTrackedField = false, want true")
			}
		})
	}
}

// TestMediaWireNamesMatchInternalNames locks the convention that these internal
// names EQUAL their wire names, which is what lets internal/ws pass them through
// with no translate-table entry. If someone renames an internal constant without
// updating the contract (or vice versa) the field would silently reach clients
// under the wrong key.
func TestMediaWireNamesMatchInternalNames(t *testing.T) {
	t.Parallel()

	want := map[FieldName]string{
		FieldMediaTitle:          "mediaNowPlayingTitle",
		FieldMediaArtist:         "mediaNowPlayingArtist",
		FieldMediaAlbum:          "mediaNowPlayingAlbum",
		FieldMediaStation:        "mediaNowPlayingStation",
		FieldMediaPlaybackSource: "mediaPlaybackSource",
		FieldMediaDurationMs:     "mediaNowPlayingDurationMs",
		FieldMediaElapsedMs:      "mediaNowPlayingElapsedMs",
		FieldMediaVolumeMax:      "mediaVolumeMax",
	}
	for field, wire := range want {
		if string(field) != wire {
			t.Errorf("internal name %q must equal the wire name %q", field, wire)
		}
	}
}

// TestSeatCoolingCapableIsNotStreamSourced is the MYR-308 structural guard.
// seatCoolingCapable rides the REST path past the MYR-300 stream-recency gate
// ONLY because it is absent from fieldMap: streamSourcedFields is built from
// fieldMap, and dropStreamSourcedFields deletes exactly that set. Adding a proto
// mapping for it — however plausible it might look — would make a
// busily-streaming car silently unable to acquire the capability.
func TestSeatCoolingCapableIsNotStreamSourced(t *testing.T) {
	t.Parallel()

	for proto, name := range fieldMap {
		if name == FieldSeatCoolingCapable {
			t.Fatalf("seatCoolingCapable must NOT be in fieldMap (found under proto %v): it is "+
				"REST-only, and a fieldMap entry would make the MYR-300 gate drop it", proto)
		}
	}

	if _, streamed := streamSourcedFields[string(FieldSeatCoolingCapable)]; streamed {
		t.Error("seatCoolingCapable must not be stream-sourced — the MYR-300 gate would drop it")
	}
	// Sanity: the REST-only sibling behaves the same way.
	if _, streamed := streamSourcedFields[string(FieldTrim)]; streamed {
		t.Error("trim must not be stream-sourced (MYR-279 precedent)")
	}
	// ...and a genuinely streamed media field IS in the set, proving the
	// assertions above are not vacuous.
	if _, streamed := streamSourcedFields[string(FieldMediaTitle)]; !streamed {
		t.Error("mediaNowPlayingTitle should be stream-sourced — it has a proto and streams")
	}
}

// TestSeatCoolingCapableSurvivesFreshStreamGate exercises the real gate rather
// than just its inputs: with a fresh stream, a REST backfill frame keeps the
// REST-only capability and loses the stream-sourceable media title.
func TestSeatCoolingCapableSurvivesFreshStreamGate(t *testing.T) {
	t.Parallel()

	title := "Summertime Friends"
	capable := true
	trim := "Performance"

	frame := map[string]events.TelemetryValue{
		string(FieldMediaTitle):         {StringVal: &title},
		string(FieldSeatCoolingCapable): {BoolVal: &capable},
		string(FieldTrim):               {StringVal: &trim},
	}

	dropped := dropStreamSourcedFields(frame)
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1 (only the streamed media title)", dropped)
	}
	if _, ok := frame[string(FieldMediaTitle)]; ok {
		t.Error("mediaNowPlayingTitle should have been dropped — it is stream-sourceable")
	}
	if _, ok := frame[string(FieldSeatCoolingCapable)]; !ok {
		t.Error("seatCoolingCapable must survive the gate — an in-service car has no stream at all")
	}
	if _, ok := frame[string(FieldTrim)]; !ok {
		t.Error("trim must survive the gate (MYR-279 precedent)")
	}
}
