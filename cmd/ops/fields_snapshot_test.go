package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
)

// TestVehicleSnapshot_JSONShape is the unit-level regression guard for the
// REST snapshot wire shape. It pins:
//   - the seven catalog fields promoted by MYR-24 are serialized with real
//     values (not null/omitted)
//   - the Go struct field FsdMilesToday is emitted under the contract wire
//     name "fsdMilesSinceReset" (MYR-27)
//   - destinationAddress stays nullable (omitempty) when the DB column is NULL
//   - locationName / locationAddress serialize as empty strings (non-nullable
//     per Prisma NOT NULL DEFAULT '') when no reverse geocode is available
func TestVehicleSnapshot_JSONShape(t *testing.T) {
	destAddr := "2001 Market St, San Francisco, CA 94114"
	populated := store.Vehicle{
		ID:                 "clxyz1234567890abcdef",
		VIN:                "5YJ3E1EA1NF000001",
		Name:               "Stumpy",
		Model:              "Model 3",
		Year:               2024,
		Color:              "Midnight Silver Metallic",
		Status:             store.VehicleStatusParked,
		LocationName:       "Home",
		LocationAddress:    "123 Market St, San Francisco, CA",
		FsdMilesToday:      412.7,
		DestinationAddress: &destAddr,
		LastUpdated:        time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC),
	}

	snap := toSnapshot(populated)
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	assertField(t, got, "model", "Model 3")
	assertField(t, got, "year", float64(2024))
	assertField(t, got, "color", "Midnight Silver Metallic")
	assertField(t, got, "locationName", "Home")
	assertField(t, got, "locationAddress", "123 Market St, San Francisco, CA")
	assertField(t, got, "fsdMilesSinceReset", 412.7)
	assertField(t, got, "destinationAddress", destAddr)

	if _, present := got["fsdMilesToday"]; present {
		t.Errorf("snapshot leaks DB column name fsdMilesToday; wire name must be fsdMilesSinceReset")
	}

	// Empty-strings / nil branches: nullable destinationAddress must omit.
	minimal := store.Vehicle{
		ID:          "clxyz1234567890abcdef",
		VIN:         "5YJ3E1EA1NF000002",
		Name:        "Tiny",
		Model:       "Model Y",
		Year:        2023,
		Color:       "Pearl White",
		Status:      store.VehicleStatusOffline,
		LastUpdated: time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC),
	}
	raw, err = json.Marshal(toSnapshot(minimal))
	if err != nil {
		t.Fatalf("marshal minimal: %v", err)
	}
	var gotMin map[string]any
	if err := json.Unmarshal(raw, &gotMin); err != nil {
		t.Fatalf("unmarshal minimal: %v", err)
	}
	assertField(t, gotMin, "locationName", "")
	assertField(t, gotMin, "locationAddress", "")
	assertField(t, gotMin, "fsdMilesSinceReset", float64(0))
	if _, present := gotMin["destinationAddress"]; present {
		t.Errorf("nil destinationAddress should be omitted, got %v", gotMin["destinationAddress"])
	}
}

// toSnapshot mirrors the assignment block in runFieldsSnapshot without the
// DB lookup, so the test can exercise the JSON mapping in isolation. Keep
// the field list here in sync with fields.go.
func toSnapshot(v store.Vehicle) vehicleSnapshot {
	return vehicleSnapshot{
		ID:                   v.ID,
		VIN:                  v.VIN,
		Name:                 v.Name,
		Model:                v.Model,
		Year:                 v.Year,
		Color:                v.Color,
		Status:               string(v.Status),
		ChargeLevel:          v.ChargeLevel,
		EstimatedRange:       v.EstimatedRange,
		Speed:                v.Speed,
		GearPosition:         v.GearPosition,
		Heading:              v.Heading,
		Latitude:             v.Latitude,
		Longitude:            v.Longitude,
		LocationName:         v.LocationName,
		LocationAddress:      v.LocationAddress,
		InteriorTemp:         v.InteriorTemp,
		ExteriorTemp:         v.ExteriorTemp,
		OdometerMiles:        v.OdometerMiles,
		FsdMilesSinceReset:   v.FsdMilesToday,
		DestinationName:      v.DestinationName,
		DestinationAddress:   v.DestinationAddress,
		DestinationLatitude:  v.DestinationLatitude,
		DestinationLongitude: v.DestinationLongitude,
		OriginLatitude:       v.OriginLatitude,
		OriginLongitude:      v.OriginLongitude,
		EtaMinutes:           v.EtaMinutes,
		TripDistRemaining:    v.TripDistRemaining,
		NavRouteCoordinates:  v.NavRouteCoordinates,
		LastUpdated:          v.LastUpdated.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

func assertField(t *testing.T, m map[string]any, key string, want any) {
	t.Helper()
	got, ok := m[key]
	if !ok {
		t.Errorf("snapshot missing field %q", key)
		return
	}
	if got != want {
		t.Errorf("snapshot[%q] = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}
