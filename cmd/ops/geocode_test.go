package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/geocode"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// fakeGeocoder is a test double implementing geocode.Geocoder. Results
// and errors are keyed by "%.4f,%.4f" of (lat, lng) so tests can pin
// exact coordinates to exact outcomes without touching the network.
type fakeGeocoder struct {
	results map[string]*geocode.Result
	errs    map[string]error
}

func (f *fakeGeocoder) ReverseGeocode(_ context.Context, lat, lng float64) (*geocode.Result, error) {
	key := fmt.Sprintf("%.4f,%.4f", lat, lng)
	if err, ok := f.errs[key]; ok {
		return nil, err
	}
	if r, ok := f.results[key]; ok {
		return r, nil
	}
	return nil, geocode.ErrNoResult
}

// discardLogger is a slog.Logger that writes nowhere, so test output
// stays focused on t.Errorf/t.Fatalf failures.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDecodeRouteEndpoints(t *testing.T) {
	tests := []struct {
		name      string
		raw       json.RawMessage
		wantOK    bool
		wantStart store.RoutePointRecord
		wantEnd   store.RoutePointRecord
	}{
		{
			name:   "empty array",
			raw:    json.RawMessage(`[]`),
			wantOK: false,
		},
		{
			name:   "nil/empty raw message",
			raw:    nil,
			wantOK: false,
		},
		{
			name:   "malformed JSON",
			raw:    json.RawMessage(`not json`),
			wantOK: false,
		},
		{
			name:      "single point is both start and end",
			raw:       json.RawMessage(`[{"lat":33.0860,"lng":-96.8522,"speed":0,"heading":0,"timestamp":"2026-07-17T10:00:00Z"}]`),
			wantOK:    true,
			wantStart: store.RoutePointRecord{Latitude: 33.0860, Longitude: -96.8522, Timestamp: "2026-07-17T10:00:00Z"},
			wantEnd:   store.RoutePointRecord{Latitude: 33.0860, Longitude: -96.8522, Timestamp: "2026-07-17T10:00:00Z"},
		},
		{
			name: "multiple points, first and last returned",
			raw: json.RawMessage(`[
				{"lat":33.0860,"lng":-96.8522,"speed":0,"heading":0,"timestamp":"2026-07-17T10:00:00Z"},
				{"lat":33.0900,"lng":-96.8400,"speed":30,"heading":45,"timestamp":"2026-07-17T10:10:00Z"},
				{"lat":33.1032,"lng":-96.8236,"speed":0,"heading":90,"timestamp":"2026-07-17T10:20:00Z"}
			]`),
			wantOK:    true,
			wantStart: store.RoutePointRecord{Latitude: 33.0860, Longitude: -96.8522, Timestamp: "2026-07-17T10:00:00Z"},
			wantEnd:   store.RoutePointRecord{Latitude: 33.1032, Longitude: -96.8236, Heading: 90, Timestamp: "2026-07-17T10:20:00Z"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, ok := decodeRouteEndpoints(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if start != tt.wantStart {
				t.Errorf("start = %+v, want %+v", start, tt.wantStart)
			}
			if end != tt.wantEnd {
				t.Errorf("end = %+v, want %+v", end, tt.wantEnd)
			}
		})
	}
}

func TestGeocodeSide(t *testing.T) {
	tests := []struct {
		name         string
		pt           store.RoutePointRecord
		geocoder     *fakeGeocoder
		wantLocation string
		wantAddress  string
		wantNil      bool
	}{
		{
			name:    "zero-GPS sentinel is skipped without calling the geocoder",
			pt:      store.RoutePointRecord{Latitude: 0, Longitude: 0},
			wantNil: true,
		},
		{
			name: "successful lookup returns PlaceName/Address",
			pt:   store.RoutePointRecord{Latitude: 33.0860, Longitude: -96.8522},
			geocoder: &fakeGeocoder{results: map[string]*geocode.Result{
				"33.0860,-96.8522": {PlaceName: "Stonebriar", Address: "4220 Tributary Way, Frisco, TX"},
			}},
			wantLocation: "Stonebriar",
			wantAddress:  "4220 Tributary Way, Frisco, TX",
		},
		{
			name: "ErrNoResult is a soft skip",
			pt:   store.RoutePointRecord{Latitude: 1, Longitude: 1},
			geocoder: &fakeGeocoder{errs: map[string]error{
				"1.0000,1.0000": geocode.ErrNoResult,
			}},
			wantNil: true,
		},
		{
			name: "invalid coordinate is a soft skip, not a crash",
			pt:   store.RoutePointRecord{Latitude: 999, Longitude: 999},
			geocoder: &fakeGeocoder{errs: map[string]error{
				"999.0000,999.0000": geocode.ErrInvalidCoordinate,
			}},
			wantNil: true,
		},
		{
			name: "generic transport error is a soft skip",
			pt:   store.RoutePointRecord{Latitude: 2, Longitude: 2},
			geocoder: &fakeGeocoder{errs: map[string]error{
				"2.0000,2.0000": errors.New("connection reset"),
			}},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := tt.geocoder
			if g == nil {
				g = &fakeGeocoder{}
			}
			loc, addr := geocodeSide(context.Background(), g, "drv_test", "start", tt.pt, discardLogger())
			if tt.wantNil {
				if loc != nil || addr != nil {
					t.Fatalf("expected (nil, nil), got (%v, %v)", loc, addr)
				}
				return
			}
			if loc == nil || addr == nil {
				t.Fatalf("expected non-nil location/address, got (%v, %v)", loc, addr)
			}
			if *loc != tt.wantLocation {
				t.Errorf("location = %q, want %q", *loc, tt.wantLocation)
			}
			if *addr != tt.wantAddress {
				t.Errorf("address = %q, want %q", *addr, tt.wantAddress)
			}
		})
	}
}

// fakeDriveAddressUpdater is a test double for driveAddressUpdater so
// backfillRow's post-geocode UPDATE handling — in particular the
// ErrDriveNotFound-as-skip race — can be exercised without a real
// database.
type fakeDriveAddressUpdater struct {
	err   error
	calls int
}

func (f *fakeDriveAddressUpdater) UpdateAddresses(_ context.Context, _ string, _, _, _, _ *string) error {
	f.calls++
	return f.err
}

// TestBackfillRow_DryRun exercises backfillRow's decision logic without
// touching the database — dry-run never calls UpdateAddresses, so a nil
// driveAddressUpdater is safe to pass here.
func TestBackfillRow_DryRun(t *testing.T) {
	routePoints := json.RawMessage(`[
		{"lat":33.0860,"lng":-96.8522,"speed":0,"heading":0,"timestamp":"2026-07-17T10:00:00Z"},
		{"lat":33.1032,"lng":-96.8236,"speed":0,"heading":0,"timestamp":"2026-07-17T10:20:00Z"}
	]`)
	geocoderResults := map[string]*geocode.Result{
		"33.0860,-96.8522": {PlaceName: "Stonebriar", Address: "4220 Tributary Way, Frisco, TX"},
		"33.1032,-96.8236": {PlaceName: "", Address: "Dallas Parkway, Frisco, TX"},
	}

	tests := []struct {
		name        string
		row         store.DriveBackfillRow
		wantOutcome backfillOutcome
	}{
		{
			name: "both sides missing on a closed drive, both geocoded successfully",
			row: store.DriveBackfillRow{
				ID: "drv_both", StartAddress: "", EndAddress: "", EndTime: "2026-07-17T10:20:00Z", RoutePoints: routePoints,
			},
			wantOutcome: backfillUpdated,
		},
		{
			name: "only start missing, end left untouched",
			row: store.DriveBackfillRow{
				ID: "drv_start_only", StartAddress: "", EndAddress: "already set", EndTime: "2026-07-17T10:20:00Z", RoutePoints: routePoints,
			},
			wantOutcome: backfillUpdated,
		},
		{
			name: "no usable route points at all",
			row: store.DriveBackfillRow{
				ID: "drv_no_points", StartAddress: "", EndAddress: "", EndTime: "2026-07-17T10:20:00Z", RoutePoints: json.RawMessage(`[]`),
			},
			wantOutcome: backfillSkipped,
		},
		{
			// MYR-240 adversarial-review fix: an OPEN drive (EndTime=="")
			// must never have its end side geocoded, even though
			// EndAddress=="" matches the naive predicate — the last
			// routePoints entry is the car's current position, not a
			// drive endpoint. Start is still geocoded normally.
			name: "open drive: end side skipped even though endAddress is empty",
			row: store.DriveBackfillRow{
				ID: "drv_open", StartAddress: "", EndAddress: "", EndTime: "", RoutePoints: routePoints,
			},
			wantOutcome: backfillUpdated, // start side still gets written
		},
		{
			// Same open-drive row, but startAddress is ALSO already
			// populated — so after skipping the end side there is
			// nothing left to write at all.
			name: "open drive: nothing to do when start is already populated too",
			row: store.DriveBackfillRow{
				ID: "drv_open_nothing_to_do", StartAddress: "already set", EndAddress: "", EndTime: "", RoutePoints: routePoints,
			},
			wantOutcome: backfillSkipped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &fakeGeocoder{results: geocoderResults}
			got := backfillRow(context.Background(), g, nil, tt.row, true /* dryRun */, discardLogger())
			if got != tt.wantOutcome {
				t.Errorf("outcome = %v, want %v", got, tt.wantOutcome)
			}
		})
	}
}

// TestBackfillRow_ApplyPath exercises backfillRow's non-dry-run branch
// against a fakeDriveAddressUpdater, in particular the MYR-240
// adversarial-review fix: DriveRepo.UpdateAddresses returning
// ErrDriveNotFound (the row was hard-deleted between this backfill's
// SELECT and UPDATE — handleDriveDiscarded does this for micro-drives)
// must be treated as a skip, not a failure.
func TestBackfillRow_ApplyPath(t *testing.T) {
	routePoints := json.RawMessage(`[{"lat":33.0860,"lng":-96.8522,"speed":0,"heading":0,"timestamp":"2026-07-17T10:00:00Z"}]`)
	geocoderResults := map[string]*geocode.Result{
		"33.0860,-96.8522": {PlaceName: "Stonebriar", Address: "4220 Tributary Way, Frisco, TX"},
	}
	row := store.DriveBackfillRow{
		ID: "drv_apply", StartAddress: "", EndAddress: "already set", EndTime: "2026-07-17T10:20:00Z", RoutePoints: routePoints,
	}

	tests := []struct {
		name        string
		updaterErr  error
		wantOutcome backfillOutcome
	}{
		{name: "update succeeds", updaterErr: nil, wantOutcome: backfillUpdated},
		{name: "row deleted before update is a skip, not a failure", updaterErr: store.ErrDriveNotFound, wantOutcome: backfillSkipped},
		{name: "generic DB error is a failure", updaterErr: errors.New("connection refused"), wantOutcome: backfillFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &fakeGeocoder{results: geocoderResults}
			updater := &fakeDriveAddressUpdater{err: tt.updaterErr}
			got := backfillRow(context.Background(), g, updater, row, false /* dryRun */, discardLogger())
			if got != tt.wantOutcome {
				t.Errorf("outcome = %v, want %v", got, tt.wantOutcome)
			}
			if updater.calls != 1 {
				t.Errorf("UpdateAddresses calls = %d, want 1", updater.calls)
			}
		})
	}
}

func TestLogSafePresence(t *testing.T) {
	set := "4220 Tributary Way, Frisco, TX"
	empty := ""

	tests := []struct {
		name string
		in   *string
		want string
	}{
		{name: "nil pointer", in: nil, want: "-"},
		{name: "empty string", in: &empty, want: "empty"},
		{name: "set value never appears in output", in: &set, want: fmt.Sprintf("set(len=%d)", len(set))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := logSafePresence(tt.in)
			if got != tt.want {
				t.Errorf("logSafePresence = %q, want %q", got, tt.want)
			}
			if tt.in != nil && *tt.in != "" && got == *tt.in {
				t.Fatalf("logSafePresence leaked the raw P1 value")
			}
		})
	}
}
