package drives

import (
	"context"
	"testing"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
)

func TestStartDrive_ZeroLocation(t *testing.T) {
	tests := []struct {
		name        string
		eventLoc    *events.Location // location in the telemetry event
		cachedLoc   *events.Location // state.lastLocation
		wantLat     float64
		wantLng     float64
		wantZeroLoc bool // expect startLoc to be zero-value
	}{
		{
			name:        "valid event location used",
			eventLoc:    &events.Location{Latitude: 33.09, Longitude: -96.82},
			cachedLoc:   nil,
			wantLat:     33.09,
			wantLng:     -96.82,
			wantZeroLoc: false,
		},
		{
			name: "zero event location with valid cached stays zero",
			// When the event carries a (0,0) location, handleTelemetry
			// overwrites state.lastLocation before startDrive runs, so
			// the cached value is lost. Both branches see (0,0).
			eventLoc:    &events.Location{Latitude: 0, Longitude: 0},
			cachedLoc:   &events.Location{Latitude: 34.05, Longitude: -118.25},
			wantZeroLoc: true,
		},
		{
			name:        "nil event location falls back to valid cached",
			eventLoc:    nil,
			cachedLoc:   &events.Location{Latitude: 34.05, Longitude: -118.25},
			wantLat:     34.05,
			wantLng:     -118.25,
			wantZeroLoc: false,
		},
		{
			name:        "zero event location and zero cached stays zero",
			eventLoc:    &events.Location{Latitude: 0, Longitude: 0},
			cachedLoc:   &events.Location{Latitude: 0, Longitude: 0},
			wantZeroLoc: true,
		},
		{
			name:        "zero event location and nil cached stays zero",
			eventLoc:    &events.Location{Latitude: 0, Longitude: 0},
			cachedLoc:   nil,
			wantZeroLoc: true,
		},
		{
			name:        "nil event location and nil cached stays zero",
			eventLoc:    nil,
			cachedLoc:   nil,
			wantZeroLoc: true,
		},
		{
			name:        "nil event location and zero cached stays zero",
			eventLoc:    nil,
			cachedLoc:   &events.Location{Latitude: 0, Longitude: 0},
			wantZeroLoc: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := testBus()
			defer bus.Close(context.Background())

			d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{})
			if err := d.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = d.Stop() }()

			startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)

			// Build the telemetry fields.
			fields := map[string]events.TelemetryValue{
				string(telemetry.FieldGear):  {StringVal: ptr("D")},
				string(telemetry.FieldSpeed): {FloatVal: ptr(25.0)},
			}
			if tt.eventLoc != nil {
				fields[string(telemetry.FieldLocation)] = events.TelemetryValue{
					LocationVal: tt.eventLoc,
				}
			}

			// Pre-seed the vehicle state with a cached location and gear=D
			// so that handleIdle triggers startDrive.
			state := &vehicleState{
				lastGear:     "D",
				lastLocation: tt.cachedLoc,
			}
			d.states.Store("VIN_ZERO", state)

			now := time.Now()
			te := telemetryEvent("VIN_ZERO", now, fields)
			publishTelemetry(t, bus, te)

			evt, ok := waitForEvent(startedCh)
			if !ok {
				t.Fatal("timed out waiting for DriveStartedEvent")
			}

			payload, ok := evt.Payload.(events.DriveStartedEvent)
			if !ok {
				t.Fatalf("expected DriveStartedEvent, got %T", evt.Payload)
			}

			if tt.wantZeroLoc {
				if payload.Location.Latitude != 0 || payload.Location.Longitude != 0 {
					t.Errorf("expected zero location, got (%f, %f)",
						payload.Location.Latitude, payload.Location.Longitude)
				}
			} else {
				if payload.Location.Latitude != tt.wantLat {
					t.Errorf("Latitude: got %f, want %f", payload.Location.Latitude, tt.wantLat)
				}
				if payload.Location.Longitude != tt.wantLng {
					t.Errorf("Longitude: got %f, want %f", payload.Location.Longitude, tt.wantLng)
				}
			}
		})
	}
}
