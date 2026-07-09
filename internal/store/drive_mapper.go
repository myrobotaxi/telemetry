package store

import (
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// mapDriveStarted converts a DriveStartedEvent into a DriveRecord suitable
// for insertion. End-time fields are set to placeholder values that will be
// overwritten when the drive completes.
//
// StartLocation/StartAddress default to "" — the same "not geocoded yet"
// sentinel documented for the four Drive location columns in
// rest-api.md §7.2 ("zero-GPS at drive start, or reverse-geocode lookup
// failed" => the wire handler omits the key rather than emitting a
// placeholder). writer_drives.go's handleDriveStarted overwrites
// StartLocation/StartAddress from the geocoder's Result.PlaceName /
// Result.Address only on a successful lookup; leaving both fields empty
// here (rather than a formatted "lat,lng" string) is required so a
// disabled geocoder or a lookup miss doesn't leak raw coordinates into
// what is documented as a human-readable place-name field.
func mapDriveStarted(evt events.DriveStartedEvent, vehicleID string) DriveRecord {
	return DriveRecord{
		ID:            evt.DriveID,
		VehicleID:     vehicleID,
		Date:          evt.StartedAt.Format(time.DateOnly),
		StartTime:     evt.StartedAt.Format(time.RFC3339),
		EndTime:       "",
		StartLocation: "",
		StartAddress:  "",
		EndLocation:   "",
		EndAddress:    "",
		CreatedAt:     time.Now(),
	}
}

// mapDriveCompletion converts a DriveEndedEvent into a DriveCompletion
// with the final stats from the drive detector.
//
// EndLocation/EndAddress default to "" for the same reason StartLocation
// does in mapDriveStarted — see that comment.
func mapDriveCompletion(evt events.DriveEndedEvent) DriveCompletion {
	return DriveCompletion{
		EndTime:         evt.EndedAt.Format(time.RFC3339),
		EndLocation:     "",
		EndAddress:      "",
		DistanceMiles:   evt.Stats.Distance,
		DurationMinutes: int(evt.Stats.Duration.Minutes()),
		AvgSpeedMph:     evt.Stats.AvgSpeed,
		MaxSpeedMph:     evt.Stats.MaxSpeed,
		EnergyUsedKwh:   evt.Stats.EnergyDelta,
		EndChargeLevel:  evt.Stats.EndChargeLevel,
		FsdMiles:        evt.Stats.FSDMiles,
		FsdPercentage:   evt.Stats.FSDPercentage,
		Interventions:   0,
	}
}

// mapSingleRoutePoint converts a single event-layer RoutePoint to the
// store's RoutePointRecord format.
func mapSingleRoutePoint(pt events.RoutePoint) RoutePointRecord {
	return RoutePointRecord{
		Latitude:  pt.Latitude,
		Longitude: pt.Longitude,
		Speed:     pt.Speed,
		Heading:   pt.Heading,
		Timestamp: pt.Timestamp.Format(time.RFC3339),
	}
}

// mapRoutePoints converts event-layer RoutePoints to the store's
// RoutePointRecord format for JSONB persistence.
func mapRoutePoints(pts []events.RoutePoint) []RoutePointRecord {
	if len(pts) == 0 {
		return nil
	}
	records := make([]RoutePointRecord, len(pts))
	for i, pt := range pts {
		records[i] = RoutePointRecord{
			Latitude:  pt.Latitude,
			Longitude: pt.Longitude,
			Speed:     pt.Speed,
			Heading:   pt.Heading,
			Timestamp: pt.Timestamp.Format(time.RFC3339),
		}
	}
	return records
}
