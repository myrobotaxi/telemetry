package simulator

import "math"

// EncodePolyline encodes a slice of [lng, lat] coordinate pairs into a
// Google Encoded Polyline string. This is the inverse of the decoder in
// internal/ws/polyline.go.
//
// The algorithm rounds lat/lng to 5 decimal places, computes deltas between
// successive points, and encodes each delta as a variable-length ASCII string.
// See: https://developers.google.com/maps/documentation/utilities/polylinealgorithm
func EncodePolyline(coords [][2]float64) string {
	var buf []byte
	prevLat, prevLng := 0, 0

	for _, c := range coords {
		// Coordinates are [lng, lat] — polyline encodes lat first.
		lat := int(math.Round(c[1] * 1e5))
		lng := int(math.Round(c[0] * 1e5))

		buf = appendEncodedValue(buf, lat-prevLat)
		buf = appendEncodedValue(buf, lng-prevLng)

		prevLat = lat
		prevLng = lng
	}

	return string(buf)
}

// appendEncodedValue encodes a single integer delta and appends the
// resulting bytes to buf. Negative values are inverted before encoding.
func appendEncodedValue(buf []byte, val int) []byte {
	// Left-shift and invert if negative.
	v := val << 1
	if val < 0 {
		v = ^v
	}

	// Break into 5-bit chunks, set continuation bit on all but last.
	// Each chunk is 5 bits (0-31) + continuation bit (0 or 32) + 63 offset,
	// so the result is always in [63, 126] — safe for byte conversion.
	for v >= 0x20 {
		// #nosec G115 -- max value of (v&0x1F)|0x20 is 63; +63 = 126, fits in byte
		buf = append(buf, byte((v&0x1F)|0x20)+63) //nolint:gosec // safe: max 126
		v >>= 5
	}
	// #nosec G115 -- v is in [0, 31]; +63 = max 94, fits in byte
	buf = append(buf, byte(v)+63) //nolint:gosec // safe: max 94

	return buf
}
