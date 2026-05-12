package telemetry

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/myrobotaxi/telemetry/internal/events"
	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

func TestDecodeRawPayload_IncludesUntrackedFields(t *testing.T) {
	d := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		// Tracked field (in InternalFieldName): VehicleSpeed
		makeDatum(tpb.Field_VehicleSpeed, stringVal("65.2")),
		// Tracked enum: Gear
		makeDatum(tpb.Field_Gear, &tpb.Value{Value: &tpb.Value_ShiftStateValue{
			ShiftStateValue: tpb.ShiftState_ShiftStateD,
		}}),
		// Location value
		makeDatum(tpb.Field_Location, locationVal(37.7749, -122.4194)),
		// Untracked field to confirm raw decoder does NOT filter it out.
		// BmsState is not in fieldMap but is a valid Tesla field.
		makeDatum(tpb.Field_BMSState, intVal(3)),
		// Invalid datum — should surface with Invalid=true.
		makeDatum(tpb.Field_Odometer, &tpb.Value{Value: &tpb.Value_Invalid{Invalid: true}}),
	})

	raw, err := d.DecodeRawPayload(payload)
	if err != nil {
		t.Fatalf("DecodeRawPayload error: %v", err)
	}
	if raw.VIN != testVIN {
		t.Errorf("VIN: got %q, want %q", raw.VIN, testVIN)
	}
	if len(raw.Fields) != 5 {
		t.Fatalf("expected 5 fields, got %d: %+v", len(raw.Fields), raw.Fields)
	}

	by := indexByProto(raw.Fields)

	speed, ok := by[tpb.Field_VehicleSpeed]
	if !ok || speed.Type != "string" || speed.Value != "65.2" {
		t.Errorf("VehicleSpeed: got %+v", speed)
	}
	if speed.ProtoField != int32(tpb.Field_VehicleSpeed) {
		t.Errorf("VehicleSpeed proto field: got %d, want %d", speed.ProtoField, tpb.Field_VehicleSpeed)
	}

	gear, ok := by[tpb.Field_Gear]
	if !ok || gear.Type != "enum.shift_state" || gear.Value != "D" {
		t.Errorf("Gear: got %+v", gear)
	}

	loc, ok := by[tpb.Field_Location]
	if !ok || loc.Type != "location" {
		t.Errorf("Location: got %+v", loc)
	}
	if l, ok := loc.Value.(events.Location); !ok || l.Latitude != 37.7749 || l.Longitude != -122.4194 {
		t.Errorf("Location value: got %+v", loc.Value)
	}

	bms, ok := by[tpb.Field_BMSState]
	if !ok {
		t.Errorf("untracked BmsState missing from raw output — raw decoder should include unfiltered fields")
	}
	if bms.Type != "int" {
		t.Errorf("BmsState type: got %q", bms.Type)
	}

	odo, ok := by[tpb.Field_Odometer]
	if !ok || !odo.Invalid || odo.Type != "invalid" {
		t.Errorf("Odometer invalid datum: got %+v", odo)
	}
}

func TestDecodeWithRaw_ReturnsBothEvents(t *testing.T) {
	d := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_VehicleSpeed, stringVal("10")),
	})
	env := wrapPayload(t, payload)

	result, rawEvt, err := d.DecodeWithRaw(env)
	if err != nil {
		t.Fatalf("DecodeWithRaw error: %v", err)
	}
	if len(result.Event.Fields) != 1 {
		t.Errorf("filtered event field count: got %d, want 1", len(result.Event.Fields))
	}
	if len(rawEvt.Fields) != 1 {
		t.Errorf("raw event field count: got %d, want 1", len(rawEvt.Fields))
	}
	if rawEvt.Fields[0].ProtoField != int32(tpb.Field_VehicleSpeed) {
		t.Errorf("raw proto field: got %d", rawEvt.Fields[0].ProtoField)
	}
}

func TestDecode_WithoutRaw_DoesNotPopulateRawEvent(t *testing.T) {
	// Decode() (no raw flag) must keep the old behavior — raw event zero value.
	d := NewDecoder()
	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_VehicleSpeed, stringVal("10")),
	})
	env := wrapPayload(t, payload)
	_, err := d.Decode(env)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
}

// wrapPayload marshals the Tesla Payload and wraps it in a FlatBuffers
// envelope for end-to-end decoder tests.
func wrapPayload(t *testing.T, payload *tpb.Payload) []byte {
	t.Helper()
	raw, err := proto.Marshal(payload)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	return BuildTestEnvelope(testVIN, raw)
}

// indexByProto maps raw fields by their Tesla Field enum for easy lookups.
func indexByProto(fields []events.RawTelemetryField) map[tpb.Field]events.RawTelemetryField {
	m := make(map[tpb.Field]events.RawTelemetryField, len(fields))
	for _, f := range fields {
		m[tpb.Field(f.ProtoField)] = f
	}
	return m
}
