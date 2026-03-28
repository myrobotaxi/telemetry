package ws

import "fmt"

// DecodePolyline decodes a Google Encoded Polyline string into a slice of
// [lat, lng] coordinate pairs. The algorithm is documented at:
// https://developers.google.com/maps/documentation/utilities/polylinealgorithm
//
// Tesla vehicles may truncate long route polylines, producing strings that
// end mid-value. This decoder is tolerant of truncation: it returns all
// complete coordinate pairs decoded before the truncation point. An error
// is only returned if zero coordinates could be decoded.
func DecodePolyline(encoded string) ([][]float64, error) {
	var coords [][]float64
	lat, lng := 0, 0
	i := 0

	for i < len(encoded) {
		dlat, n, err := decodeNextValue(encoded, i)
		if err != nil {
			// Truncated mid-latitude — return what we have.
			break
		}
		i += n
		lat += dlat

		dlng, n, err := decodeNextValue(encoded, i)
		if err != nil {
			// Truncated mid-longitude — return what we have.
			break
		}
		i += n
		lng += dlng

		coords = append(coords, []float64{
			float64(lat) / 1e5,
			float64(lng) / 1e5,
		})
	}

	if len(coords) == 0 {
		return nil, fmt.Errorf("DecodePolyline: no valid coordinates in polyline (len=%d)", len(encoded))
	}
	return coords, nil
}

// decodeNextValue reads one encoded integer from the polyline string starting
// at position idx. Returns the decoded value, the number of bytes consumed,
// and any error.
func decodeNextValue(encoded string, idx int) (value, consumed int, err error) {
	shift := 0

	for {
		if idx >= len(encoded) {
			return 0, 0, fmt.Errorf("unexpected end of polyline string")
		}
		b := int(encoded[idx]) - 63
		idx++
		consumed++

		value |= (b & 0x1F) << shift
		shift += 5

		if b < 0x20 {
			break
		}
	}

	// If the least-significant bit is set, the value is negative.
	if value&1 != 0 {
		value = ^(value >> 1)
	} else {
		value >>= 1
	}

	return value, consumed, nil
}
