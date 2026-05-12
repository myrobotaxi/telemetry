package drives

import (
	"math"

	"github.com/myrobotaxi/telemetry/internal/events"
)

const (
	// earthRadiusMiles is the mean radius of the Earth in miles.
	earthRadiusMiles = 3958.8
)

// haversine returns the great-circle distance in miles between two
// geographic coordinates.
func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	dLat := degreesToRadians(lat2 - lat1)
	dLon := degreesToRadians(lon2 - lon1)

	lat1Rad := degreesToRadians(lat1)
	lat2Rad := degreesToRadians(lat2)

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1Rad)*math.Cos(lat2Rad)*
			math.Sin(dLon/2)*math.Sin(dLon/2)

	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMiles * c
}

// degreesToRadians converts degrees to radians.
func degreesToRadians(deg float64) float64 {
	return deg * math.Pi / 180.0
}

// totalDistance calculates the total distance in miles along a route by
// summing haversine distances between consecutive points.
func totalDistance(points []events.RoutePoint) float64 {
	if len(points) < 2 {
		return 0
	}
	var dist float64
	for i := 1; i < len(points); i++ {
		dist += haversine(
			points[i-1].Latitude, points[i-1].Longitude,
			points[i].Latitude, points[i].Longitude,
		)
	}
	return dist
}

// calculateStats computes the final drive statistics from an activeDrive.
// The endSOC and endEnergy values come from the most recent telemetry event.
func calculateStats(drive *activeDrive) events.DriveStats {
	duration := drive.lastTimestamp.Sub(drive.startedAt)
	distance := totalDistance(drive.routePoints)

	var avgSpeed float64
	if drive.speedCount > 0 {
		avgSpeed = drive.speedSum / float64(drive.speedCount)
	}

	energyDelta := drive.startEnergy - drive.lastEnergy
	if drive.startEnergy == 0 {
		energyDelta = 0
	}

	fsdMiles := drive.lastFSDMiles - drive.startFSDMiles
	if fsdMiles < 0 {
		fsdMiles = 0
	}
	// If start FSD miles was not captured, report zero.
	if drive.startFSDMiles == 0 {
		fsdMiles = 0
	}

	var fsdPercentage float64
	if distance > 0 && fsdMiles > 0 {
		fsdPercentage = (fsdMiles / distance) * 100.0
	}

	endLocation := drive.lastLocation
	if endLocation == (events.Location{}) && len(drive.routePoints) > 0 {
		last := drive.routePoints[len(drive.routePoints)-1]
		endLocation = events.Location{
			Latitude:  last.Latitude,
			Longitude: last.Longitude,
		}
	}

	return events.DriveStats{
		Distance:         distance,
		Duration:         duration,
		AvgSpeed:         avgSpeed,
		MaxSpeed:         drive.maxSpeed,
		EnergyDelta:      energyDelta,
		StartLocation:    drive.startLocation,
		EndLocation:      endLocation,
		StartChargeLevel: int(drive.startCharge),
		EndChargeLevel:   int(drive.lastSOC),
		FSDMiles:         fsdMiles,
		FSDPercentage:    fsdPercentage,
		RoutePoints:      drive.routePoints,
	}
}

// redactVIN returns the last 4 characters of a VIN, prefixed with "***".
// Used in log messages to comply with the security policy that VINs must
// not appear in full in production logs.
func redactVIN(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return "***" + vin[len(vin)-4:]
}
