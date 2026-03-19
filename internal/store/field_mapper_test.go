package store

import (
	"testing"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
)

func TestMapTelemetryToUpdate(t *testing.T) {
	speed := 72.4
	heading := 245.0
	soc := 87.0
	estRange := 182.5
	insideTemp := 21.3
	outsideTemp := 15.7
	odometer := 12345.6
	gear := "D"

	tests := []struct {
		name   string
		fields map[string]events.TelemetryValue
		check  func(t *testing.T, u *VehicleUpdate)
	}{
		{
			name:   "nil for empty fields",
			fields: map[string]events.TelemetryValue{},
			check: func(t *testing.T, u *VehicleUpdate) {
				if u != nil {
					t.Errorf("expected nil for empty fields, got %+v", u)
				}
			},
		},
		{
			name: "nil for unrecognized fields only",
			fields: map[string]events.TelemetryValue{
				"unknownField": {StringVal: strPtr("abc")},
			},
			check: func(t *testing.T, u *VehicleUpdate) {
				if u != nil {
					t.Errorf("expected nil for unrecognized fields, got %+v", u)
				}
			},
		},
		{
			name: "speed mapped from float",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldSpeed): {FloatVal: &speed},
			},
			check: func(t *testing.T, u *VehicleUpdate) {
				if u == nil {
					t.Fatal("expected non-nil update")
				}
				if u.Speed == nil || *u.Speed != 72 {
					t.Errorf("Speed = %v, want 72", ptrVal(u.Speed))
				}
			},
		},
		{
			name: "location mapped from LocationVal",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.0975, Longitude: -96.8214}},
			},
			check: func(t *testing.T, u *VehicleUpdate) {
				if u == nil {
					t.Fatal("expected non-nil update")
				}
				if u.Latitude == nil || *u.Latitude != 33.0975 {
					t.Errorf("Latitude = %v, want 33.0975", ptrVal(u.Latitude))
				}
				if u.Longitude == nil || *u.Longitude != -96.8214 {
					t.Errorf("Longitude = %v, want -96.8214", ptrVal(u.Longitude))
				}
			},
		},
		{
			name: "gear mapped from StringVal",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldGear): {StringVal: &gear},
			},
			check: func(t *testing.T, u *VehicleUpdate) {
				if u == nil {
					t.Fatal("expected non-nil update")
				}
				if u.GearPosition == nil || *u.GearPosition != "D" {
					t.Errorf("GearPosition = %v, want D", ptrVal(u.GearPosition))
				}
			},
		},
		{
			name: "all supported fields",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldSpeed):           {FloatVal: &speed},
				string(telemetry.FieldHeading):         {FloatVal: &heading},
				string(telemetry.FieldSOC):             {FloatVal: &soc},
				string(telemetry.FieldEstBatteryRange): {FloatVal: &estRange},
				string(telemetry.FieldInsideTemp):      {FloatVal: &insideTemp},
				string(telemetry.FieldOutsideTemp):     {FloatVal: &outsideTemp},
				string(telemetry.FieldOdometer):        {FloatVal: &odometer},
				string(telemetry.FieldGear):            {StringVal: &gear},
				string(telemetry.FieldLocation):        {LocationVal: &events.Location{Latitude: 33.0975, Longitude: -96.8214}},
			},
			check: func(t *testing.T, u *VehicleUpdate) {
				if u == nil {
					t.Fatal("expected non-nil update")
				}
				if u.Speed == nil || *u.Speed != 72 {
					t.Errorf("Speed = %v, want 72", ptrVal(u.Speed))
				}
				if u.Heading == nil || *u.Heading != 245 {
					t.Errorf("Heading = %v, want 245", ptrVal(u.Heading))
				}
				if u.ChargeLevel == nil || *u.ChargeLevel != 87 {
					t.Errorf("ChargeLevel = %v, want 87", ptrVal(u.ChargeLevel))
				}
				if u.EstimatedRange == nil || *u.EstimatedRange != 183 {
					t.Errorf("EstimatedRange = %v, want 183", ptrVal(u.EstimatedRange))
				}
				if u.InteriorTemp == nil || *u.InteriorTemp != 21 {
					t.Errorf("InteriorTemp = %v, want 21", ptrVal(u.InteriorTemp))
				}
				if u.ExteriorTemp == nil || *u.ExteriorTemp != 16 {
					t.Errorf("ExteriorTemp = %v, want 16", ptrVal(u.ExteriorTemp))
				}
				if u.OdometerMiles == nil || *u.OdometerMiles != 12346 {
					t.Errorf("OdometerMiles = %v, want 12346", ptrVal(u.OdometerMiles))
				}
			},
		},
		{
			name: "nil FloatVal ignored",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldSpeed): {FloatVal: nil},
			},
			check: func(t *testing.T, u *VehicleUpdate) {
				if u != nil {
					t.Errorf("expected nil for nil FloatVal, got %+v", u)
				}
			},
		},
		{
			name: "batteryLevel also maps to ChargeLevel",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldBatteryLevel): {FloatVal: &soc},
			},
			check: func(t *testing.T, u *VehicleUpdate) {
				if u == nil {
					t.Fatal("expected non-nil update")
				}
				if u.ChargeLevel == nil || *u.ChargeLevel != 87 {
					t.Errorf("ChargeLevel = %v, want 87", ptrVal(u.ChargeLevel))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := mapTelemetryToUpdate(tt.fields)
			tt.check(t, u)
		})
	}
}

func TestFloatToIntPtr(t *testing.T) {
	tests := []struct {
		name string
		in   *float64
		want *int
	}{
		{name: "nil", in: nil, want: nil},
		{name: "round down", in: floatPtr(72.4), want: intPtr(72)},
		{name: "round up", in: floatPtr(72.6), want: intPtr(73)},
		{name: "exact", in: floatPtr(65.0), want: intPtr(65)},
		{name: "half rounds up", in: floatPtr(72.5), want: intPtr(73)},
		{name: "negative", in: floatPtr(-3.7), want: intPtr(-4)},
		{name: "zero", in: floatPtr(0.0), want: intPtr(0)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := floatToIntPtr(tt.in)
			if tt.want == nil {
				if got != nil {
					t.Errorf("got %d, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want %d", *tt.want)
			}
			if *got != *tt.want {
				t.Errorf("got %d, want %d", *got, *tt.want)
			}
		})
	}
}

// test helpers

func strPtr(s string) *string    { return &s }
func floatPtr(f float64) *float64 { return &f }
func intPtr(i int) *int           { return &i }

func ptrVal[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
