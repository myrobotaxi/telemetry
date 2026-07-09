package store

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/geocode"
)

// MYR-206 — unit tests for reverse-geocode Result.PlaceName wiring into
// the Drive record's StartLocation/EndLocation columns. These
// complement TestWriter_DriveStarted / TestWriter_DriveEnded in
// writer_test.go (which both use geocode.NoopGeocoder{} and never
// assert on the location/address fields) by exercising
// handleDriveStarted/handleDriveEnded in writer_drives.go with a
// geocoder that actually returns results.

// newDriveGeocodeTestWriter wires a Writer with the supplied geocoder
// so handleDriveStarted/handleDriveEnded's reverse-geocode calls can be
// asserted on.
func newDriveGeocodeTestWriter(t *testing.T, bus events.Bus, drives drivePersister, lookup vinIDLookup, geo geocode.Geocoder) *Writer {
	t.Helper()
	vehicles := &mockVehicleUpdater{}
	return NewWriter(vehicles, drives, lookup, bus, geo, slog.Default(), WriterConfig{
		FlushInterval: 50 * time.Millisecond,
		BatchSize:     1000,
	})
}

// TestWriter_DriveStarted_GeocoderPlaceName verifies that a successful
// reverse-geocode with a POI match (Result.PlaceName non-empty) is
// wired onto DriveRecord.StartLocation, and the accompanying street
// address onto StartAddress.
func TestWriter_DriveStarted_GeocoderPlaceName(t *testing.T) {
	bus := newTestBus(t)
	drives := &mockDrivePersister{}
	lookup := &stubIDLookup{
		pairs: map[string]struct{ id, userID string }{
			"5YJ3E1EA1NF000001": {id: "veh_001", userID: "user_001"},
		},
	}
	geo := &stubGeocoder{
		results: []geocode.Result{{PlaceName: "Whole Foods Market", Address: "399 4th Street, San Francisco, CA"}},
	}

	w := newDriveGeocodeTestWriter(t, bus, drives, lookup, geo)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	evt := events.NewEvent(events.DriveStartedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive_001",
		Location: events.Location{
			Latitude:  33.0975,
			Longitude: -96.8214,
		},
		StartedAt: time.Now(),
	})
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return len(drives.getCreates()) > 0
	})

	record := drives.getCreates()[0].Record
	if record.StartLocation != "Whole Foods Market" {
		t.Errorf("StartLocation = %q, want %q", record.StartLocation, "Whole Foods Market")
	}
	if record.StartAddress != "399 4th Street, San Francisco, CA" {
		t.Errorf("StartAddress = %q, want %q", record.StartAddress, "399 4th Street, San Francisco, CA")
	}
	if got := geo.callCount(); got != 1 {
		t.Errorf("geocoder calls = %d, want 1", got)
	}
}

// TestWriter_DriveStarted_GeocoderAddressOnly verifies the residential
// case: Mapbox returns a match but no POI within the geocoder's
// distance threshold, so Result.PlaceName is empty while Result.Address
// is populated. StartLocation must stay empty (omitted on the wire per
// rest-api.md §7.2) while StartAddress is still set from the address.
func TestWriter_DriveStarted_GeocoderAddressOnly(t *testing.T) {
	bus := newTestBus(t)
	drives := &mockDrivePersister{}
	lookup := &stubIDLookup{
		pairs: map[string]struct{ id, userID string }{
			"5YJ3E1EA1NF000001": {id: "veh_001", userID: "user_001"},
		},
	}
	geo := &stubGeocoder{
		results: []geocode.Result{{PlaceName: "", Address: "4220 Tributary Wy, Frisco, TX"}},
	}

	w := newDriveGeocodeTestWriter(t, bus, drives, lookup, geo)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	evt := events.NewEvent(events.DriveStartedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive_002",
		Location: events.Location{
			Latitude:  33.0975,
			Longitude: -96.8214,
		},
		StartedAt: time.Now(),
	})
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return len(drives.getCreates()) > 0
	})

	record := drives.getCreates()[0].Record
	if record.StartLocation != "" {
		t.Errorf("StartLocation = %q, want empty (no POI within threshold)", record.StartLocation)
	}
	if record.StartAddress != "4220 Tributary Wy, Frisco, TX" {
		t.Errorf("StartAddress = %q, want %q", record.StartAddress, "4220 Tributary Wy, Frisco, TX")
	}
}

// TestWriter_DriveStarted_GeocoderNoResult is the regression guard for
// the raw-coordinate leak this ticket fixes: when the geocoder returns
// ErrNoResult (disabled geocoder, or Mapbox genuinely had no match),
// both StartLocation and StartAddress must stay empty — never a
// formatted "lat,lng" string. Before the MYR-206 fix, mapDriveStarted
// seeded StartLocation with formatLocation(evt.Location), which only
// got overwritten on a *successful* geocode, so a failed/disabled
// lookup left the raw coordinate string in place.
func TestWriter_DriveStarted_GeocoderNoResult(t *testing.T) {
	bus := newTestBus(t)
	drives := &mockDrivePersister{}
	lookup := &stubIDLookup{
		pairs: map[string]struct{ id, userID string }{
			"5YJ3E1EA1NF000001": {id: "veh_001", userID: "user_001"},
		},
	}
	geo := &stubGeocoder{} // empty results → ErrNoResult on every call

	w := newDriveGeocodeTestWriter(t, bus, drives, lookup, geo)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	evt := events.NewEvent(events.DriveStartedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive_003",
		Location: events.Location{
			Latitude:  33.0975,
			Longitude: -96.8214,
		},
		StartedAt: time.Now(),
	})
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return len(drives.getCreates()) > 0
	})

	record := drives.getCreates()[0].Record
	if record.StartLocation != "" {
		t.Errorf("StartLocation = %q, want empty (geocoder returned no result)", record.StartLocation)
	}
	if record.StartAddress != "" {
		t.Errorf("StartAddress = %q, want empty (geocoder returned no result)", record.StartAddress)
	}
}

// TestWriter_DriveEnded_GeocoderPlaceName mirrors
// TestWriter_DriveStarted_GeocoderPlaceName for the drive-completion
// path: a successful reverse-geocode with a POI match wires
// Result.PlaceName onto DriveCompletion.EndLocation.
func TestWriter_DriveEnded_GeocoderPlaceName(t *testing.T) {
	bus := newTestBus(t)
	drives := &mockDrivePersister{}
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}
	geo := &stubGeocoder{
		results: []geocode.Result{{PlaceName: "Home", Address: "742 Evergreen Terrace, San Francisco, CA"}},
	}

	w := newDriveGeocodeTestWriter(t, bus, drives, lookup, geo)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	evt := events.NewEvent(events.DriveEndedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive_004",
		EndedAt: time.Now(),
		Stats: events.DriveStats{
			Distance: 12.5,
			Duration: 45 * time.Minute,
			EndLocation: events.Location{
				Latitude:  33.1100,
				Longitude: -96.8300,
			},
			EndChargeLevel: 82,
		},
	})
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return len(drives.getCompletes()) > 0
	})

	stats := drives.getCompletes()[0].Stats
	if stats.EndLocation != "Home" {
		t.Errorf("EndLocation = %q, want %q", stats.EndLocation, "Home")
	}
	if stats.EndAddress != "742 Evergreen Terrace, San Francisco, CA" {
		t.Errorf("EndAddress = %q, want %q", stats.EndAddress, "742 Evergreen Terrace, San Francisco, CA")
	}
}

// TestWriter_DriveEnded_GeocoderNoResult is the drive-completion
// counterpart to TestWriter_DriveStarted_GeocoderNoResult: a failed /
// absent geocode must leave EndLocation and EndAddress both empty,
// never a raw "lat,lng" string.
func TestWriter_DriveEnded_GeocoderNoResult(t *testing.T) {
	bus := newTestBus(t)
	drives := &mockDrivePersister{}
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}
	geo := &stubGeocoder{} // empty results → ErrNoResult on every call

	w := newDriveGeocodeTestWriter(t, bus, drives, lookup, geo)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	evt := events.NewEvent(events.DriveEndedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive_005",
		EndedAt: time.Now(),
		Stats: events.DriveStats{
			Distance: 12.5,
			Duration: 45 * time.Minute,
			EndLocation: events.Location{
				Latitude:  33.1100,
				Longitude: -96.8300,
			},
			EndChargeLevel: 82,
		},
	})
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return len(drives.getCompletes()) > 0
	})

	stats := drives.getCompletes()[0].Stats
	if stats.EndLocation != "" {
		t.Errorf("EndLocation = %q, want empty (geocoder returned no result)", stats.EndLocation)
	}
	if stats.EndAddress != "" {
		t.Errorf("EndAddress = %q, want empty (geocoder returned no result)", stats.EndAddress)
	}
}

// TestWriter_DriveEnded_GeocoderTransportError verifies that a
// non-ErrNoResult geocoder failure (e.g. Mapbox transport/HTTP error)
// is logged but does not crash the write path, and — like the
// ErrNoResult case — leaves EndLocation/EndAddress empty rather than
// falling back to raw coordinates.
func TestWriter_DriveEnded_GeocoderTransportError(t *testing.T) {
	bus := newTestBus(t)
	drives := &mockDrivePersister{}
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}
	geo := &stubGeocoder{err: errors.New("mapbox unreachable")}

	w := newDriveGeocodeTestWriter(t, bus, drives, lookup, geo)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	evt := events.NewEvent(events.DriveEndedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive_006",
		EndedAt: time.Now(),
		Stats: events.DriveStats{
			Distance: 12.5,
			Duration: 45 * time.Minute,
			EndLocation: events.Location{
				Latitude:  33.1100,
				Longitude: -96.8300,
			},
			EndChargeLevel: 82,
		},
	})
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return len(drives.getCompletes()) > 0
	})

	stats := drives.getCompletes()[0].Stats
	if stats.EndLocation != "" {
		t.Errorf("EndLocation = %q, want empty (geocoder transport error)", stats.EndLocation)
	}
	if stats.EndAddress != "" {
		t.Errorf("EndAddress = %q, want empty (geocoder transport error)", stats.EndAddress)
	}
}
