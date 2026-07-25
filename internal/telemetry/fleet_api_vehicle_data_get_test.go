package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// realisticVehicleDataBody is a trimmed-but-realistic Fleet API vehicle_data
// response. It deliberately includes drive_state GPS (P1) to prove the decoder
// simply ignores it — we never read or log it.
const realisticVehicleDataBody = `{
  "response": {
    "vin": "7SAYGDET7TA613795",
    "drive_state": {"latitude": 37.7749, "longitude": -122.4194, "speed": null},
    "vehicle_state": {"locked": true, "ft": 0, "rt": 255, "odometer": 12345.678, "sentry_mode": false},
    "climate_state": {"is_climate_on": true, "inside_temp": 21.5, "outside_temp": 15.0},
    "charge_state": {"charging_state": "Charging", "charge_port_door_open": true, "battery_level": 72}
  }
}`

func TestFleetAPIClient_GetVehicleData(t *testing.T) {
	t.Parallel()

	const (
		vin   = "7SAYGDET7TA613795"
		token = "tesla-oauth-token-abc" //nolint:gosec // test fixture, not a real credential
	)

	tests := []struct {
		name        string
		token       string
		vin         string
		status      int
		body        string
		wantErr     bool
		wantReached bool
		check       func(t *testing.T, d *VehicleData)
	}{
		{
			name:        "realistic payload decodes control subset",
			token:       token,
			vin:         vin,
			status:      http.StatusOK,
			body:        realisticVehicleDataBody,
			wantReached: true,
			check: func(t *testing.T, d *VehicleData) {
				if d.VehicleState == nil || d.VehicleState.Locked == nil || !*d.VehicleState.Locked {
					t.Errorf("locked not decoded true: %+v", d.VehicleState)
				}
				if d.VehicleState.RearTrunk == nil || *d.VehicleState.RearTrunk != 255 {
					t.Errorf("rt not decoded")
				}
				if d.VehicleState.Odometer == nil || *d.VehicleState.Odometer != 12345.678 {
					t.Errorf("odometer not decoded")
				}
				if d.ClimateState == nil || d.ClimateState.IsClimateOn == nil || !*d.ClimateState.IsClimateOn {
					t.Errorf("is_climate_on not decoded")
				}
				if d.ChargeState == nil || d.ChargeState.ChargingState == nil || *d.ChargeState.ChargingState != "Charging" {
					t.Errorf("charging_state not decoded")
				}
				if d.ChargeState.BatteryLevel == nil || *d.ChargeState.BatteryLevel != 72 {
					t.Errorf("battery_level not decoded")
				}
			},
		},
		{
			// A sleeping/offline car returns 408 — doWithRetry does NOT retry it
			// (not in isRetryable), so we get one non-2xx error, no wake attempt.
			name:        "asleep returns 408 as error (no wake)",
			token:       token,
			vin:         vin,
			status:      http.StatusRequestTimeout,
			body:        `{"error": "vehicle unavailable"}`,
			wantErr:     true,
			wantReached: true,
		},
		{name: "empty token rejected before request", token: "", vin: vin, status: http.StatusOK, wantErr: true, wantReached: false},
		{name: "invalid VIN rejected before request", token: token, vin: "short", status: http.StatusOK, wantErr: true, wantReached: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var reached bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				if r.Method != http.MethodGet {
					t.Errorf("method = %s, want GET", r.Method)
				}
				wantPath := "/api/1/vehicles/" + vin + "/vehicle_data"
				if r.URL.Path != wantPath {
					t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer "+token {
					t.Errorf("authorization = %q, want Bearer token", got)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			client := newTestFleetClient(srv.URL)
			got, err := client.GetVehicleData(context.Background(), tt.token, tt.vin)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if reached != tt.wantReached {
				t.Errorf("server reached = %v, want %v", reached, tt.wantReached)
			}
			if tt.wantErr {
				return
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

// TestFleetAPIClient_GetVehicleData_RedactsVINInError ensures the full VIN never
// appears in a returned error (only the last-4 redaction) — the vehicle_data
// path carries P1 and must not leak identifiers into logs.
func TestFleetAPIClient_GetVehicleData_RedactsVINInError(t *testing.T) {
	t.Parallel()

	const vin = "7SAYGDET7TA613795"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newTestFleetClient(srv.URL).GetVehicleData(context.Background(), "tok", vin)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), vin) {
		t.Errorf("error leaks full VIN: %v", err)
	}
}

// TestVehicleDataToFields verifies the REST subset maps onto the same internal
// field names the protobuf decoder emits, with the same unit conventions.
func TestVehicleDataToFields(t *testing.T) {
	t.Parallel()

	locked := true
	ft := 0
	rt := 3
	odo := 12345.678
	climateOn := false
	inside := 20.0 // Celsius -> 68F
	outside := 0.0 // Celsius -> 32F
	charging := "Complete"
	portOpen := true
	battery := 55

	data := &VehicleData{
		VehicleState: &VehicleDataVehicleState{Locked: &locked, FrontTrunk: &ft, RearTrunk: &rt, Odometer: &odo},
		ClimateState: &VehicleDataClimateState{IsClimateOn: &climateOn, InsideTemp: &inside, OutsideTemp: &outside},
		ChargeState:  &VehicleDataChargeState{ChargingState: &charging, ChargePortDoorOpen: &portOpen, BatteryLevel: &battery},
	}

	fields := vehicleDataToFields(data)

	// locked
	if v, ok := fields[string(FieldLocked)]; !ok || v.BoolVal == nil || !*v.BoolVal {
		t.Errorf("FieldLocked = %+v, want true", fields[string(FieldLocked)])
	}
	// doorState bitmask: ft=0 (frunk closed), rt=3 (trunk open) -> only DoorTrunk bit.
	ds, ok := fields[string(FieldDoorState)]
	if !ok || ds.IntVal == nil {
		t.Fatalf("FieldDoorState missing")
	}
	if events.DoorOpen(*ds.IntVal, events.DoorFrunk) {
		t.Errorf("frunk should be closed (ft=0)")
	}
	if !events.DoorOpen(*ds.IntVal, events.DoorTrunk) {
		t.Errorf("trunk should be open (rt=3)")
	}
	// odometer passes through as float.
	if v := fields[string(FieldOdometer)]; v.FloatVal == nil || *v.FloatVal != 12345.678 {
		t.Errorf("FieldOdometer = %+v", v)
	}
	// climate off -> hvacPower "Off".
	if v := fields[string(FieldHvacPower)]; v.StringVal == nil || *v.StringVal != "Off" {
		t.Errorf("FieldHvacPower = %+v, want Off", v)
	}
	// temps: Celsius -> Fahrenheit.
	if v := fields[string(FieldInsideTemp)]; v.FloatVal == nil || *v.FloatVal != 68.0 {
		t.Errorf("FieldInsideTemp = %+v, want 68F", v)
	}
	if v := fields[string(FieldOutsideTemp)]; v.FloatVal == nil || *v.FloatVal != 32.0 {
		t.Errorf("FieldOutsideTemp = %+v, want 32F", v)
	}
	// charge fields.
	if v := fields[string(FieldChargeState)]; v.StringVal == nil || *v.StringVal != "Complete" {
		t.Errorf("FieldChargeState = %+v", v)
	}
	if v := fields[string(FieldChargePortDoorOpen)]; v.BoolVal == nil || !*v.BoolVal {
		t.Errorf("FieldChargePortDoorOpen = %+v", v)
	}
	// Charge % is emitted under FieldSOC ("soc") — the name that survives the
	// owner mask (translates to wire "chargeLevel"); NOT FieldBatteryLevel.
	if v := fields[string(FieldSOC)]; v.FloatVal == nil || *v.FloatVal != 55 {
		t.Errorf("FieldSOC = %+v", v)
	}
	if _, bad := fields[string(FieldBatteryLevel)]; bad {
		t.Errorf("charge %% emitted under FieldBatteryLevel — dropped by the owner mask")
	}
}

// TestVehicleDataToFields_PartialAndEmpty verifies absent sub-objects/fields are
// skipped (nil-safe) rather than written as misleading zero values.
func TestVehicleDataToFields_PartialAndEmpty(t *testing.T) {
	t.Parallel()

	if got := vehicleDataToFields(&VehicleData{}); len(got) != 0 {
		t.Errorf("empty VehicleData produced %d fields, want 0", len(got))
	}
	if got := vehicleDataToFields(nil); len(got) != 0 {
		t.Errorf("nil VehicleData produced %d fields, want 0", len(got))
	}

	// Only a lock value present: exactly one field, no zero-value odometer/temps.
	locked := false
	got := vehicleDataToFields(&VehicleData{VehicleState: &VehicleDataVehicleState{Locked: &locked}})
	if len(got) != 1 {
		t.Fatalf("partial payload produced %d fields, want 1: %+v", len(got), got)
	}
	if _, ok := got[string(FieldOdometer)]; ok {
		t.Errorf("odometer should be absent when not provided")
	}
}
