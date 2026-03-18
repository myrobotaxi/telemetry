package simulator

import "math"

const (
	// earthRadiusMiles is the mean radius of the Earth in miles, used to
	// convert speed + heading into lat/lng deltas.
	earthRadiusMiles = 3958.8

	// degreesToRadians converts degrees to radians.
	degreesToRadians = math.Pi / 180.0

	// radiansToDegrees converts radians to degrees.
	radiansToDegrees = 180.0 / math.Pi
)

// Waypoint is a geographic coordinate on a simulated route.
type Waypoint struct {
	Latitude  float64
	Longitude float64
}

// advancePosition computes the new lat/lng after traveling at the given speed
// (mph) and heading (degrees, 0=north) for the given interval (seconds).
// Uses the spherical-Earth approximation, which is accurate enough for
// short simulated drives.
func advancePosition(lat, lng, heading, speedMPH, intervalSec float64) (newLat, newLng float64) {
	distanceMiles := speedMPH * (intervalSec / 3600.0)
	if distanceMiles == 0 {
		return lat, lng
	}

	headingRad := heading * degreesToRadians
	latRad := lat * degreesToRadians
	lngRad := lng * degreesToRadians
	angularDist := distanceMiles / earthRadiusMiles

	newLatRad := math.Asin(
		math.Sin(latRad)*math.Cos(angularDist) +
			math.Cos(latRad)*math.Sin(angularDist)*math.Cos(headingRad),
	)
	newLngRad := lngRad + math.Atan2(
		math.Sin(headingRad)*math.Sin(angularDist)*math.Cos(latRad),
		math.Cos(angularDist)-math.Sin(latRad)*math.Sin(newLatRad),
	)

	return newLatRad * radiansToDegrees, newLngRad * radiansToDegrees
}

// normalizeHeading wraps a heading value to [0, 360).
func normalizeHeading(h float64) float64 {
	h = math.Mod(h, 360.0)
	if h < 0 {
		h += 360.0
	}
	return h
}
