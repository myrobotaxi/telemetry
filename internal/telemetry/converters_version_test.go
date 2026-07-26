package telemetry

import (
	"testing"

	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

// TestConvertValue_Version covers MYR-279: the firmware Version field must decode
// to a StringVal verbatim and must NOT be numeric-coerced (so a rare
// numeric-looking version like "2026" is persisted as a version string, not a
// float that the string-typed store column would drop).
func TestConvertValue_Version(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"typical firmware string", "2026.20.1 9a8b7c6", "2026.20.1 9a8b7c6"},
		{"numeric-looking version stays a string", "2026", "2026"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertValue(tpb.Field_Version, &tpb.Value{
				Value: &tpb.Value_StringValue{StringValue: tt.in},
			})
			if err != nil {
				t.Fatalf("convertValue(Version): %v", err)
			}
			if got.StringVal == nil {
				t.Fatalf("Version must decode to StringVal, got %+v", got)
			}
			if *got.StringVal != tt.want {
				t.Errorf("Version = %q, want %q", *got.StringVal, tt.want)
			}
			if got.FloatVal != nil {
				t.Errorf("Version must NOT be numeric-coerced, got FloatVal=%v", *got.FloatVal)
			}
		})
	}
}
