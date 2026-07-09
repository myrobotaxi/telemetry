package store

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/geocode"
)

// MYR-144 — unit tests for the current-location reverse-geocode
// scheduler. The tests drive scheduleLocationGeocode and
// geocodeParkedLocation directly rather than through the full event-bus +
// flush plumbing so debounce timing can be simulated by mutating the
// cache entry (lastFiredAt) instead of sleeping. The async goroutine
// path is awaited via a tight waitForCondition poll capped at 200ms; it
// must finish well before that under load.

const testVIN = "5YJ3E1EA1NF000099"

// newLocAddrTestWriter wires up a Writer with the supplied geocoder and
// vehicleUpdater for direct invocation of scheduleLocationGeocode /
// geocodeParkedLocation. The bus, drive persister, and VIN lookup are
// no-op stubs because these tests never publish events or persist drives
// — they exercise the per-VIN debounce + async write path in isolation.
func newLocAddrTestWriter(t *testing.T, vehicles vehicleUpdater, geo geocode.Geocoder) *Writer {
	t.Helper()
	bus := newTestBus(t)
	drives := &mockDrivePersister{}
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}
	return NewWriter(vehicles, drives, lookup, bus, geo, slog.Default(), WriterConfig{
		FlushInterval:            50 * time.Millisecond,
		BatchSize:                1000,
		LocationGeocodeDebounce:  60 * time.Second,
		LocationGeocodeMinMeters: 50.0,
	})
}

// awaitGeocodeCalls polls until the geocoder records the expected number
// of calls or 200ms passes. Async-goroutine completion in
// scheduleLocationGeocode should land in single-digit milliseconds; the
// cap prevents a flaky test from hanging CI.
func awaitGeocodeCalls(t *testing.T, geo *stubGeocoder, want int) {
	t.Helper()
	waitForCondition(t, 200*time.Millisecond, func() bool {
		return geo.callCount() >= want
	})
}

// awaitVehicleWrites polls until the recorded vehicle writes reach the
// expected count or 200ms passes. Used to confirm the async path stamped
// LocationName/LocationAddr onto the row.
func awaitVehicleWrites(t *testing.T, vehicles *mockVehicleUpdater, want int) {
	t.Helper()
	waitForCondition(t, 200*time.Millisecond, func() bool {
		return len(vehicles.getTelemetryWrites()) >= want
	})
}

// TestScheduleLocationGeocode_FirstFlushFires verifies the cache-miss
// path: when no entry exists for a VIN, the scheduler fires immediately,
// the geocoder is called once, the cache is populated, and the Vehicle
// row is written with LocationName + LocationAddr.
func TestScheduleLocationGeocode_FirstFlushFires(t *testing.T) {
	vehicles := &mockVehicleUpdater{}
	geo := &stubGeocoder{
		results: []geocode.Result{{PlaceName: "Home", Address: "4220 Tributary Wy"}},
	}
	w := newLocAddrTestWriter(t, vehicles, geo)

	update := &VehicleUpdate{
		Latitude:    floatPtr(33.086131),
		Longitude:   floatPtr(-96.852202),
		LastUpdated: time.Now(),
	}
	w.scheduleLocationGeocode(testVIN, update)

	awaitGeocodeCalls(t, geo, 1)
	awaitVehicleWrites(t, vehicles, 1)
	w.geocodeWG.Wait()

	if got := geo.callCount(); got != 1 {
		t.Fatalf("geocoder calls = %d, want 1", got)
	}
	writes := vehicles.getTelemetryWrites()
	if len(writes) != 1 {
		t.Fatalf("vehicle writes = %d, want 1", len(writes))
	}
	if writes[0].VIN != testVIN {
		t.Errorf("VIN = %q, want %q", writes[0].VIN, testVIN)
	}
	if writes[0].Update.LocationName == nil || *writes[0].Update.LocationName != "Home" {
		t.Errorf("LocationName = %v, want %q", ptrVal(writes[0].Update.LocationName), "Home")
	}
	if writes[0].Update.LocationAddr == nil || *writes[0].Update.LocationAddr != "4220 Tributary Wy" {
		t.Errorf("LocationAddr = %v, want %q", ptrVal(writes[0].Update.LocationAddr), "4220 Tributary Wy")
	}

	// Cache must hold the anchor so the next sub-debounce flush short-circuits.
	w.locAddrMu.Lock()
	entry := w.locAddrCache[testVIN]
	w.locAddrMu.Unlock()
	if entry.address != "4220 Tributary Wy" || entry.name != "Home" {
		t.Errorf("cache entry = {name=%q, address=%q}, want {Home, 4220 Tributary Wy}", entry.name, entry.address)
	}
	if entry.inFlight {
		t.Errorf("cache entry.inFlight = true, want false (goroutine finished)")
	}
}

// TestScheduleLocationGeocode_EmptyPlaceName verifies MYR-206's Result
// carrying an empty PlaceName (address-only match — no POI within the
// geocoder's distance threshold) still produces a Vehicle row write:
// LocationName is set to the empty string (the documented
// vehicle-state.schema.json sentinel for "no geocode available", NOT
// omitted from the update) while LocationAddr carries the resolved
// street address.
func TestScheduleLocationGeocode_EmptyPlaceName(t *testing.T) {
	vehicles := &mockVehicleUpdater{}
	geo := &stubGeocoder{
		results: []geocode.Result{{PlaceName: "", Address: "4220 Tributary Wy"}},
	}
	w := newLocAddrTestWriter(t, vehicles, geo)

	update := &VehicleUpdate{
		Latitude:    floatPtr(33.086131),
		Longitude:   floatPtr(-96.852202),
		LastUpdated: time.Now(),
	}
	w.scheduleLocationGeocode(testVIN, update)

	awaitGeocodeCalls(t, geo, 1)
	awaitVehicleWrites(t, vehicles, 1)
	w.geocodeWG.Wait()

	writes := vehicles.getTelemetryWrites()
	if len(writes) != 1 {
		t.Fatalf("vehicle writes = %d, want 1", len(writes))
	}
	if writes[0].Update.LocationName == nil || *writes[0].Update.LocationName != "" {
		t.Errorf("LocationName = %v, want empty string (no POI within threshold)", ptrVal(writes[0].Update.LocationName))
	}
	if writes[0].Update.LocationAddr == nil || *writes[0].Update.LocationAddr != "4220 Tributary Wy" {
		t.Errorf("LocationAddr = %v, want %q", ptrVal(writes[0].Update.LocationAddr), "4220 Tributary Wy")
	}
}

// TestScheduleLocationGeocode_DebouncedFlushes drives each branch of the
// debounce decision via a single table: the cache is pre-seeded with an
// anchor entry, the scheduler is invoked once, and the test asserts
// whether the geocoder was called. Mutating the cache directly is the
// cheapest way to simulate elapsed time / distance without sleeping.
func TestScheduleLocationGeocode_DebouncedFlushes(t *testing.T) {
	// Reference anchor: a known (lat, lng) fired ago at some time before
	// now. Individual cases override delta + distance to test each branch.
	const (
		anchorLat = 33.086131
		anchorLng = -96.852202
	)

	tests := []struct {
		name           string
		seedCache      bool
		entry          locAddrEntry
		updateLat      *float64
		updateLng      *float64
		wantFire       bool
		wantWriteCount int
	}{
		{
			name:      "time debounce: same coords 1s later",
			seedCache: true,
			entry: locAddrEntry{
				lat:         anchorLat,
				lng:         anchorLng,
				address:     "4220 Tributary Wy",
				lastFiredAt: time.Now().Add(-1 * time.Second),
			},
			updateLat: floatPtr(anchorLat),
			updateLng: floatPtr(anchorLng),
			wantFire:  false,
		},
		{
			name:      "distance debounce: 10m away within 1s",
			seedCache: true,
			entry: locAddrEntry{
				lat:         anchorLat,
				lng:         anchorLng,
				address:     "4220 Tributary Wy",
				lastFiredAt: time.Now().Add(-1 * time.Second),
			},
			// ~10m north (1 degree latitude ≈ 111_000m, so 1e-4 deg ≈ 11m)
			updateLat: floatPtr(anchorLat + 1e-4),
			updateLng: floatPtr(anchorLng),
			wantFire:  false,
		},
		{
			name:      "distance debounce: 5m away after 70s",
			seedCache: true,
			entry: locAddrEntry{
				lat:         anchorLat,
				lng:         anchorLng,
				address:     "4220 Tributary Wy",
				lastFiredAt: time.Now().Add(-70 * time.Second),
			},
			// ~5m north (5/111_000 deg)
			updateLat: floatPtr(anchorLat + 5.0/111_000.0),
			updateLng: floatPtr(anchorLng),
			wantFire:  false,
		},
		{
			name:      "both pass: 100m away after 70s",
			seedCache: true,
			entry: locAddrEntry{
				lat:         anchorLat,
				lng:         anchorLng,
				address:     "4220 Tributary Wy",
				lastFiredAt: time.Now().Add(-70 * time.Second),
			},
			// ~100m north (100/111_000 deg)
			updateLat:      floatPtr(anchorLat + 100.0/111_000.0),
			updateLng:      floatPtr(anchorLng),
			wantFire:       true,
			wantWriteCount: 1,
		},
		{
			name:      "inFlight blocks: in-flight even when stale",
			seedCache: true,
			entry: locAddrEntry{
				lat:         anchorLat,
				lng:         anchorLng,
				address:     "4220 Tributary Wy",
				lastFiredAt: time.Now().Add(-10 * time.Minute),
				inFlight:    true,
			},
			updateLat: floatPtr(anchorLat + 1.0), // ~111km away — distance/time clearly pass
			updateLng: floatPtr(anchorLng),
			wantFire:  false,
		},
		{
			name:      "missing latitude: no fire",
			seedCache: false,
			updateLat: nil,
			updateLng: floatPtr(anchorLng),
			wantFire:  false,
		},
		{
			name:      "missing longitude: no fire",
			seedCache: false,
			updateLat: floatPtr(anchorLat),
			updateLng: nil,
			wantFire:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vehicles := &mockVehicleUpdater{}
			geo := &stubGeocoder{
				results: []geocode.Result{{PlaceName: "Other", Address: "Elsewhere"}},
			}
			w := newLocAddrTestWriter(t, vehicles, geo)

			// Pre-seed the cache only when the case configured one. Cases
			// that test missing-coord paths leave the cache empty so the
			// "no fire" assertion verifies the lat/lng nil guard, not the
			// debounce path.
			if tt.seedCache {
				w.locAddrMu.Lock()
				w.locAddrCache[testVIN] = tt.entry
				w.locAddrMu.Unlock()
			}

			update := &VehicleUpdate{
				Latitude:    tt.updateLat,
				Longitude:   tt.updateLng,
				LastUpdated: time.Now(),
			}
			w.scheduleLocationGeocode(testVIN, update)

			if tt.wantFire {
				awaitGeocodeCalls(t, geo, 1)
			}
			// Always wait on the WG so we can't false-positive a "no fire"
			// case because the goroutine simply hasn't run yet.
			w.geocodeWG.Wait()

			gotCalls := geo.callCount()
			if tt.wantFire && gotCalls != 1 {
				t.Errorf("geocoder calls = %d, want 1", gotCalls)
			}
			if !tt.wantFire && gotCalls != 0 {
				t.Errorf("geocoder calls = %d, want 0 (debounce should skip)", gotCalls)
			}

			gotWrites := len(vehicles.getTelemetryWrites())
			if gotWrites != tt.wantWriteCount {
				t.Errorf("vehicle writes = %d, want %d", gotWrites, tt.wantWriteCount)
			}
		})
	}
}

// TestScheduleLocationGeocode_NilUpdate is a paranoia guard: nil
// VehicleUpdate from a buggy caller must return cleanly, not panic.
// Mirrors the destination-side defensive checks.
func TestScheduleLocationGeocode_NilUpdate(t *testing.T) {
	vehicles := &mockVehicleUpdater{}
	geo := &stubGeocoder{}
	w := newLocAddrTestWriter(t, vehicles, geo)

	w.scheduleLocationGeocode(testVIN, nil)
	w.geocodeWG.Wait()

	if got := geo.callCount(); got != 0 {
		t.Errorf("geocoder calls = %d, want 0", got)
	}
}

// TestScheduleLocationGeocode_ZeroZeroSentinel verifies the scheduler
// filters the (0,0) GPS sentinel the same way geocodeParkedLocation and
// the drive-start/end paths do. A cold-start / mock / buggy upstream
// shipping Latitude:&0, Longitude:&0 must not burn a Mapbox call on the
// Atlantic (asymmetric with field_mapper.applyLocation which does not
// filter the pair).
func TestScheduleLocationGeocode_ZeroZeroSentinel(t *testing.T) {
	vehicles := &mockVehicleUpdater{}
	geo := &stubGeocoder{}
	w := newLocAddrTestWriter(t, vehicles, geo)

	zeroLat, zeroLng := 0.0, 0.0
	w.scheduleLocationGeocode(testVIN, &VehicleUpdate{
		Latitude:  &zeroLat,
		Longitude: &zeroLng,
	})
	w.geocodeWG.Wait()

	if got := geo.callCount(); got != 0 {
		t.Errorf("geocoder calls = %d, want 0 (zero-zero sentinel must be filtered)", got)
	}
	if writes := vehicles.getTelemetryWrites(); len(writes) != 0 {
		t.Errorf("vehicle writes = %d, want 0", len(writes))
	}
}

// TestGeocodeParkedLocation_HappyPath verifies the synchronous drive-end
// path: geocoder is called once, Vehicle row is written before the
// function returns (no waitgroup), and the cache anchor + lastFiredAt
// are refreshed so the next flush within the debounce window
// short-circuits.
func TestGeocodeParkedLocation_HappyPath(t *testing.T) {
	vehicles := &mockVehicleUpdater{}
	geo := &stubGeocoder{
		results: []geocode.Result{{PlaceName: "Trailhead", Address: "1 Park Rd"}},
	}
	w := newLocAddrTestWriter(t, vehicles, geo)

	const (
		lat = 33.0
		lng = -96.0
	)
	before := time.Now()
	w.geocodeParkedLocation(context.Background(), testVIN, lat, lng)

	if got := geo.callCount(); got != 1 {
		t.Fatalf("geocoder calls = %d, want 1", got)
	}
	writes := vehicles.getTelemetryWrites()
	if len(writes) != 1 {
		t.Fatalf("vehicle writes = %d, want 1 (sync path must write before return)", len(writes))
	}
	if writes[0].Update.LocationAddr == nil || *writes[0].Update.LocationAddr != "1 Park Rd" {
		t.Errorf("LocationAddr = %v, want %q", ptrVal(writes[0].Update.LocationAddr), "1 Park Rd")
	}

	w.locAddrMu.Lock()
	entry := w.locAddrCache[testVIN]
	w.locAddrMu.Unlock()
	if entry.address != "1 Park Rd" {
		t.Errorf("cache address = %q, want %q", entry.address, "1 Park Rd")
	}
	if entry.lat != lat || entry.lng != lng {
		t.Errorf("cache anchor = (%v,%v), want (%v,%v)", entry.lat, entry.lng, lat, lng)
	}
	if entry.lastFiredAt.Before(before) {
		t.Errorf("lastFiredAt = %v, want >= %v (refreshed by parked geocode)", entry.lastFiredAt, before)
	}
}

// TestGeocodeParkedLocation_NoResult verifies the silent-no-result path:
// when the geocoder returns ErrNoResult (NoopGeocoder, or Mapbox had no
// match) the function must NOT write to the Vehicle row and NOT log at
// Warn — it's the routine fallback path, not an operator-actionable
// failure.
func TestGeocodeParkedLocation_NoResult(t *testing.T) {
	vehicles := &mockVehicleUpdater{}
	geo := &stubGeocoder{} // empty results → ErrNoResult on every call
	w := newLocAddrTestWriter(t, vehicles, geo)

	w.geocodeParkedLocation(context.Background(), testVIN, 33.0, -96.0)

	if got := geo.callCount(); got != 1 {
		t.Errorf("geocoder calls = %d, want 1", got)
	}
	if writes := vehicles.getTelemetryWrites(); len(writes) != 0 {
		t.Errorf("vehicle writes = %d, want 0 (ErrNoResult is silent)", len(writes))
	}
}

// TestGeocodeParkedLocation_ZeroZero verifies the (0,0) guard: an
// uninitialized end-location coordinate must be skipped (zero-zero is
// the "no data" sentinel Tesla uses on cold-start) rather than firing a
// geocode for the middle of the Atlantic.
func TestGeocodeParkedLocation_ZeroZero(t *testing.T) {
	vehicles := &mockVehicleUpdater{}
	geo := &stubGeocoder{}
	w := newLocAddrTestWriter(t, vehicles, geo)

	w.geocodeParkedLocation(context.Background(), testVIN, 0, 0)

	if got := geo.callCount(); got != 0 {
		t.Errorf("geocoder calls = %d, want 0 (0,0 sentinel skipped)", got)
	}
}

// TestHaversineMeters spot-checks the great-circle helper against known
// fixtures. Exact equality is too strict for trig — we accept results
// within ~0.1% of the published value.
func TestHaversineMeters(t *testing.T) {
	tests := []struct {
		name                   string
		lat1, lng1, lat2, lng2 float64
		wantMeters             float64
		tolerance              float64
	}{
		{
			name:       "identical points",
			lat1:       33.086131, lng1: -96.852202,
			lat2: 33.086131, lng2: -96.852202,
			wantMeters: 0,
			tolerance:  0.001,
		},
		{
			name: "1e-4 degrees latitude north ≈ 11.1m",
			lat1: 33.086131, lng1: -96.852202,
			lat2: 33.086231, lng2: -96.852202,
			wantMeters: 11.1,
			tolerance:  0.5,
		},
		{
			name: "0.001 degrees latitude north ≈ 111m",
			lat1: 33.086131, lng1: -96.852202,
			lat2: 33.087131, lng2: -96.852202,
			wantMeters: 111.1,
			tolerance:  0.5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := haversineMeters(tt.lat1, tt.lng1, tt.lat2, tt.lng2)
			diff := got - tt.wantMeters
			if diff < 0 {
				diff = -diff
			}
			if diff > tt.tolerance {
				t.Errorf("haversineMeters = %v, want %v ± %v", got, tt.wantMeters, tt.tolerance)
			}
		})
	}
}

// Compile-time assertion that mockVehicleUpdater satisfies vehicleUpdater
// — guards against signature drift while leaving the test file using
// the existing fake from writer_test.go.
var _ vehicleUpdater = (*mockVehicleUpdater)(nil)

// Compile-time assertion that stubGeocoder satisfies geocode.Geocoder.
var _ geocode.Geocoder = (*stubGeocoder)(nil)
