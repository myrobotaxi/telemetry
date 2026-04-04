// Package ws re-exports polyline functions from the shared internal/polyline
// package for backward compatibility within the ws package.
package ws

import (
	"fmt"

	"github.com/tnando/my-robo-taxi-telemetry/internal/polyline"
)

// DecodeRouteLine delegates to polyline.DecodeRouteLine.
func DecodeRouteLine(encoded string) ([][]float64, error) {
	coords, err := polyline.DecodeRouteLine(encoded)
	if err != nil {
		return nil, fmt.Errorf("DecodeRouteLine: %w", err)
	}
	return coords, nil
}

// DecodePolyline delegates to polyline.DecodePolyline.
func DecodePolyline(encoded string) ([][]float64, error) {
	coords, err := polyline.DecodePolyline(encoded)
	if err != nil {
		return nil, fmt.Errorf("DecodePolyline: %w", err)
	}
	return coords, nil
}
