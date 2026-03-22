package simulator

import (
	"math"
	"testing"
)

func TestEncodePolyline_Simple(t *testing.T) {
	// Known example from Google's polyline algorithm documentation.
	// Coordinates: [lng, lat] format. The classic example encodes
	// (38.5, -120.2), (40.7, -120.95), (43.252, -126.453).
	coords := [][2]float64{
		{-120.2, 38.5},
		{-120.95, 40.7},
		{-126.453, 43.252},
	}

	got := EncodePolyline(coords)
	want := "_p~iF~ps|U_ulLnnqC_mqNvxq`@"

	if got != want {
		t.Errorf("EncodePolyline = %q, want %q", got, want)
	}
}

func TestEncodePolyline_RoundTrip(t *testing.T) {
	// Encode then manually verify the encoded string decodes back
	// to the same coordinates (within 5-decimal precision).
	coords := [][2]float64{
		{-96.7970, 32.7767},
		{-96.7960, 32.7780},
		{-96.6153, 33.1972},
	}

	encoded := EncodePolyline(coords)

	// Manually decode to verify.
	decoded := decodeForTest(t, encoded)
	if len(decoded) != len(coords) {
		t.Fatalf("decoded %d coords, want %d", len(decoded), len(coords))
	}

	for i, c := range coords {
		// Polyline has 5 decimal places of precision.
		if math.Abs(decoded[i][1]-c[1]) > 0.00001 {
			t.Errorf("coord[%d] lat = %f, want %f", i, decoded[i][1], c[1])
		}
		if math.Abs(decoded[i][0]-c[0]) > 0.00001 {
			t.Errorf("coord[%d] lng = %f, want %f", i, decoded[i][0], c[0])
		}
	}
}

func TestEncodePolyline_Empty(t *testing.T) {
	encoded := EncodePolyline(nil)
	if encoded != "" {
		t.Errorf("EncodePolyline(nil) = %q, want empty string", encoded)
	}
}

func TestEncodePolyline_SinglePoint(t *testing.T) {
	coords := [][2]float64{{-96.7970, 32.7767}}
	encoded := EncodePolyline(coords)
	if encoded == "" {
		t.Error("EncodePolyline with single point should produce non-empty string")
	}

	decoded := decodeForTest(t, encoded)
	if len(decoded) != 1 {
		t.Fatalf("decoded %d coords, want 1", len(decoded))
	}
}

// decodeForTest is a minimal polyline decoder used only in tests to verify
// the encoder's output. Returns [lng, lat] pairs.
func decodeForTest(t *testing.T, encoded string) [][2]float64 {
	t.Helper()

	var result [][2]float64
	lat, lng := 0, 0
	i := 0

	for i < len(encoded) {
		dlat, n := decodeTestValue(encoded, i)
		i += n
		lat += dlat

		dlng, n := decodeTestValue(encoded, i)
		i += n
		lng += dlng

		result = append(result, [2]float64{
			float64(lng) / 1e5,
			float64(lat) / 1e5,
		})
	}

	return result
}

func decodeTestValue(s string, idx int) (value, consumed int) {
	shift := 0
	for {
		b := int(s[idx]) - 63
		idx++
		consumed++
		value |= (b & 0x1F) << shift
		shift += 5
		if b < 0x20 {
			break
		}
	}
	if value&1 != 0 {
		value = ^(value >> 1)
	} else {
		value >>= 1
	}
	return value, consumed
}
