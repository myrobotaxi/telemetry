package telemetry

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	tpb "github.com/tnando/my-robo-taxi-telemetry/internal/telemetry/proto/tesla"
)

// testVIN is a fake VIN used in tests. Not a real vehicle.
const testVIN = "5YJ3E7EB2NF000001"

// makePayload creates a valid Tesla Payload with the given data.
func makePayload(data []*tpb.Datum) *tpb.Payload {
	return &tpb.Payload{
		Vin:       testVIN,
		CreatedAt: timestamppb.Now(),
		Data:      data,
	}
}

// makeDatum creates a Datum with the given field and value.
func makeDatum(field tpb.Field, value *tpb.Value) *tpb.Datum {
	return &tpb.Datum{Key: field, Value: value}
}

// stringVal creates a Value with a string_value.
func stringVal(s string) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_StringValue{StringValue: s}}
}

// floatVal creates a Value with a float_value.
func floatVal(f float32) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_FloatValue{FloatValue: f}}
}

// doubleVal creates a Value with a double_value.
func doubleVal(d float64) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_DoubleValue{DoubleValue: d}}
}

// intVal creates a Value with an int_value.
func intVal(i int32) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_IntValue{IntValue: i}}
}

// longVal creates a Value with a long_value.
func longVal(l int64) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_LongValue{LongValue: l}}
}

// locationVal creates a Value with a location_value.
func locationVal(lat, lng float64) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_LocationValue{
		LocationValue: &tpb.LocationValue{Latitude: lat, Longitude: lng},
	}}
}

// shiftStateVal creates a Value with a shift_state_value.
func shiftStateVal(ss tpb.ShiftState) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_ShiftStateValue{ShiftStateValue: ss}}
}

// detailedChargeStateVal creates a Value with a detailed_charge_state_value.
func detailedChargeStateVal(cs tpb.DetailedChargeStateValue) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_DetailedChargeStateValue{DetailedChargeStateValue: cs}}
}

// chargingVal creates a Value with the deprecated charging_value.
func chargingVal(cs tpb.ChargingState) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_ChargingValue{ChargingValue: cs}}
}

// carTypeVal creates a Value with a car_type_value.
func carTypeVal(ct tpb.CarTypeValue) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_CarTypeValue{CarTypeValue: ct}}
}

// boolVal creates a Value with a boolean_value.
func boolVal(b bool) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_BooleanValue{BooleanValue: b}}
}

// invalidVal creates a Value with the invalid flag set.
func invalidVal() *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_Invalid{Invalid: true}}
}

func TestDecoder_DecodePayload_Validation(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	tests := []struct {
		name    string
		payload *tpb.Payload
		wantErr error
	}{
		{
			name:    "nil payload",
			payload: nil,
			wantErr: ErrEmptyPayload,
		},
		{
			name:    "missing VIN",
			payload: &tpb.Payload{CreatedAt: timestamppb.Now(), Data: []*tpb.Datum{{}}},
			wantErr: ErrMissingVIN,
		},
		{
			name:    "missing timestamp",
			payload: &tpb.Payload{Vin: testVIN, Data: []*tpb.Datum{{}}},
			wantErr: ErrMissingTimestamp,
		},
		{
			name:    "empty data",
			payload: &tpb.Payload{Vin: testVIN, CreatedAt: timestamppb.Now()},
			wantErr: ErrEmptyPayload,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := dec.DecodePayload(tt.payload)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("DecodePayload() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestDecoder_Decode_FlatBuffersEnvelope(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_VehicleSpeed, stringVal("65.2")),
	})
	raw, err := proto.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}

	envelope := BuildTestEnvelope(testVIN, raw)

	result, err := dec.Decode(envelope)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(result.FieldErrors) != 0 {
		t.Errorf("unexpected field errors: %v", result.FieldErrors)
	}
	if result.Event.VIN != testVIN {
		t.Errorf("VIN = %q, want %q", result.Event.VIN, testVIN)
	}
	if result.Topic != "V" {
		t.Errorf("Topic = %q, want %q", result.Topic, "V")
	}
	if result.DeviceID != testVIN {
		t.Errorf("DeviceID = %q, want %q", result.DeviceID, testVIN)
	}

	speed, ok := result.Event.Fields["speed"]
	if !ok {
		t.Fatal("missing speed field")
	}
	if speed.FloatVal == nil || *speed.FloatVal != 65.2 {
		t.Errorf("speed = %v, want 65.2", speed)
	}
}

func TestDecoder_Decode_VINFallbackFromEnvelope(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	// Payload with empty VIN — vehicle doesn't populate it in typed format.
	payload := &tpb.Payload{
		Vin:       "",
		CreatedAt: timestamppb.Now(),
		Data:      []*tpb.Datum{makeDatum(tpb.Field_VehicleSpeed, stringVal("55"))},
	}
	raw, err := proto.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	envelope := BuildTestEnvelope(testVIN, raw)
	result, err := dec.Decode(envelope)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if result.Event.VIN != testVIN {
		t.Errorf("VIN = %q, want %q (from envelope fallback)", result.Event.VIN, testVIN)
	}
}

func TestDecoder_Decode_BothVINsEmpty(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := &tpb.Payload{
		Vin:       "",
		CreatedAt: timestamppb.Now(),
		Data:      []*tpb.Datum{makeDatum(tpb.Field_VehicleSpeed, stringVal("55"))},
	}
	raw, err := proto.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Envelope with empty deviceId AND empty payload VIN.
	envelope := BuildTestEnvelope("", raw)
	_, err = dec.Decode(envelope)
	if err == nil {
		t.Fatal("expected error when both VINs are empty, got nil")
	}
	if !errors.Is(err, ErrMissingVIN) {
		t.Errorf("error = %v, want ErrMissingVIN", err)
	}
}

func TestDecoder_Decode_InvalidEnvelope(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	_, err := dec.Decode([]byte("not a valid flatbuffers envelope"))
	if err == nil {
		t.Error("expected error for invalid envelope, got nil")
	}
}

func TestDecoder_Decode_EmptyEnvelope(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	_, err := dec.Decode(nil)
	if err == nil {
		t.Error("expected error for nil input, got nil")
	}

	_, err = dec.Decode([]byte{})
	if err == nil {
		t.Error("expected error for empty input, got nil")
	}
}

func TestDecoder_DecodePayload_SpeedFormats(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	tests := []struct {
		name      string
		value     *tpb.Value
		wantFloat float64
	}{
		{
			name:      "string value",
			value:     stringVal("72.5"),
			wantFloat: 72.5,
		},
		{
			name:      "float value",
			value:     floatVal(72.5),
			wantFloat: 72.5,
		},
		{
			name:      "double value",
			value:     doubleVal(72.5),
			wantFloat: 72.5,
		},
		{
			name:      "zero string",
			value:     stringVal("0"),
			wantFloat: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := makePayload([]*tpb.Datum{
				makeDatum(tpb.Field_VehicleSpeed, tt.value),
			})

			evt, fieldErrs, err := dec.DecodePayload(payload)
			if err != nil {
				t.Fatalf("DecodePayload() error = %v", err)
			}
			if len(fieldErrs) != 0 {
				t.Errorf("unexpected field errors: %v", fieldErrs)
			}

			speed := evt.Fields["speed"]
			if speed.FloatVal == nil {
				t.Fatal("speed.FloatVal is nil")
			}
			if *speed.FloatVal != tt.wantFloat {
				t.Errorf("speed = %f, want %f", *speed.FloatVal, tt.wantFloat)
			}
		})
	}
}

func TestDecoder_DecodePayload_IntegerFields(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	tests := []struct {
		name    string
		value   *tpb.Value
		wantInt int64
	}{
		{
			name:    "int32 value",
			value:   intVal(245),
			wantInt: 245,
		},
		{
			name:    "int64 value",
			value:   longVal(123456),
			wantInt: 123456,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := makePayload([]*tpb.Datum{
				makeDatum(tpb.Field_GpsHeading, tt.value),
			})

			evt, _, err := dec.DecodePayload(payload)
			if err != nil {
				t.Fatalf("DecodePayload() error = %v", err)
			}

			heading := evt.Fields["heading"]
			if heading.IntVal == nil {
				t.Fatal("heading.IntVal is nil")
			}
			if *heading.IntVal != tt.wantInt {
				t.Errorf("heading = %d, want %d", *heading.IntVal, tt.wantInt)
			}
		})
	}
}

func TestDecoder_DecodePayload_Location(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	const (
		wantLat = 33.0903
		wantLng = -96.8237
	)

	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_Location, locationVal(wantLat, wantLng)),
	})

	evt, fieldErrs, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}
	if len(fieldErrs) != 0 {
		t.Errorf("unexpected field errors: %v", fieldErrs)
	}

	loc := evt.Fields["location"]
	if loc.LocationVal == nil {
		t.Fatal("location.LocationVal is nil")
	}
	if loc.LocationVal.Latitude != wantLat {
		t.Errorf("lat = %f, want %f", loc.LocationVal.Latitude, wantLat)
	}
	if loc.LocationVal.Longitude != wantLng {
		t.Errorf("lng = %f, want %f", loc.LocationVal.Longitude, wantLng)
	}
}

func TestDecoder_DecodePayload_OriginDestLocation(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_OriginLocation, locationVal(33.0, -96.0)),
		makeDatum(tpb.Field_DestinationLocation, locationVal(32.0, -97.0)),
	})

	evt, _, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}

	origin := evt.Fields["originLocation"]
	if origin.LocationVal == nil || origin.LocationVal.Latitude != 33.0 {
		t.Errorf("origin = %v, want lat=33.0", origin)
	}

	dest := evt.Fields["destinationLocation"]
	if dest.LocationVal == nil || dest.LocationVal.Latitude != 32.0 {
		t.Errorf("dest = %v, want lat=32.0", dest)
	}
}

func TestDecoder_DecodePayload_ShiftState(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	tests := []struct {
		name       string
		value      *tpb.Value
		wantString string
	}{
		{
			name:       "drive",
			value:      shiftStateVal(tpb.ShiftState_ShiftStateD),
			wantString: "D",
		},
		{
			name:       "park",
			value:      shiftStateVal(tpb.ShiftState_ShiftStateP),
			wantString: "P",
		},
		{
			name:       "reverse",
			value:      shiftStateVal(tpb.ShiftState_ShiftStateR),
			wantString: "R",
		},
		{
			name:       "neutral",
			value:      shiftStateVal(tpb.ShiftState_ShiftStateN),
			wantString: "N",
		},
		{
			name:       "string fallback",
			value:      stringVal("D"),
			wantString: "D",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := makePayload([]*tpb.Datum{
				makeDatum(tpb.Field_Gear, tt.value),
			})

			evt, _, err := dec.DecodePayload(payload)
			if err != nil {
				t.Fatalf("DecodePayload() error = %v", err)
			}

			gear := evt.Fields["gear"]
			if gear.StringVal == nil || *gear.StringVal != tt.wantString {
				t.Errorf("gear = %v, want %q", gear, tt.wantString)
			}
		})
	}
}

func TestDecoder_DecodePayload_ChargeState(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	tests := []struct {
		name       string
		value      *tpb.Value
		wantString string
	}{
		{
			name:       "detailed charging",
			value:      detailedChargeStateVal(tpb.DetailedChargeStateValue_DetailedChargeStateCharging),
			wantString: "Charging",
		},
		{
			name:       "detailed complete",
			value:      detailedChargeStateVal(tpb.DetailedChargeStateValue_DetailedChargeStateComplete),
			wantString: "Complete",
		},
		{
			name:       "deprecated charging state",
			value:      chargingVal(tpb.ChargingState_ChargeStateCharging),
			wantString: "Charging",
		},
		{
			name:       "string fallback",
			value:      stringVal("Charging"),
			wantString: "Charging",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := makePayload([]*tpb.Datum{
				makeDatum(tpb.Field_DetailedChargeState, tt.value),
			})

			evt, _, err := dec.DecodePayload(payload)
			if err != nil {
				t.Fatalf("DecodePayload() error = %v", err)
			}

			cs := evt.Fields["chargeState"]
			if cs.StringVal == nil || *cs.StringVal != tt.wantString {
				t.Errorf("chargeState = %v, want %q", cs, tt.wantString)
			}
		})
	}
}

func TestDecoder_DecodePayload_CarType(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_CarType, carTypeVal(tpb.CarTypeValue_CarTypeModelY)),
	})

	evt, _, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}

	ct := evt.Fields["carType"]
	if ct.StringVal == nil || *ct.StringVal != "Model Y" {
		t.Errorf("carType = %v, want 'Model Y'", ct)
	}
}

func TestDecoder_DecodePayload_BooleanField(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_Locked, boolVal(true)),
	})

	evt, _, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}

	locked := evt.Fields["locked"]
	if locked.BoolVal == nil || !*locked.BoolVal {
		t.Errorf("locked = %v, want true", locked)
	}
}

func TestDecoder_DecodePayload_StringFields(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_VehicleName, stringVal("My Model Y")),
		makeDatum(tpb.Field_Version, stringVal("2024.38.1")),
		makeDatum(tpb.Field_DestinationName, stringVal("Thompson Hotel")),
		makeDatum(tpb.Field_RouteLine, stringVal("_p~iF~ps|U")),
	})

	evt, _, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}

	wantStrings := map[string]string{
		"vehicleName":     "My Model Y",
		"version":         "2024.38.1",
		"destinationName": "Thompson Hotel",
		"routeLine":       "_p~iF~ps|U",
	}

	for field, want := range wantStrings {
		got, ok := evt.Fields[field]
		if !ok {
			t.Errorf("missing field %q", field)
			continue
		}
		if got.StringVal == nil || *got.StringVal != want {
			t.Errorf("field %q = %v, want %q", field, got, want)
		}
	}
}

func TestDecoder_DecodePayload_InvalidDatum(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_VehicleSpeed, invalidVal()),
		makeDatum(tpb.Field_Odometer, stringVal("12345.6")),
	})

	evt, fieldErrs, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}

	// Invalid datums are no longer field errors — they are included in
	// the event with Invalid=true so downstream consumers can clear
	// stale frontend state.
	if len(fieldErrs) != 0 {
		t.Fatalf("expected 0 field errors, got %d: %v", len(fieldErrs), fieldErrs)
	}

	// The valid odometer field should still be decoded.
	odo, ok := evt.Fields["odometer"]
	if !ok {
		t.Fatal("missing odometer field")
	}
	if odo.FloatVal == nil || *odo.FloatVal != 12345.6 {
		t.Errorf("odometer = %v, want 12345.6", odo)
	}

	// Speed should be present with Invalid=true.
	spd, ok := evt.Fields["speed"]
	if !ok {
		t.Fatal("speed field should be present for invalid datum")
	}
	if !spd.Invalid {
		t.Error("speed.Invalid should be true")
	}
}

func TestDecoder_DecodePayload_NilDatum(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		nil,
		makeDatum(tpb.Field_VehicleSpeed, stringVal("55.0")),
	})

	evt, fieldErrs, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}

	if len(fieldErrs) != 1 {
		t.Fatalf("expected 1 field error for nil datum, got %d", len(fieldErrs))
	}
	if !errors.Is(fieldErrs[0].Err, ErrNilDatum) {
		t.Errorf("field error = %v, want ErrNilDatum", fieldErrs[0].Err)
	}

	// Valid datum should still be decoded.
	if _, ok := evt.Fields["speed"]; !ok {
		t.Error("speed field should be present")
	}
}

func TestDecoder_DecodePayload_NilValue(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		{Key: tpb.Field_VehicleSpeed, Value: nil},
	})

	_, fieldErrs, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}

	if len(fieldErrs) != 1 {
		t.Fatalf("expected 1 field error for nil value, got %d", len(fieldErrs))
	}
	if !errors.Is(fieldErrs[0].Err, ErrNilValue) {
		t.Errorf("field error = %v, want ErrNilValue", fieldErrs[0].Err)
	}
}

func TestDecoder_DecodePayload_UntrackedFieldsSkipped(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		// SeatHeaterRearLeft is not in our fieldMap — should be silently skipped.
		makeDatum(tpb.Field_SeatHeaterRearLeft, stringVal("3")),
		makeDatum(tpb.Field_VehicleSpeed, stringVal("70")),
	})

	evt, fieldErrs, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}
	if len(fieldErrs) != 0 {
		t.Errorf("unexpected field errors: %v", fieldErrs)
	}

	if len(evt.Fields) != 1 {
		t.Errorf("expected 1 decoded field, got %d: %v", len(evt.Fields), evt.Fields)
	}
	if _, ok := evt.Fields["speed"]; !ok {
		t.Error("expected speed field to be present")
	}
}

func TestDecoder_DecodePayload_FullPayload(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_VehicleSpeed, stringVal("65.2")),
		makeDatum(tpb.Field_Location, locationVal(33.0903, -96.8237)),
		makeDatum(tpb.Field_GpsHeading, stringVal("245")),
		makeDatum(tpb.Field_Gear, shiftStateVal(tpb.ShiftState_ShiftStateD)),
		makeDatum(tpb.Field_Soc, stringVal("78.5")),
		makeDatum(tpb.Field_EstBatteryRange, stringVal("210.3")),
		makeDatum(tpb.Field_Odometer, doubleVal(12345.6)),
		makeDatum(tpb.Field_InsideTemp, stringVal("22.1")),
		makeDatum(tpb.Field_OutsideTemp, stringVal("28.4")),
	})

	evt, fieldErrs, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}
	if len(fieldErrs) != 0 {
		t.Errorf("unexpected field errors: %v", fieldErrs)
	}

	if evt.VIN != testVIN {
		t.Errorf("VIN = %q, want %q", evt.VIN, testVIN)
	}
	if len(evt.Fields) != 9 {
		t.Errorf("expected 9 fields, got %d", len(evt.Fields))
	}

	// Spot-check a few fields.
	assertFloat(t, evt.Fields, "speed", 65.2)
	assertFloat(t, evt.Fields, "soc", 78.5)
	assertFloat(t, evt.Fields, "odometer", 12345.6)
	assertString(t, evt.Fields, "gear", "D")
	assertLocation(t, evt.Fields, "location", 33.0903, -96.8237)

	// Temperatures are converted from Celsius to Fahrenheit.
	// 22.1C -> 71.78F, 28.4C -> 83.12F
	assertFloatApprox(t, evt.Fields, "insideTemp", 71.78)
	assertFloatApprox(t, evt.Fields, "outsideTemp", 83.12)
}

func TestDecoder_DecodePayload_EmptyString(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_DestinationName, stringVal("")),
	})

	evt, fieldErrs, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}
	if len(fieldErrs) != 0 {
		t.Errorf("unexpected field errors: %v", fieldErrs)
	}

	dn := evt.Fields["destinationName"]
	if dn.StringVal == nil || *dn.StringVal != "" {
		t.Errorf("destinationName = %v, want empty string", dn)
	}
}

func TestDecoder_DecodePayload_LocationUnexpectedType(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	// Send an int value for a location field — should produce a field error.
	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_Location, intVal(42)),
	})

	_, fieldErrs, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}
	if len(fieldErrs) != 1 {
		t.Fatalf("expected 1 field error, got %d", len(fieldErrs))
	}
	if !errors.Is(fieldErrs[0].Err, ErrUnexpectedValueType) {
		t.Errorf("error = %v, want ErrUnexpectedValueType", fieldErrs[0].Err)
	}
}

func TestDecoder_DecodePayload_GearUnexpectedType(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_Gear, intVal(5)),
	})

	_, fieldErrs, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}
	if len(fieldErrs) != 1 {
		t.Fatalf("expected 1 field error, got %d", len(fieldErrs))
	}
	if !errors.Is(fieldErrs[0].Err, ErrUnexpectedValueType) {
		t.Errorf("error = %v, want ErrUnexpectedValueType", fieldErrs[0].Err)
	}
}

func TestFieldError_ErrorString(t *testing.T) {
	t.Parallel()
	fe := FieldError{
		Field: FieldSpeed,
		Key:   tpb.Field_VehicleSpeed,
		Err:   ErrInvalidValue,
	}

	got := fe.Error()
	if got == "" {
		t.Error("FieldError.Error() returned empty string")
	}

	if !errors.Is(fe, ErrInvalidValue) {
		t.Error("FieldError should unwrap to ErrInvalidValue")
	}
}

func TestIsTrackedField(t *testing.T) {
	t.Parallel()
	tests := []struct {
		field tpb.Field
		want  bool
	}{
		{tpb.Field_VehicleSpeed, true},
		{tpb.Field_Location, true},
		{tpb.Field_Gear, true},
		{tpb.Field_SeatHeaterLeft, true},
		{tpb.Field_SeatHeaterRearLeft, false},
		{tpb.Field_Unknown, false},
	}

	for _, tt := range tests {
		t.Run(tt.field.String(), func(t *testing.T) {
			t.Parallel()
			if got := IsTrackedField(tt.field); got != tt.want {
				t.Errorf("IsTrackedField(%v) = %v, want %v", tt.field, got, tt.want)
			}
		})
	}
}

func TestInternalFieldName(t *testing.T) {
	t.Parallel()

	name, ok := InternalFieldName(tpb.Field_VehicleSpeed)
	if !ok || name != FieldSpeed {
		t.Errorf("InternalFieldName(VehicleSpeed) = (%q, %v), want (%q, true)", name, ok, FieldSpeed)
	}

	name, ok = InternalFieldName(tpb.Field_Unknown)
	if ok {
		t.Errorf("InternalFieldName(Unknown) = (%q, %v), want (\"\", false)", name, ok)
	}
}

// assertFloat checks that a field in the event has the expected float64 value.
func assertFloat(t *testing.T, fields map[string]events.TelemetryValue, name string, want float64) {
	t.Helper()
	v, ok := fields[name]
	if !ok {
		t.Errorf("missing field %q", name)
		return
	}
	if v.FloatVal == nil {
		t.Errorf("field %q: FloatVal is nil", name)
		return
	}
	if *v.FloatVal != want {
		t.Errorf("field %q = %f, want %f", name, *v.FloatVal, want)
	}
}

// assertString checks that a field in the event has the expected string value.
func assertString(t *testing.T, fields map[string]events.TelemetryValue, name string, want string) {
	t.Helper()
	v, ok := fields[name]
	if !ok {
		t.Errorf("missing field %q", name)
		return
	}
	if v.StringVal == nil {
		t.Errorf("field %q: StringVal is nil", name)
		return
	}
	if *v.StringVal != want {
		t.Errorf("field %q = %q, want %q", name, *v.StringVal, want)
	}
}

// assertLocation checks that a field in the event has the expected lat/lng.
func assertLocation(t *testing.T, fields map[string]events.TelemetryValue, name string, wantLat, wantLng float64) {
	t.Helper()
	v, ok := fields[name]
	if !ok {
		t.Errorf("missing field %q", name)
		return
	}
	if v.LocationVal == nil {
		t.Errorf("field %q: LocationVal is nil", name)
		return
	}
	if v.LocationVal.Latitude != wantLat {
		t.Errorf("field %q lat = %f, want %f", name, v.LocationVal.Latitude, wantLat)
	}
	if v.LocationVal.Longitude != wantLng {
		t.Errorf("field %q lng = %f, want %f", name, v.LocationVal.Longitude, wantLng)
	}
}

// assertFloatApprox checks that a float field is within 0.01 of the
// expected value. Used for converted values where floating-point rounding
// may produce slight differences.
func assertFloatApprox(t *testing.T, fields map[string]events.TelemetryValue, name string, want float64) {
	t.Helper()
	const epsilon = 0.01
	v, ok := fields[name]
	if !ok {
		t.Errorf("missing field %q", name)
		return
	}
	if v.FloatVal == nil {
		t.Errorf("field %q: FloatVal is nil", name)
		return
	}
	diff := *v.FloatVal - want
	if diff < 0 {
		diff = -diff
	}
	if diff > epsilon {
		t.Errorf("field %q = %f, want %f (within %f)", name, *v.FloatVal, want, epsilon)
	}
}

func TestDecoder_DecodePayload_TemperatureConversion(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	tests := []struct {
		name      string
		field     tpb.Field
		fieldName string
		celsius   string
		wantF     float64
	}{
		{
			name:      "inside temp string",
			field:     tpb.Field_InsideTemp,
			fieldName: "insideTemp",
			celsius:   "20.0",
			wantF:     68.0,
		},
		{
			name:      "outside temp string",
			field:     tpb.Field_OutsideTemp,
			fieldName: "outsideTemp",
			celsius:   "0",
			wantF:     32.0,
		},
		{
			name:      "driver temp setting",
			field:     tpb.Field_HvacLeftTemperatureRequest,
			fieldName: "driverTempSetting",
			celsius:   "22.0",
			wantF:     71.6,
		},
		{
			name:      "passenger temp setting",
			field:     tpb.Field_HvacRightTemperatureRequest,
			fieldName: "passengerTempSetting",
			celsius:   "25.0",
			wantF:     77.0,
		},
		{
			name:      "negative celsius",
			field:     tpb.Field_OutsideTemp,
			fieldName: "outsideTemp",
			celsius:   "-10.0",
			wantF:     14.0,
		},
		{
			name:      "100 celsius",
			field:     tpb.Field_InsideTemp,
			fieldName: "insideTemp",
			celsius:   "100",
			wantF:     212.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := makePayload([]*tpb.Datum{
				makeDatum(tt.field, stringVal(tt.celsius)),
			})

			evt, fieldErrs, err := dec.DecodePayload(payload)
			if err != nil {
				t.Fatalf("DecodePayload() error = %v", err)
			}
			if len(fieldErrs) != 0 {
				t.Errorf("unexpected field errors: %v", fieldErrs)
			}

			assertFloatApprox(t, evt.Fields, tt.fieldName, tt.wantF)
		})
	}
}

func TestDecoder_DecodePayload_TemperatureConversionFloat(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	// Test that float/double values also get converted.
	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_InsideTemp, floatVal(20.0)),
		makeDatum(tpb.Field_OutsideTemp, doubleVal(25.5)),
	})

	evt, fieldErrs, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}
	if len(fieldErrs) != 0 {
		t.Errorf("unexpected field errors: %v", fieldErrs)
	}

	assertFloatApprox(t, evt.Fields, "insideTemp", 68.0)
	assertFloatApprox(t, evt.Fields, "outsideTemp", 77.9)
}

func TestDecoder_DecodePayload_ClimateEnumFields(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	tests := []struct {
		name       string
		field      tpb.Field
		value      *tpb.Value
		fieldName  string
		wantString string
	}{
		{
			name:       "defrost mode off",
			field:      tpb.Field_DefrostMode,
			value:      defrostModeVal(tpb.DefrostModeState_DefrostModeStateOff),
			fieldName:  "defrostMode",
			wantString: "Off",
		},
		{
			name:       "defrost mode max",
			field:      tpb.Field_DefrostMode,
			value:      defrostModeVal(tpb.DefrostModeState_DefrostModeStateMax),
			fieldName:  "defrostMode",
			wantString: "Max",
		},
		{
			name:       "climate keeper dog",
			field:      tpb.Field_ClimateKeeperMode,
			value:      climateKeeperModeVal(tpb.ClimateKeeperModeState_ClimateKeeperModeStateDog),
			fieldName:  "climateKeeperMode",
			wantString: "Dog",
		},
		{
			name:       "hvac power on",
			field:      tpb.Field_HvacPower,
			value:      hvacPowerVal(tpb.HvacPowerState_HvacPowerStateOn),
			fieldName:  "hvacPower",
			wantString: "On",
		},
		{
			name:       "hvac power precondition",
			field:      tpb.Field_HvacPower,
			value:      hvacPowerVal(tpb.HvacPowerState_HvacPowerStatePrecondition),
			fieldName:  "hvacPower",
			wantString: "Precondition",
		},
		{
			name:       "defrost string fallback",
			field:      tpb.Field_DefrostMode,
			value:      stringVal("Max"),
			fieldName:  "defrostMode",
			wantString: "Max",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := makePayload([]*tpb.Datum{
				makeDatum(tt.field, tt.value),
			})

			evt, fieldErrs, err := dec.DecodePayload(payload)
			if err != nil {
				t.Fatalf("DecodePayload() error = %v", err)
			}
			if len(fieldErrs) != 0 {
				t.Errorf("unexpected field errors: %v", fieldErrs)
			}

			assertString(t, evt.Fields, tt.fieldName, tt.wantString)
		})
	}
}

func TestDecoder_DecodePayload_ClimateNumericFields(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	payload := makePayload([]*tpb.Datum{
		makeDatum(tpb.Field_HvacFanSpeed, stringVal("5")),
		makeDatum(tpb.Field_SeatHeaterLeft, stringVal("3")),
		makeDatum(tpb.Field_SeatHeaterRight, stringVal("0")),
	})

	evt, fieldErrs, err := dec.DecodePayload(payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}
	if len(fieldErrs) != 0 {
		t.Errorf("unexpected field errors: %v", fieldErrs)
	}

	assertFloat(t, evt.Fields, "hvacFanSpeed", 5.0)
	assertFloat(t, evt.Fields, "seatHeaterLeft", 3.0)
	assertFloat(t, evt.Fields, "seatHeaterRight", 0.0)
}

func TestCelsiusToFahrenheit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		celsius float64
		wantF   float64
	}{
		{"freezing point", 0, 32},
		{"boiling point", 100, 212},
		{"body temp", 37, 98.6},
		{"negative", -40, -40},
		{"room temp", 20, 68},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := celsiusToFahrenheit(tt.celsius)
			diff := got - tt.wantF
			if diff < 0 {
				diff = -diff
			}
			if diff > 0.01 {
				t.Errorf("celsiusToFahrenheit(%f) = %f, want %f", tt.celsius, got, tt.wantF)
			}
		})
	}
}

// defrostModeVal creates a Value with a defrost_mode_value.
func defrostModeVal(dm tpb.DefrostModeState) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_DefrostModeValue{DefrostModeValue: dm}}
}

// climateKeeperModeVal creates a Value with a climate_keeper_mode_value.
func climateKeeperModeVal(ck tpb.ClimateKeeperModeState) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_ClimateKeeperModeValue{ClimateKeeperModeValue: ck}}
}

// hvacPowerVal creates a Value with an hvac_power_value.
func hvacPowerVal(hp tpb.HvacPowerState) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_HvacPowerValue{HvacPowerValue: hp}}
}
