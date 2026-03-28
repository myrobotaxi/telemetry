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
