package store

import (
	"fmt"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// mapDriveStarted converts a DriveStartedEvent into a DriveRecord suitable
// for insertion. End-time fields are set to placeholder values that will be
// overwritten when the drive completes.
func mapDriveStarted(evt events.DriveStartedEvent, vehicleID string) DriveRecord {
	return DriveRecord{
		ID:            evt.DriveID,
		VehicleID:     vehicleID,
		Date:          evt.StartedAt.Format(time.DateOnly),
		StartTime:     evt.StartedAt.Format(time.RFC3339),
		EndTime:       "",
		StartLocation: formatLocation(evt.Location),
		StartAddress:  "",
		EndLocation:   "",
		EndAddress:    "",
		CreatedAt:     time.Now(),
	}
}

// mapDriveCompletion converts a DriveEndedEvent into a DriveCompletion
// with the final stats from the drive detector.
func mapDriveCompletion(evt events.DriveEndedEvent) DriveCompletion {
	return DriveCompletion{
		EndTime:         evt.EndedAt.Format(time.RFC3339),
		EndLocation:     formatLocation(evt.Stats.EndLocation),
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

// formatLocation formats a Location as a "lat,lng" string for the Prisma
// schema's string-typed location columns. Returns empty string if both
// coordinates are zero (protobuf default for "not set").
func formatLocation(loc events.Location) string {
	if loc.Latitude == 0 && loc.Longitude == 0 {
		return ""
	}
	return fmt.Sprintf("%.6f,%.6f", loc.Latitude, loc.Longitude)
}
