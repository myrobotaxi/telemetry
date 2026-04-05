package ws

import (
	"math"
	"testing"
)

func TestDecodePolyline(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
		want    [][]float64
		wantErr bool
	}{
		{
			name:    "Google example polyline",
			encoded: "_p~iF~ps|U_ulLnnqC_mqNvxq`@",
			want: [][]float64{
				{38.5, -120.2},
				{40.7, -120.95},
				{43.252, -126.453},
			},
		},
		{
			name:    "single point",
			encoded: "_p~iF~ps|U",
			want: [][]float64{
				{38.5, -120.2},
			},
		},
		{
			name:    "empty string",
			encoded: "",
			wantErr: true,
		},
		{
			name:    "truncated mid-latitude returns complete pairs before truncation",
			encoded: "_p~iF~ps|U_u",
			want: [][]float64{
				{38.5, -120.2},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodePolyline(tt.encoded)
			if (err != nil) != tt.wantErr {
				t.Fatalf("DecodePolyline() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d coords, want %d", len(got), len(tt.want))
			}
			for i := range tt.want {
				if !coordsClose(got[i], tt.want[i]) {
					t.Errorf("coord[%d] = [%f, %f], want [%f, %f]",
						i, got[i][0], got[i][1], tt.want[i][0], tt.want[i][1])
				}
			}
		})
	}
}

func TestDecodePolyline_MultiPoint(t *testing.T) {
	// "a~l~Fjk~uOwHJy@P" encodes three points near Chicago (41.85, -87.65).
	// Verified against Google's interactive polyline utility.
	encoded := "a~l~Fjk~uOwHJy@P"
	coords, err := DecodePolyline(encoded)
	if err != nil {
		t.Fatalf("DecodePolyline(%q): %v", encoded, err)
	}
	if len(coords) != 3 {
		t.Fatalf("expected 3 coordinates, got %d", len(coords))
	}
	// First point should be near Chicago.
	if coords[0][0] < 41.0 || coords[0][0] > 42.0 {
		t.Errorf("first lat %f not near expected range [41, 42]", coords[0][0])
	}
	if coords[0][1] < -88.0 || coords[0][1] > -87.0 {
		t.Errorf("first lng %f not near expected range [-88, -87]", coords[0][1])
	}
}

// coordsClose returns true if two [lat, lng] pairs are within 1e-4 of each other.
func coordsClose(a, b []float64) bool {
	return math.Abs(a[0]-b[0]) < 1e-4 && math.Abs(a[1]-b[1]) < 1e-4
}

func TestDecodeRouteLine_RealTeslaData(t *testing.T) {
	// Real Base64-encoded protobuf from Tesla RouteLine field (truncated for
	// test brevity). Field 1 contains a Google Encoded Polyline at 1e6
	// precision. First coordinate should be in the Dallas/Plano TX area
	// (~32.87°N, ~-96.77°W).
	//
	// Protobuf structure: tag=0x0a (field 1, wire type 2), varint length,
	// then the polyline string.
	encoded := "CjRnfWZ1fUBwYXJxd0R9eEBsQGdKTH1JTWtHTG1JP3tGP29OTWFTP19jQD95YEBPc0w/Z0Q/"

	coords, err := DecodeRouteLine(encoded)
	if err != nil {
		t.Fatalf("DecodeRouteLine() error: %v", err)
	}
	if len(coords) == 0 {
		t.Fatal("expected coordinates, got none")
	}

	// First point should be near Dallas/Plano TX (lat ~32.87, lng ~-96.77).
	first := coords[0]
	if first[0] < 32.0 || first[0] > 34.0 {
		t.Errorf("first lat %f not in Dallas area [32, 34]", first[0])
	}
	if first[1] < -98.0 || first[1] > -96.0 {
		t.Errorf("first lng %f not in Dallas area [-98, -96]", first[1])
	}
}

func TestDecodeRouteLine_InvalidBase64(t *testing.T) {
	_, err := DecodeRouteLine("not-valid-base64!!!")
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestDecodeRouteLine_EmptyProto(t *testing.T) {
	_, err := DecodeRouteLine("AA==") // single zero byte
	if err == nil {
		t.Fatal("expected error for empty/invalid proto")
	}
}
