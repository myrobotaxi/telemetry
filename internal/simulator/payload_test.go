package simulator

import (
	"testing"

	"google.golang.org/protobuf/proto"

	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

func TestBuildPayload_Fields(t *testing.T) {
	state := ScenarioState{
		Speed:          65.5,
		Latitude:       32.7767,
		Longitude:      -96.7970,
		Heading:        45.0,
		GearPosition:   "D",
		ChargeLevel:    80,
		EstimatedRange: 200,
		InteriorTemp:   22,
		ExteriorTemp:   28,
		OdometerMiles:  12500.5,
	}

	payload := BuildPayload("5YJ3SIM00001", state)

	if payload.GetVin() != "5YJ3SIM00001" {
		t.Errorf("VIN = %q, want %q", payload.GetVin(), "5YJ3SIM00001")
	}
	if payload.GetCreatedAt() == nil {
		t.Fatal("CreatedAt is nil")
	}
	if len(payload.GetData()) != 9 {
		t.Fatalf("data length = %d, want 9", len(payload.GetData()))
	}

	// Verify specific field encodings.
	fieldMap := make(map[tpb.Field]*tpb.Value)
	for _, d := range payload.GetData() {
		fieldMap[d.GetKey()] = d.GetValue()
	}

	// Speed should be a string value.
	speedVal := fieldMap[tpb.Field_VehicleSpeed]
	if speedVal == nil {
		t.Fatal("VehicleSpeed datum missing")
	}
	if sv, ok := speedVal.Value.(*tpb.Value_StringValue); !ok {
		t.Errorf("VehicleSpeed type = %T, want StringValue", speedVal.Value)
	} else if sv.StringValue != "65.50" {
		t.Errorf("VehicleSpeed = %q, want %q", sv.StringValue, "65.50")
	}

	// Location should be a LocationValue.
	locVal := fieldMap[tpb.Field_Location]
	if locVal == nil {
		t.Fatal("Location datum missing")
	}
	loc := locVal.GetLocationValue()
	if loc == nil {
		t.Fatal("LocationValue is nil")
	}
	if loc.GetLatitude() != 32.7767 {
		t.Errorf("latitude = %f, want 32.7767", loc.GetLatitude())
	}
	if loc.GetLongitude() != -96.7970 {
		t.Errorf("longitude = %f, want -96.7970", loc.GetLongitude())
	}

	// Gear should be a ShiftStateValue.
	gearVal := fieldMap[tpb.Field_Gear]
	if gearVal == nil {
		t.Fatal("Gear datum missing")
	}
	if ss, ok := gearVal.Value.(*tpb.Value_ShiftStateValue); !ok {
		t.Errorf("Gear type = %T, want ShiftStateValue", gearVal.Value)
	} else if ss.ShiftStateValue != tpb.ShiftState_ShiftStateD {
		t.Errorf("Gear = %v, want ShiftStateD", ss.ShiftStateValue)
	}
}

func TestBuildPayload_AllGears(t *testing.T) {
	tests := []struct {
		gear string
		want tpb.ShiftState
	}{
		{"P", tpb.ShiftState_ShiftStateP},
		{"D", tpb.ShiftState_ShiftStateD},
		{"R", tpb.ShiftState_ShiftStateR},
		{"N", tpb.ShiftState_ShiftStateN},
		{"X", tpb.ShiftState_ShiftStateInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.gear, func(t *testing.T) {
			state := scenarioDefaults()
			state.GearPosition = tt.gear
			payload := BuildPayload("TEST", state)

			for _, d := range payload.GetData() {
				if d.GetKey() != tpb.Field_Gear {
					continue
				}
				ss, ok := d.GetValue().Value.(*tpb.Value_ShiftStateValue)
				if !ok {
					t.Fatalf("Gear type = %T, want ShiftStateValue", d.GetValue().Value)
				}
				if ss.ShiftStateValue != tt.want {
					t.Errorf("ShiftState = %v, want %v", ss.ShiftStateValue, tt.want)
				}
				return
			}
			t.Fatal("Gear datum not found")
		})
	}
}

func TestBuildPayload_ETAIncluded(t *testing.T) {
	state := scenarioDefaults()
	state.GearPosition = "D"
	state.Speed = 65
	state.ETA = 15.5

	payload := BuildPayload("5YJ3SIM00001", state)

	// With ETA > 0, data should have 10 items (9 base + MinutesToArrival).
	if len(payload.GetData()) != 10 {
		t.Fatalf("data length = %d, want 10 (9 base + ETA)", len(payload.GetData()))
	}

	fieldMap := make(map[tpb.Field]*tpb.Value)
	for _, d := range payload.GetData() {
		fieldMap[d.GetKey()] = d.GetValue()
	}

	etaVal := fieldMap[tpb.Field_MinutesToArrival]
	if etaVal == nil {
		t.Fatal("MinutesToArrival datum missing")
	}
	sv, ok := etaVal.Value.(*tpb.Value_StringValue)
	if !ok {
		t.Fatalf("MinutesToArrival type = %T, want StringValue", etaVal.Value)
	}
	if sv.StringValue != "15.50" {
		t.Errorf("MinutesToArrival = %q, want %q", sv.StringValue, "15.50")
	}
}

func TestBuildPayload_ETAOmittedWhenZero(t *testing.T) {
	state := scenarioDefaults()
	state.ETA = 0

	payload := BuildPayload("5YJ3SIM00001", state)

	// With ETA=0, should only have the 9 base data items.
	if len(payload.GetData()) != 9 {
		t.Fatalf("data length = %d, want 9 (no ETA field)", len(payload.GetData()))
	}

	for _, d := range payload.GetData() {
		if d.GetKey() == tpb.Field_MinutesToArrival {
			t.Error("MinutesToArrival should not be present when ETA=0")
		}
	}
}

func TestBuildPayload_NavFields(t *testing.T) {
	state := scenarioDefaults()
	state.GearPosition = "D"
	state.Speed = 65
	state.ETA = 15.0
	state.Nav.TripDistanceRemain = 20.5
	state.Nav.RouteLine = "_p~iF~ps|U"
	state.Nav.DestinationName = "McKinney Town Center"
	state.Nav.DestinationLat = 33.1972
	state.Nav.DestinationLng = -96.6153

	payload := BuildPayload("5YJ3SIM00001", state)

	fieldMap := make(map[tpb.Field]*tpb.Value)
	for _, d := range payload.GetData() {
		fieldMap[d.GetKey()] = d.GetValue()
	}

	// MilesToArrival
	if val := fieldMap[tpb.Field_MilesToArrival]; val == nil {
		t.Error("MilesToArrival datum missing")
	} else if sv, ok := val.Value.(*tpb.Value_StringValue); !ok {
		t.Errorf("MilesToArrival type = %T, want StringValue", val.Value)
	} else if sv.StringValue != "20.50" {
		t.Errorf("MilesToArrival = %q, want %q", sv.StringValue, "20.50")
	}

	// RouteLine
	if val := fieldMap[tpb.Field_RouteLine]; val == nil {
		t.Error("RouteLine datum missing")
	} else if sv, ok := val.Value.(*tpb.Value_StringValue); !ok {
		t.Errorf("RouteLine type = %T, want StringValue", val.Value)
	} else if sv.StringValue != "_p~iF~ps|U" {
		t.Errorf("RouteLine = %q, want %q", sv.StringValue, "_p~iF~ps|U")
	}

	// DestinationName
	if val := fieldMap[tpb.Field_DestinationName]; val == nil {
		t.Error("DestinationName datum missing")
	} else if sv, ok := val.Value.(*tpb.Value_StringValue); !ok {
		t.Errorf("DestinationName type = %T, want StringValue", val.Value)
	} else if sv.StringValue != "McKinney Town Center" {
		t.Errorf("DestinationName = %q, want %q", sv.StringValue, "McKinney Town Center")
	}

	// DestinationLocation
	if val := fieldMap[tpb.Field_DestinationLocation]; val == nil {
		t.Error("DestinationLocation datum missing")
	} else {
		loc := val.GetLocationValue()
		if loc == nil {
			t.Fatal("DestinationLocation is not a LocationValue")
		}
		if loc.GetLatitude() != 33.1972 {
			t.Errorf("DestinationLocation.Lat = %f, want 33.1972", loc.GetLatitude())
		}
		if loc.GetLongitude() != -96.6153 {
			t.Errorf("DestinationLocation.Lng = %f, want -96.6153", loc.GetLongitude())
		}
	}
}

func TestBuildPayload_NavFieldsOmittedWhenEmpty(t *testing.T) {
	state := scenarioDefaults()

	payload := BuildPayload("5YJ3SIM00001", state)

	for _, d := range payload.GetData() {
		switch d.GetKey() {
		case tpb.Field_MilesToArrival:
			t.Error("MilesToArrival should not be present without nav data")
		case tpb.Field_RouteLine:
			t.Error("RouteLine should not be present without nav data")
		case tpb.Field_DestinationName:
			t.Error("DestinationName should not be present without nav data")
		case tpb.Field_DestinationLocation:
			t.Error("DestinationLocation should not be present without nav data")
		}
	}
}

func TestMarshalPayload_RoundTrip(t *testing.T) {
	state := scenarioDefaults()
	state.Speed = 42.0
	state.GearPosition = "D"

	raw, err := MarshalPayload("5YJ3SIM00001", state)
	if err != nil {
		t.Fatalf("MarshalPayload: %v", err)
	}

	var payload tpb.Payload
	if err := proto.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if payload.GetVin() != "5YJ3SIM00001" {
		t.Errorf("VIN = %q, want %q", payload.GetVin(), "5YJ3SIM00001")
	}
	if len(payload.GetData()) != 9 {
		t.Errorf("data length = %d, want 9", len(payload.GetData()))
	}
}
