package ws

import (
	"math"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/myrobotaxi/telemetry/internal/telemetry"
	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

// TestRouteLine_EndToEnd exercises the full pipeline from a protobuf
// Payload with a RouteLine Datum through the Decoder and into
// mapFieldsForClient, verifying that the frontend receives
// "navRouteCoordinates" as [][]float64 in [lng, lat] format.
func TestRouteLine_EndToEnd(t *testing.T) {
	t.Parallel()

	// Tesla RouteLine: Base64-encoded protobuf wrapping a Google Encoded
	// Polyline at 1e6 precision. Three points:
	// (38.5, -120.2), (40.7, -120.95), (43.252, -126.453)
	const encodedPolyline = "CiBfaXpsaEF+cmxnZEZfe2dlQ355d2xAX2t3ekNuYHtuSQ=="

	tests := []struct {
		name           string
		datum          *tpb.Datum
		wantCoordCount int
		wantFirst      [2]float64 // [lng, lat] (Mapbox format)
		wantFieldErrs  int
	}{
		{
			name: "string_value polyline decodes to navRouteCoordinates",
			datum: &tpb.Datum{
				Key: tpb.Field_RouteLine,
				Value: &tpb.Value{
					Value: &tpb.Value_StringValue{StringValue: encodedPolyline},
				},
			},
			wantCoordCount: 3,
			wantFirst:      [2]float64{-120.2, 38.5}, // [lng, lat]
			wantFieldErrs:  0,
		},
		{
			name: "non-string value produces field error and no navRouteCoordinates",
			datum: &tpb.Datum{
				Key: tpb.Field_RouteLine,
				Value: &tpb.Value{
					Value: &tpb.Value_IntValue{IntValue: 42},
				},
			},
			wantCoordCount: 0,
			wantFieldErrs:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Build a protobuf Payload containing the RouteLine datum plus
			// a speed field so the payload passes validation (non-empty data).
			payload := &tpb.Payload{
				Vin:       "5YJ3E7EB2NF000001",
				CreatedAt: timestamppb.Now(),
				Data: []*tpb.Datum{
					tt.datum,
					{
						Key: tpb.Field_VehicleSpeed,
						Value: &tpb.Value{
							Value: &tpb.Value_StringValue{StringValue: "65.0"},
						},
					},
				},
			}

			// Phase 1: Decode through telemetry.Decoder
			dec := telemetry.NewDecoder()
			evt, fieldErrs, err := dec.DecodePayload(payload)
			if err != nil {
				t.Fatalf("DecodePayload() error = %v", err)
			}
			if len(fieldErrs) != tt.wantFieldErrs {
				t.Fatalf("got %d field errors, want %d: %v", len(fieldErrs), tt.wantFieldErrs, fieldErrs)
			}

			if tt.wantFieldErrs > 0 {
				// When we expect a field error, routeLine should NOT be in the
				// decoded event fields.
				if _, ok := evt.Fields["routeLine"]; ok {
					t.Fatal("routeLine should not be in fields when field error occurred")
				}

				// mapFieldsForClient should produce no navRouteCoordinates.
				out := mapFieldsForClient(evt.Fields)
				if _, ok := out["navRouteCoordinates"]; ok {
					t.Fatal("navRouteCoordinates should not be present when routeLine had an error")
				}
				return
			}

			// Phase 2: Verify decoded event has routeLine with StringVal
			rl, ok := evt.Fields["routeLine"]
			if !ok {
				t.Fatal("decoded event missing routeLine field")
			}
			if rl.StringVal == nil {
				t.Fatal("routeLine.StringVal is nil")
			}
			if *rl.StringVal != encodedPolyline {
				t.Fatalf("routeLine = %q, want %q", *rl.StringVal, encodedPolyline)
			}

			// Phase 3: Map through mapFieldsForClient
			clientFields := mapFieldsForClient(evt.Fields)
			coords, ok := clientFields["navRouteCoordinates"].([][]float64)
			if !ok {
				t.Fatalf("navRouteCoordinates not [][]float64, got %T", clientFields["navRouteCoordinates"])
			}
			if len(coords) != tt.wantCoordCount {
				t.Fatalf("got %d coordinates, want %d", len(coords), tt.wantCoordCount)
			}

			// Phase 4: Verify [lng, lat] Mapbox format
			if !e2eFloatClose(coords[0][0], tt.wantFirst[0]) ||
				!e2eFloatClose(coords[0][1], tt.wantFirst[1]) {
				t.Fatalf("first coord = [%f, %f], want [%f, %f]",
					coords[0][0], coords[0][1], tt.wantFirst[0], tt.wantFirst[1])
			}
		})
	}
}

// TestRouteLine_ConverterExplicit verifies the explicit RouteLine converter
// in the decoder rejects non-string values with a FieldError.
func TestRouteLine_ConverterExplicit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		value         *tpb.Value
		wantStringVal string
		wantErr       bool
	}{
		{
			name: "string value passes through",
			value: &tpb.Value{
				Value: &tpb.Value_StringValue{StringValue: "_p~iF~ps|U"},
			},
			wantStringVal: "_p~iF~ps|U",
		},
		{
			name: "float value produces field error",
			value: &tpb.Value{
				Value: &tpb.Value_FloatValue{FloatValue: 1.23},
			},
			wantErr: true,
		},
		{
			name: "int value produces field error",
			value: &tpb.Value{
				Value: &tpb.Value_IntValue{IntValue: 42},
			},
			wantErr: true,
		},
		{
			name: "boolean value produces field error",
			value: &tpb.Value{
				Value: &tpb.Value_BooleanValue{BooleanValue: true},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dec := telemetry.NewDecoder()

			payload := &tpb.Payload{
				Vin:       "5YJ3E7EB2NF000001",
				CreatedAt: timestamppb.Now(),
				Data: []*tpb.Datum{
					{Key: tpb.Field_RouteLine, Value: tt.value},
					// Include a second field so the payload is never empty.
					{
						Key:   tpb.Field_VehicleSpeed,
						Value: &tpb.Value{Value: &tpb.Value_StringValue{StringValue: "55"}},
					},
				},
			}

			evt, fieldErrs, err := dec.DecodePayload(payload)
			if err != nil {
				t.Fatalf("DecodePayload() error = %v", err)
			}

			if tt.wantErr {
				if len(fieldErrs) == 0 {
					t.Fatal("expected a field error for non-string RouteLine value")
				}
				if _, ok := evt.Fields["routeLine"]; ok {
					t.Fatal("routeLine should not be in fields when converter returned error")
				}
				return
			}

			if len(fieldErrs) != 0 {
				t.Fatalf("unexpected field errors: %v", fieldErrs)
			}

			rl, ok := evt.Fields["routeLine"]
			if !ok {
				t.Fatal("missing routeLine field")
			}
			if rl.StringVal == nil || *rl.StringVal != tt.wantStringVal {
				t.Fatalf("routeLine = %v, want %q", rl, tt.wantStringVal)
			}
		})
	}
}

// e2eFloatClose returns true if a and b are within 1e-4 of each other.
func e2eFloatClose(a, b float64) bool {
	return math.Abs(a-b) < 1e-4
}
