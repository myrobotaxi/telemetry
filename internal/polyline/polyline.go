package polyline

import (
	"encoding/base64"
	"fmt"
)

// teslaPrecision is the coordinate precision Tesla uses in its RouteLine
// polylines. Standard Google Encoded Polylines use 1e5; Tesla uses 1e6.
const teslaPrecision = 1e6

// DecodeRouteLine decodes Tesla's RouteLine field value. Tesla wraps a
// Google Encoded Polyline inside a protobuf message (field 1 = polyline
// string) and Base64-encodes the whole thing. The polyline uses 1e6
// precision instead of the standard 1e5.
//
// Returns [lat, lng] pairs (Google convention). The caller must swap to
// [lng, lat] for Mapbox/GeoJSON.
func DecodeRouteLine(encoded string) ([][]float64, error) {
	// Step 1: Base64-decode to get protobuf bytes.
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Try with padding variants.
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("DecodeRouteLine: base64 decode: %w", err)
		}
	}

	// Step 2: Extract the polyline string from protobuf field 1.
	polyline, err := extractProtoStringField1(raw)
	if err != nil {
		return nil, fmt.Errorf("DecodeRouteLine: %w", err)
	}

	// Step 3: Decode the Google Encoded Polyline with Tesla's 1e6 precision.
	coords, err := decodePolylineWithPrecision(polyline, teslaPrecision)
	if err != nil {
		return nil, fmt.Errorf("DecodeRouteLine: %w", err)
	}
	return coords, nil
}

// extractProtoStringField1 extracts a string from protobuf field 1
// (tag 0x0a = field 1, wire type 2). This is the minimal protobuf
// parsing needed for Tesla's RouteLine wrapper message.
func extractProtoStringField1(data []byte) (string, error) {
	if len(data) < 2 {
		return "", fmt.Errorf("protobuf too short (%d bytes)", len(data))
	}
	// Expect tag 0x0a = field 1, wire type 2 (length-delimited).
	if data[0] != 0x0a {
		return "", fmt.Errorf("unexpected protobuf tag: 0x%02x (want 0x0a)", data[0])
	}
	// Read varint length.
	length, bytesRead := decodeVarint(data[1:])
	if bytesRead == 0 {
		return "", fmt.Errorf("invalid varint length")
	}
	start := 1 + bytesRead
	end := start + length
	if end > len(data) {
		// Truncated — use what we have.
		end = len(data)
	}
	return string(data[start:end]), nil
}

// decodeVarint reads a protobuf varint from buf. Returns the value and
// the number of bytes consumed. Returns (0, 0) on error.
func decodeVarint(buf []byte) (value, bytesRead int) {
	shift := 0
	for i, b := range buf {
		value |= int(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			return value, i + 1
		}
		if i >= 9 { // varint too long
			return 0, 0
		}
	}
	return 0, 0 // unterminated varint
}

// DecodePolyline decodes a standard Google Encoded Polyline string (1e5
// precision) into [lat, lng] pairs. Kept for backwards compatibility
// with tests and the simulator.
func DecodePolyline(encoded string) ([][]float64, error) {
	return decodePolylineWithPrecision(encoded, 1e5)
}

// decodePolylineWithPrecision decodes a Google Encoded Polyline with the
// given coordinate precision divisor.
func decodePolylineWithPrecision(encoded string, precision float64) ([][]float64, error) {
	var coords [][]float64
	lat, lng := 0, 0
	i := 0

	for i < len(encoded) {
		dlat, n, err := decodeNextValue(encoded, i)
		if err != nil {
			break // truncated mid-latitude
		}
		i += n
		lat += dlat

		dlng, n, err := decodeNextValue(encoded, i)
		if err != nil {
			break // truncated mid-longitude
		}
		i += n
		lng += dlng

		coords = append(coords, []float64{
			float64(lat) / precision,
			float64(lng) / precision,
		})
	}

	if len(coords) == 0 {
		return nil, fmt.Errorf("DecodePolyline: no valid coordinates (len=%d)", len(encoded))
	}
	return coords, nil
}

// decodeNextValue reads one encoded integer from the polyline string.
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

	if value&1 != 0 {
		value = ^(value >> 1)
	} else {
		value >>= 1
	}

	return value, consumed, nil
}
