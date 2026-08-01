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

// restDisplacementMiles is how far a NON-STREAMED position fix must sit from
// the previous one before it counts as movement. It is the STREAM's own
// Location filter — `MinimumDelta: 10` metres in DefaultFieldConfig
// (internal/telemetry/fleet_api_fields.go) — restated in the miles that
// haversine returns.
//
// WHY THIS THRESHOLD EXISTS AT ALL (MYR-394). Before REST position polling, the
// only source of `location` was the stream, and Tesla applied that 10m delta
// filter on the CAR: a parked, streaming vehicle sent gear and speed but not a
// repeated Location, so handleDriving's location branch never ran and the
// MYR-160 stall watchdog could see a motionless drive and close it. A REST
// vehicle_data read has no such filter — it returns the car's cached fix on
// every poll, byte-identical for a car that has not moved. Taking each of those
// as "movement" would refresh lastMovementAt every ~25s forever, so StallTimeout
// could never fire, and an open drive on a parked car would accumulate an
// identical route point (and broadcast a DriveUpdatedEvent) every cycle until
// MaxDriveDuration's 12h backstop. Matching the stream's own delta means the
// two sources agree on what "moved" means.
const restDisplacementMiles = 0.0062137 // 10 metres

// displacedEnough reports whether te's location is far enough from the drive's
// previous fix to count as real motion.
//
// A STREAMED frame is taken at face value: Tesla already applied the 10m
// MinimumDelta on the car, so its arrival IS the movement signal, and
// re-deriving the same judgement from sparse GPS here would risk discarding
// genuine points at the boundary. Only the REST-backfilled frames — which carry
// no delta filter of their own — are measured.
func displacedEnough(te events.VehicleTelemetryEvent, prev, next events.Location) bool {
	if te.Streamed() {
		return true
	}
	return haversine(prev.Latitude, prev.Longitude, next.Latitude, next.Longitude) >= restDisplacementMiles
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

	// Distance prefers the odometer delta (MYR-157): odometer is an
	// accurate cumulative counter, whereas totalDistance() sums GPS
	// haversine chords between sparse route points and undercounts real
	// road distance. Fall back to GPS when no odometer baseline was
	// observed, or when the delta is non-positive (e.g. an odometer reset,
	// or a drive with no in-motion odometer sample yet).
	distance := totalDistance(drive.routePoints)
	if drive.odometerBaselineSet {
		if odoDist := drive.lastOdometer - drive.startOdometer; odoDist > 0 {
			distance = odoDist
		}
	}

	var avgSpeed float64
	if drive.speedCount > 0 {
		avgSpeed = drive.speedSum / float64(drive.speedCount)
	}

	energyDelta := drive.startEnergy - drive.lastEnergy
	if drive.startEnergy == 0 {
		energyDelta = 0
	}

	// FSD miles is the delta of the cumulative "miles since reset" counter
	// over the drive. Only meaningful if a baseline was actually observed;
	// otherwise report zero rather than the full cumulative counter.
	var fsdMiles float64
	if drive.fsdBaselineSet {
		fsdMiles = drive.lastFSDMiles - drive.startFSDMiles
		if fsdMiles < 0 {
			fsdMiles = 0
		}
	}

	// FSD share, clamped to [0,100]. FSD miles ⊆ total odometer miles, so a
	// correct measurement is always ≤100%; the clamp guards against residual
	// counter/sampling skew that previously produced >100% (e.g. 176%) when
	// FSD (odometer-accurate) was divided by the undercounting GPS distance.
	var fsdPercentage float64
	if distance > 0 && fsdMiles > 0 {
		fsdPercentage = (fsdMiles / distance) * 100.0
		if fsdPercentage > 100.0 {
			fsdPercentage = 100.0
		}
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
