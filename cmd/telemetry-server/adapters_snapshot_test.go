package main

import (
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestVehicleToSnapshotFields locks in the store.Vehicle -> wire-field-map
// projection that backs the WS initial-snapshot path (MYR-137). A
// regression here (e.g. a renamed struct field silently dropping out of
// the map) would reintroduce the exact bug this issue fixes -- model,
// year, color, estimatedRange, and fsdMilesSinceReset silently missing
// from vehicle_update.
func TestVehicleToSnapshotFields(t *testing.T) {
	gear := "D"
	chargeState := "Charging"
	timeToFull := 1.5
	destName := "Golden Gate Bridge"

	v := store.Vehicle{
		ID:                 "veh_1",
		Name:               "Stumpy",
		Model:              "Model 3",
		Year:               2024,
		Color:              "Midnight Silver Metallic",
		Status:             store.VehicleStatusDriving,
		ChargeLevel:        78,
		EstimatedRange:     245,
		ChargeState:        &chargeState,
		TimeToFull:         &timeToFull,
		Speed:              42,
		GearPosition:       &gear,
		Heading:            180,
		Latitude:           37.7749,
		Longitude:          -122.4194,
		LocationName:       "Home",
		LocationAddress:    "123 Market St, San Francisco, CA",
		InteriorTemp:       70,
		ExteriorTemp:       65,
		OdometerMiles:      12345,
		FsdMilesSinceReset: 412.7,
		DestinationName:    &destName,
		LastUpdated:        time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC),
	}

	got := vehicleToSnapshotFields(v)

	wantScalar := map[string]any{
		"name":               "Stumpy",
		"model":              "Model 3",
		"year":               2024,
		"color":              "Midnight Silver Metallic",
		"status":             "driving",
		"speed":              42,
		"heading":            180,
		"latitude":           37.7749,
		"longitude":          -122.4194,
		"locationName":       "Home",
		"locationAddress":    "123 Market St, San Francisco, CA",
		"gearPosition":       "D",
		"chargeLevel":        78,
		"chargeState":        "Charging",
		"estimatedRange":     245,
		"timeToFull":         1.5,
		"interiorTemp":       70,
		"exteriorTemp":       65,
		"odometerMiles":      12345,
		"fsdMilesSinceReset": 412.7,
		"destinationName":    "Golden Gate Bridge",
	}
	for k, want := range wantScalar {
		if got[k] != want {
			t.Errorf("field %q = %#v, want %#v", k, got[k], want)
		}
	}

	// Nullable pointer fields with no value must map to nil, not be
	// absent from the map entirely -- absence vs. explicit null matters
	// because partitionSnapshotFields (internal/ws/snapshot.go) buckets
	// by map key presence, and the wire contract expects these keys to
	// exist with a null value in the steady/no-nav state
	// (vehicle-state-schema.md §2.1 nullability).
	for _, k := range []string{
		"destinationAddress", "destinationLatitude", "destinationLongitude",
		"originLatitude", "originLongitude", "etaMinutes",
		"tripDistanceRemaining", "navRouteCoordinates",
	} {
		val, ok := got[k]
		if !ok {
			t.Errorf("field %q missing from snapshot map (want present with nil value)", k)
			continue
		}
		if val != nil {
			t.Errorf("field %q = %#v, want nil", k, val)
		}
	}

	// The full key set must match exactly what partitionSnapshotFields
	// expects to bucket (vehicleId is intentionally excluded -- it's an
	// envelope-level field, not a member of `fields`).
	if _, ok := got["vehicleId"]; ok {
		t.Error("vehicleToSnapshotFields must not include vehicleId; it belongs at the envelope level")
	}
}
