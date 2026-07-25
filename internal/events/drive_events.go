package events

import "time"

// DriveStartedEvent is published when the drive detector identifies that a
// vehicle has begun a new drive (shift state transitions to D or R).
type DriveStartedEvent struct {
	BasePayload
	VIN       string
	DriveID   string
	Location  Location
	StartedAt time.Time
}

// EventTopic returns TopicDriveStarted.
func (DriveStartedEvent) EventTopic() Topic { return TopicDriveStarted }

// DriveUpdatedEvent is published for each route point accumulated during
// an active drive.
type DriveUpdatedEvent struct {
	BasePayload
	VIN        string
	DriveID    string
	RoutePoint RoutePoint
}

// EventTopic returns TopicDriveUpdated.
func (DriveUpdatedEvent) EventTopic() Topic { return TopicDriveUpdated }

// DriveEndedEvent is published when the drive detector identifies that a
// vehicle has completed a drive (shift state transitions to P and the drive
// meets minimum duration/distance thresholds).
type DriveEndedEvent struct {
	BasePayload
	VIN     string
	DriveID string
	Stats   DriveStats
	// StartedAt is the drive's start instant (vehicle event time), carried so
	// consumers can correlate the ended drive to a specific leg. The ride
	// completer (internal/ridecomplete, MYR-265) uses it to distinguish the
	// leg-2 dropoff drive (started after board) from a delayed leg-1 pickup
	// drive-end (started before board) — only the former completes the ride.
	StartedAt time.Time
	EndedAt   time.Time
}

// EventTopic returns TopicDriveEnded.
func (DriveEndedEvent) EventTopic() Topic { return TopicDriveEnded }

// DriveDiscardedEvent is published when the drive detector drops an
// active drive via the micro-drive filter instead of completing it.
// The store subscriber deletes the Drive row created on drive.started
// so discarded blips don't accumulate as stuck-open rows (MYR-160).
type DriveDiscardedEvent struct {
	BasePayload
	VIN         string
	DriveID     string
	DiscardedAt time.Time
}

// EventTopic returns TopicDriveDiscarded.
func (DriveDiscardedEvent) EventTopic() Topic { return TopicDriveDiscarded }

// RoutePoint is a single GPS sample captured during an active drive.
type RoutePoint struct {
	Latitude  float64
	Longitude float64
	Speed     float64 // mph
	Heading   float64 // degrees 0-360
	Timestamp time.Time
}

// DriveStats are the summary statistics calculated when a drive ends.
type DriveStats struct {
	Distance         float64       // miles (haversine sum of route points)
	Duration         time.Duration // wall-clock drive time
	AvgSpeed         float64       // mph
	MaxSpeed         float64       // mph
	EnergyDelta      float64       // kWh consumed (positive = used)
	StartLocation    Location
	EndLocation      Location
	StartChargeLevel int          // SOC percent at drive start
	EndChargeLevel   int          // SOC percent at drive end
	FSDMiles         float64      // FSD miles this trip
	FSDPercentage    float64      // (FSDMiles / Distance) * 100
	RoutePoints      []RoutePoint // full route for persistence
}
