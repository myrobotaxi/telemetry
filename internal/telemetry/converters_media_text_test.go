package telemetry

import (
	"testing"

	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

// stringDatum builds a Tesla Datum carrying a string_value, which is how the
// vehicle sends every MYR-303 free-text media field.
func stringDatum(field tpb.Field, s string) *tpb.Datum {
	return &tpb.Datum{
		Key:   field,
		Value: &tpb.Value{Value: &tpb.Value_StringValue{StringValue: s}},
	}
}

// TestConvertMediaFreeTextNeverNumericCoerced pins the MYR-303 free-text fields
// against the numeric-coercion trap parseStringValue sets for every field
// WITHOUT a fieldConverters entry.
//
// The fallback converter tries strconv.ParseFloat first and, on success, returns
// a FloatVal with no StringVal at all. Both consumers of these five fields
// require StringVal — store.mapControlMediaNowPlaying (control_state_media.go)
// and the WS field mapping — so a numerically-parseable value is silently
// DROPPED: never persisted, never broadcast, and indistinguishable on read from
// "the car never told us". Real media metadata hits this constantly: track
// titles ("1979", "22", "7"), album names ("21", "1989"), and FM station labels
// ("94.7") all parse as floats.
//
// This is the exact defect convertVersion (MYR-279) was added to prevent for
// softwareVersion; these five needed the same treatment and did not get it.
func TestConvertMediaFreeTextNeverNumericCoerced(t *testing.T) {
	t.Parallel()

	fields := map[string]tpb.Field{
		"title":   tpb.Field_MediaNowPlayingTitle,
		"artist":  tpb.Field_MediaNowPlayingArtist,
		"album":   tpb.Field_MediaNowPlayingAlbum,
		"station": tpb.Field_MediaNowPlayingStation,
		"source":  tpb.Field_MediaPlaybackSource,
	}

	// Every one of these is a real-world value that ParseFloat accepts.
	values := []string{"1979", "22", "7", "1989", "94.7", "-1", "1e5", "0"}

	for name, field := range fields {
		for _, want := range values {
			t.Run(name+"/"+want, func(t *testing.T) {
				t.Parallel()
				tv, err := extractValue(stringDatum(field, want))
				if err != nil {
					t.Fatalf("extractValue: %v", err)
				}
				if tv.StringVal == nil {
					t.Fatalf("%s %q decoded to StringVal=nil (FloatVal=%v) — "+
						"the store's media mapper requires StringVal, so this value is dropped",
						name, want, tv.FloatVal)
				}
				if *tv.StringVal != want {
					t.Errorf("%s = %q, want %q (verbatim)", name, *tv.StringVal, want)
				}
				if tv.FloatVal != nil {
					t.Errorf("%s produced a FloatVal (%v); free text must never be numeric-coerced",
						name, *tv.FloatVal)
				}
			})
		}
	}
}

// TestConvertMediaFreeTextKeepsEmptyString guards the MYR-303 empty-string
// semantics through the converter: an empty title is the car reporting that the
// track ENDED — a real observation that must reach the store as a non-nil empty
// StringVal so it wins the COALESCE upsert and clears a stale track. Returning
// a nil StringVal here would pin the last known title forever.
func TestConvertMediaFreeTextKeepsEmptyString(t *testing.T) {
	t.Parallel()

	tv, err := extractValue(stringDatum(tpb.Field_MediaNowPlayingTitle, ""))
	if err != nil {
		t.Fatalf("extractValue: %v", err)
	}
	if tv.StringVal == nil {
		t.Fatal("empty title decoded to StringVal=nil; the track-ended observation is lost")
	}
	if *tv.StringVal != "" {
		t.Errorf("empty title = %q, want \"\"", *tv.StringVal)
	}
}

// TestConvertMediaFreeTextPassesThroughOrdinaryText is the non-regression half:
// text that never parsed as a number must behave exactly as it did before.
func TestConvertMediaFreeTextPassesThroughOrdinaryText(t *testing.T) {
	t.Parallel()

	const want = "Bohemian Rhapsody"
	tv, err := extractValue(stringDatum(tpb.Field_MediaNowPlayingTitle, want))
	if err != nil {
		t.Fatalf("extractValue: %v", err)
	}
	if tv.StringVal == nil || *tv.StringVal != want {
		t.Errorf("title = %v, want %q", tv.StringVal, want)
	}
}
