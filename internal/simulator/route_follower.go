package simulator

import "math"

// RoutePosition holds the computed position along a route after an advance.
type RoutePosition struct {
	Lat              float64
	Lng              float64
	Heading          float64 // degrees, 0=north
	DistanceRemain   float64 // miles remaining to destination
	Finished         bool    // true when route is complete
}

// RouteFollower tracks progress along a sequence of [lng, lat] waypoints.
// On each call to Advance it moves forward by the distance implied by
// the given speed and time interval, interpolating between waypoints.
type RouteFollower struct {
	coords         [][2]float64 // [lng, lat]
	segIdx         int          // index of current segment start
	segFraction    float64      // fraction [0,1) along current segment
	totalMiles     float64      // total route distance
	traveledMiles  float64      // distance traveled so far
	segLengths     []float64    // precomputed segment lengths in miles
	finished       bool
}

// NewRouteFollower creates a follower from a RouteFile. It precomputes
// segment lengths to avoid repeated haversine calculations during Advance.
func NewRouteFollower(rf *RouteFile) *RouteFollower {
	segLengths := computeSegmentLengths(rf.Coordinates)
	return &RouteFollower{
		coords:     rf.Coordinates,
		totalMiles: rf.TotalDistanceMiles,
		segLengths: segLengths,
	}
}

// Advance moves the follower forward by the distance traveled at speedMPH
// over intervalSec seconds. Returns the new position along the route.
func (f *RouteFollower) Advance(speedMPH, intervalSec float64) RoutePosition {
	if f.finished || len(f.coords) < 2 {
		last := f.coords[len(f.coords)-1]
		return RoutePosition{
			Lat:            last[1],
			Lng:            last[0],
			Heading:        f.lastHeading(),
			DistanceRemain: 0,
			Finished:       true,
		}
	}

	distMiles := speedMPH * (intervalSec / 3600.0)
	f.traveledMiles += distMiles

	f.consumeDistance(distMiles)

	return f.currentPosition()
}

// Position returns the current position without advancing.
func (f *RouteFollower) Position() RoutePosition {
	if len(f.coords) < 2 {
		return RoutePosition{}
	}
	return f.currentPosition()
}

// consumeDistance walks forward along segments, consuming the given distance
// in miles and updating segIdx and segFraction.
func (f *RouteFollower) consumeDistance(distMiles float64) {
	remaining := distMiles

	for remaining > 0 && f.segIdx < len(f.segLengths) {
		segLen := f.segLengths[f.segIdx]
		if segLen == 0 {
			// Zero-length segment, skip it.
			f.segIdx++
			f.segFraction = 0
			continue
		}

		segRemain := segLen * (1.0 - f.segFraction)
		if remaining < segRemain {
			f.segFraction += remaining / segLen
			remaining = 0
		} else {
			remaining -= segRemain
			f.segIdx++
			f.segFraction = 0
		}
	}

	if f.segIdx >= len(f.segLengths) {
		f.segIdx = len(f.segLengths) - 1
		f.segFraction = 1.0
		f.finished = true
	}
}

// currentPosition computes lat, lng, heading, and remaining distance from
// the current segment index and fraction.
func (f *RouteFollower) currentPosition() RoutePosition {
	if f.finished {
		last := f.coords[len(f.coords)-1]
		return RoutePosition{
			Lat:            last[1],
			Lng:            last[0],
			Heading:        f.lastHeading(),
			DistanceRemain: 0,
			Finished:       true,
		}
	}

	from := f.coords[f.segIdx]
	to := f.coords[f.segIdx+1]

	lat := from[1] + (to[1]-from[1])*f.segFraction
	lng := from[0] + (to[0]-from[0])*f.segFraction

	heading := bearing(from[1], from[0], to[1], to[0])

	remain := f.totalMiles - f.traveledMiles
	if remain < 0 {
		remain = 0
	}

	return RoutePosition{
		Lat:            lat,
		Lng:            lng,
		Heading:        heading,
		DistanceRemain: remain,
	}
}

// lastHeading computes the heading of the final segment.
func (f *RouteFollower) lastHeading() float64 {
	n := len(f.coords)
	if n < 2 {
		return 0
	}
	from := f.coords[n-2]
	to := f.coords[n-1]
	return bearing(from[1], from[0], to[1], to[0])
}

// bearing computes the initial bearing in degrees from (lat1,lng1) to
// (lat2,lng2) using the forward azimuth formula.
func bearing(lat1, lng1, lat2, lng2 float64) float64 {
	lat1R := lat1 * degreesToRadians
	lat2R := lat2 * degreesToRadians
	dLng := (lng2 - lng1) * degreesToRadians

	y := math.Sin(dLng) * math.Cos(lat2R)
	x := math.Cos(lat1R)*math.Sin(lat2R) - math.Sin(lat1R)*math.Cos(lat2R)*math.Cos(dLng)

	return normalizeHeading(math.Atan2(y, x) * radiansToDegrees)
}

// haversineDistance computes the great-circle distance in miles between
// two lat/lng points.
func haversineDistance(lat1, lng1, lat2, lng2 float64) float64 {
	lat1R := lat1 * degreesToRadians
	lat2R := lat2 * degreesToRadians
	dLat := (lat2 - lat1) * degreesToRadians
	dLng := (lng2 - lng1) * degreesToRadians

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1R)*math.Cos(lat2R)*math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return earthRadiusMiles * c
}

// computeSegmentLengths calculates the haversine distance for each
// consecutive pair of [lng, lat] coordinates.
func computeSegmentLengths(coords [][2]float64) []float64 {
	if len(coords) < 2 {
		return nil
	}
	lengths := make([]float64, len(coords)-1)
	for i := 0; i < len(coords)-1; i++ {
		lengths[i] = haversineDistance(
			coords[i][1], coords[i][0],
			coords[i+1][1], coords[i+1][0],
		)
	}
	return lengths
}
