package store

import (
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

func TestMapDriveStarted(t *testing.T) {
	startedAt := time.Date(2026, 3, 17, 14, 30, 0, 0, time.UTC)
	evt := events.DriveStartedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive_abc123",
		Location: events.Location{
			Latitude:  33.0975,
			Longitude: -96.8214,
		},
		StartedAt: startedAt,
	}

	record := mapDriveStarted(evt, "veh_001")

	if record.ID != "drive_abc123" {
		t.Errorf("ID = %q, want %q", record.ID, "drive_abc123")
	}
	if record.VehicleID != "veh_001" {
		t.Errorf("VehicleID = %q, want %q", record.VehicleID, "veh_001")
	}
	if record.Date != "2026-03-17" {
		t.Errorf("Date = %q, want %q", record.Date, "2026-03-17")
	}
	if record.StartTime != "2026-03-17T14:30:00Z" {
		t.Errorf("StartTime = %q, want %q", record.StartTime, "2026-03-17T14:30:00Z")
	}
	// StartLocation is intentionally left empty by mapDriveStarted even
	// though the event carries non-zero GPS: it is populated later by
	// writer_drives.go's handleDriveStarted from the geocoder's
	// Result.PlaceName, and must default to "" (not a raw "lat,lng"
	// string) so a disabled/failed geocode leaves the wire-contract
	// "not geocoded yet" sentinel intact. See TestWriter_DriveStarted_*
	// in writer_drives_test.go for the geocoder-wired behavior.
	if record.StartLocation != "" {
		t.Errorf("StartLocation = %q, want empty (geocoder not yet consulted in mapDriveStarted)", record.StartLocation)
	}
	if record.EndTime != "" {
		t.Errorf("EndTime = %q, want empty", record.EndTime)
	}
}

func TestMapDriveCompletion(t *testing.T) {
	endedAt := time.Date(2026, 3, 17, 15, 15, 0, 0, time.UTC)
	evt := events.DriveEndedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive_abc123",
		EndedAt: endedAt,
		Stats: events.DriveStats{
			Distance:    12.5,
			Duration:    45 * time.Minute,
			AvgSpeed:    16.7,
			MaxSpeed:    55.0,
			EnergyDelta: 4.2,
			EndLocation: events.Location{
				Latitude:  33.1100,
				Longitude: -96.8300,
			},
			EndChargeLevel: 82,
			FSDMiles:       10.0,
			FSDPercentage:  80.0,
		},
	}

	completion := mapDriveCompletion(evt)

	if completion.EndTime != "2026-03-17T15:15:00Z" {
		t.Errorf("EndTime = %q, want %q", completion.EndTime, "2026-03-17T15:15:00Z")
	}
	// EndLocation is intentionally left empty by mapDriveCompletion for
	// the same reason StartLocation is in TestMapDriveStarted above.
	if completion.EndLocation != "" {
		t.Errorf("EndLocation = %q, want empty (geocoder not yet consulted in mapDriveCompletion)", completion.EndLocation)
	}
	if completion.DistanceMiles != 12.5 {
		t.Errorf("DistanceMiles = %f, want 12.5", completion.DistanceMiles)
	}
	if completion.DurationMinutes != 45 {
		t.Errorf("DurationMinutes = %d, want 45", completion.DurationMinutes)
	}
	if completion.AvgSpeedMph != 16.7 {
		t.Errorf("AvgSpeedMph = %f, want 16.7", completion.AvgSpeedMph)
	}
	if completion.MaxSpeedMph != 55.0 {
		t.Errorf("MaxSpeedMph = %f, want 55.0", completion.MaxSpeedMph)
	}
	if completion.EnergyUsedKwh != 4.2 {
		t.Errorf("EnergyUsedKwh = %f, want 4.2", completion.EnergyUsedKwh)
	}
	if completion.EndChargeLevel != 82 {
		t.Errorf("EndChargeLevel = %d, want 82", completion.EndChargeLevel)
	}
	if completion.FsdMiles != 10.0 {
		t.Errorf("FsdMiles = %f, want 10.0", completion.FsdMiles)
	}
	if completion.FsdPercentage != 80.0 {
		t.Errorf("FsdPercentage = %f, want 80.0", completion.FsdPercentage)
	}
}

// TestMapDriveCompletion_StartChargeLevel is the MYR-241 regression: the
// completion mapper must forward evt.Stats.StartChargeLevel so the drive
// detector's computed drive-start SOC is persisted on the completion UPDATE.
// Before the fix the field did not exist on DriveCompletion, so the value the
// detector already produced (MYR-207) was dropped and every Drive row kept the
// insert-time default of 0 while endChargeLevel captured correctly.
func TestMapDriveCompletion_StartChargeLevel(t *testing.T) {
	endedAt := time.Date(2026, 3, 17, 15, 15, 0, 0, time.UTC)

	tests := []struct {
		name           string
		startCharge    int // Stats.StartChargeLevel from the detector
		endCharge      int // Stats.EndChargeLevel from the detector
		wantStartLevel int
		wantEndLevel   int
	}{
		{
			// (a) SOC known before drive start → detector seeds it → persisted.
			name:           "soc known before start",
			startCharge:    80,
			endCharge:      74,
			wantStartLevel: 80,
			wantEndLevel:   74,
		},
		{
			// (b) SOC first arrived mid-drive → detector patched start once.
			name:           "soc patched mid-drive",
			startCharge:    70,
			endCharge:      65,
			wantStartLevel: 70,
			wantEndLevel:   65,
		},
		{
			// (c) SOC never observed → detector leaves both 0; mapper must not
			// invent a value. End behaviour unchanged (also 0).
			name:           "soc never observed",
			startCharge:    0,
			endCharge:      0,
			wantStartLevel: 0,
			wantEndLevel:   0,
		},
		{
			// (d) no regression: a distinct start/end pair round-trips intact.
			name:           "distinct start and end",
			startCharge:    100,
			endCharge:      42,
			wantStartLevel: 100,
			wantEndLevel:   42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := events.DriveEndedEvent{
				VIN:     "5YJ3E1EA1NF000001",
				DriveID: "drive_charge",
				EndedAt: endedAt,
				Stats: events.DriveStats{
					StartChargeLevel: tt.startCharge,
					EndChargeLevel:   tt.endCharge,
				},
			}

			completion := mapDriveCompletion(evt)

			if completion.StartChargeLevel != tt.wantStartLevel {
				t.Errorf("StartChargeLevel = %d, want %d", completion.StartChargeLevel, tt.wantStartLevel)
			}
			if completion.EndChargeLevel != tt.wantEndLevel {
				t.Errorf("EndChargeLevel = %d, want %d", completion.EndChargeLevel, tt.wantEndLevel)
			}
		})
	}
}

func TestMapSingleRoutePoint(t *testing.T) {
	ts := time.Date(2026, 3, 17, 14, 31, 0, 0, time.UTC)
	pt := events.RoutePoint{
		Latitude: 33.0975, Longitude: -96.8214,
		Speed: 45.0, Heading: 245.0, Timestamp: ts,
	}

	r := mapSingleRoutePoint(pt)

	if r.Latitude != 33.0975 {
		t.Errorf("Latitude = %f, want 33.0975", r.Latitude)
	}
	if r.Longitude != -96.8214 {
		t.Errorf("Longitude = %f, want -96.8214", r.Longitude)
	}
	if r.Speed != 45.0 {
		t.Errorf("Speed = %f, want 45.0", r.Speed)
	}
	if r.Heading != 245.0 {
		t.Errorf("Heading = %f, want 245.0", r.Heading)
	}
	if r.Timestamp != "2026-03-17T14:31:00Z" {
		t.Errorf("Timestamp = %q, want %q", r.Timestamp, "2026-03-17T14:31:00Z")
	}
}

func TestMapRoutePoints(t *testing.T) {
	ts := time.Date(2026, 3, 17, 14, 31, 0, 0, time.UTC)

	tests := []struct {
		name string
		pts  []events.RoutePoint
		want int // expected length
	}{
		{
			name: "nil input",
			pts:  nil,
			want: 0,
		},
		{
			name: "empty input",
			pts:  []events.RoutePoint{},
			want: 0,
		},
		{
			name: "single point",
			pts: []events.RoutePoint{
				{Latitude: 33.0975, Longitude: -96.8214, Speed: 45.0, Heading: 245.0, Timestamp: ts},
			},
			want: 1,
		},
		{
			name: "multiple points",
			pts: []events.RoutePoint{
				{Latitude: 33.0975, Longitude: -96.8214, Speed: 45.0, Heading: 245.0, Timestamp: ts},
				{Latitude: 33.0980, Longitude: -96.8220, Speed: 55.0, Heading: 250.0, Timestamp: ts.Add(10 * time.Second)},
			},
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records := mapRoutePoints(tt.pts)
			if len(records) != tt.want {
				t.Fatalf("len = %d, want %d", len(records), tt.want)
			}
			if tt.want == 0 {
				return
			}
			// Check first record matches input.
			r := records[0]
			if r.Latitude != 33.0975 {
				t.Errorf("Latitude = %f, want 33.0975", r.Latitude)
			}
			if r.Longitude != -96.8214 {
				t.Errorf("Longitude = %f, want -96.8214", r.Longitude)
			}
			if r.Speed != 45.0 {
				t.Errorf("Speed = %f, want 45.0", r.Speed)
			}
			if r.Heading != 245.0 {
				t.Errorf("Heading = %f, want 245.0", r.Heading)
			}
			if r.Timestamp != "2026-03-17T14:31:00Z" {
				t.Errorf("Timestamp = %q, want %q", r.Timestamp, "2026-03-17T14:31:00Z")
			}
		})
	}
}

func TestMapDriveStarted_ZeroLocation(t *testing.T) {
	startedAt := time.Date(2026, 3, 17, 14, 30, 0, 0, time.UTC)
	evt := events.DriveStartedEvent{
		VIN:       "5YJ3E1EA1NF000001",
		DriveID:   "drive_zero_loc",
		Location:  events.Location{Latitude: 0, Longitude: 0},
		StartedAt: startedAt,
	}

	record := mapDriveStarted(evt, "veh_001")

	if record.StartLocation != "" {
		t.Errorf("StartLocation = %q, want empty for (0,0)", record.StartLocation)
	}
}
