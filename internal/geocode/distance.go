package geocode

import "math"

// haversineMeters returns the great-circle distance in meters between
// two (lat, lng) coordinates using the haversine formula. The 6_371_000
// constant is the WGS-84 mean Earth radius in meters.
//
// This is package-private and intentionally duplicated from
// internal/store/writer_location_address.go to avoid a cross-package
// dependency from store → geocode (which would invert the established
// dependency direction). The store package already owns its own copy.
func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusMeters = 6_371_000.0
	const deg2rad = math.Pi / 180.0

	dLat := (lat2 - lat1) * deg2rad
	dLng := (lng2 - lng1) * deg2rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*deg2rad)*math.Cos(lat2*deg2rad)*
			math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMeters * c
}
