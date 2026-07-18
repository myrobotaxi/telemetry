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

// TestBackfillRow_DryRun exercises backfillRow's decision logic without
// touching the database — dry-run never calls DriveRepo.UpdateAddresses,
// so a nil *store.DriveRepo is safe to pass here (the DB-write path is
// covered by internal/store's own DriveRepo.UpdateAddresses tests).
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
			name: "both sides missing, both geocoded successfully",
			row: store.DriveBackfillRow{
				ID: "drv_both", StartAddress: "", EndAddress: "", RoutePoints: routePoints,
			},
			wantOutcome: backfillUpdated,
		},
		{
			name: "only start missing, end left untouched",
			row: store.DriveBackfillRow{
				ID: "drv_start_only", StartAddress: "", EndAddress: "already set", RoutePoints: routePoints,
			},
			wantOutcome: backfillUpdated,
		},
		{
			name: "no usable route points at all",
			row: store.DriveBackfillRow{
				ID: "drv_no_points", StartAddress: "", EndAddress: "", RoutePoints: json.RawMessage(`[]`),
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
