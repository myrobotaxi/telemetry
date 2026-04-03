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
		{
			name: "minutesToArrival mapped from float",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldMinutesToArrival): {FloatVal: floatPtr(12.7)},
			},
			check: func(t *testing.T, u *VehicleUpdate) {
				if u == nil {
					t.Fatal("expected non-nil update")
				}
				if u.EtaMinutes == nil || *u.EtaMinutes != 13 {
					t.Errorf("EtaMinutes = %v, want 13", ptrVal(u.EtaMinutes))
				}
			},
		},
		{
			name: "milesToArrival mapped from float",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldMilesToArrival): {FloatVal: floatPtr(8.3)},
			},
			check: func(t *testing.T, u *VehicleUpdate) {
				if u == nil {
					t.Fatal("expected non-nil update")
				}
				if u.TripDistRemaining == nil || *u.TripDistRemaining != 8.3 {
					t.Errorf("TripDistRemaining = %v, want 8.3", ptrVal(u.TripDistRemaining))
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

func TestMapTelemetryToUpdate_InvalidNavFields(t *testing.T) {
	tests := []struct {
		name       string
		fields     map[string]events.TelemetryValue
		wantClear  []string
		wantNilUpd bool // true if entire update should be nil
	}{
		{
			name: "invalid destinationName clears DB column",
			fields: map[string]events.TelemetryValue{
				"destinationName": {Invalid: true},
			},
			wantClear: []string{"destinationName"},
		},
		{
			name: "invalid minutesToArrival clears etaMinutes",
			fields: map[string]events.TelemetryValue{
				"minutesToArrival": {Invalid: true},
			},
			wantClear: []string{"etaMinutes"},
		},
		{
			name: "invalid milesToArrival clears tripDistanceRemaining",
			fields: map[string]events.TelemetryValue{
				"milesToArrival": {Invalid: true},
			},
			wantClear: []string{"tripDistanceRemaining"},
		},
		{
			name: "invalid originLocation clears both lat/lng columns",
			fields: map[string]events.TelemetryValue{
				"originLocation": {Invalid: true},
			},
			wantClear: []string{"originLatitude", "originLongitude"},
		},
		{
			name: "invalid destinationLocation clears both lat/lng columns",
			fields: map[string]events.TelemetryValue{
				"destinationLocation": {Invalid: true},
			},
			wantClear: []string{"destinationLatitude", "destinationLongitude"},
		},
		{
			name: "invalid non-nav field is ignored",
			fields: map[string]events.TelemetryValue{
				"speed": {Invalid: true},
			},
			wantNilUpd: true,
		},
		{
			name: "invalid nav field skips applier even if value present",
			fields: map[string]events.TelemetryValue{
				"destinationName": {Invalid: true, StringVal: strPtr("Stale Dest")},
			},
			wantClear: []string{"destinationName"},
		},
		{
			name: "mix of invalid and valid fields",
			fields: map[string]events.TelemetryValue{
				"destinationName": {Invalid: true},
				"speed":           {FloatVal: floatPtr(65.0)},
			},
			wantClear: []string{"destinationName"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := mapTelemetryToUpdate(tt.fields)
			if tt.wantNilUpd {
				if u != nil {
					t.Fatalf("expected nil update, got %+v", u)
				}
				return
			}
			if u == nil {
				t.Fatal("expected non-nil update")
			}

			// Sort both slices for deterministic comparison since map
			// iteration order is random.
			gotClear := make([]string, len(u.ClearFields))
			copy(gotClear, u.ClearFields)
			wantClear := make([]string, len(tt.wantClear))
			copy(wantClear, tt.wantClear)
			sortStrings(gotClear)
			sortStrings(wantClear)

			if len(gotClear) != len(wantClear) {
				t.Fatalf("ClearFields = %v, want %v", gotClear, wantClear)
			}
			for i := range gotClear {
				if gotClear[i] != wantClear[i] {
					t.Errorf("ClearFields[%d] = %q, want %q", i, gotClear[i], wantClear[i])
				}
			}

			// For the mixed case, verify that the valid field was still applied.
			if tt.name == "mix of invalid and valid fields" {
				if u.Speed == nil || *u.Speed != 65 {
					t.Errorf("Speed = %v, want 65 (valid field should still apply)", ptrVal(u.Speed))
				}
				if u.DestinationName != nil {
					t.Errorf("DestinationName = %v, want nil (invalid field should not apply)", ptrVal(u.DestinationName))
				}
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

func sortStrings(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[i] > s[j] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}
