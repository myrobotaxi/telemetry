package commands

import (
	"encoding/json"
	"testing"
)

func TestRegistrySeededCommands(t *testing.T) {
	r := NewRegistry()

	// Every command the P11/P10 surface needs, with its expected scope and
	// signer flag. This locks the registry contract that MYR-181-183 and
	// MYR-176 build on.
	want := map[string]struct {
		scope  Scope
		signed bool
	}{
		"door_lock":               {ScopeVehicleCmds, true},
		"door_unlock":             {ScopeVehicleCmds, true},
		"auto_conditioning_start": {ScopeVehicleCmds, true},
		"auto_conditioning_stop":  {ScopeVehicleCmds, true},
		"set_temps":               {ScopeVehicleCmds, true},
		"charge_start":            {ScopeChargingCmds, true},
		"charge_stop":             {ScopeChargingCmds, true},
		"set_charge_limit":        {ScopeChargingCmds, true},
		"actuate_trunk":           {ScopeVehicleCmds, true},
		"remote_start_drive":      {ScopeVehicleCmds, true},
		"honk_horn":               {ScopeVehicleCmds, true},
		"flash_lights":            {ScopeVehicleCmds, true},
		"navigation_gps_request":  {ScopeVehicleCmds, false},
		"navigation_request":      {ScopeVehicleCmds, false},

		// MYR-249 owner-app additions.
		"charge_port_door_open":      {ScopeChargingCmds, true},
		"charge_port_door_close":     {ScopeChargingCmds, true},
		"remote_seat_heater_request": {ScopeVehicleCmds, true},
		"remote_seat_cooler_request": {ScopeVehicleCmds, true},
		"media_toggle_playback":      {ScopeVehicleCmds, true},
		"media_next_track":           {ScopeVehicleCmds, true},
		"media_prev_track":           {ScopeVehicleCmds, true},
		"adjust_volume":              {ScopeVehicleCmds, true},
	}

	for name, w := range want {
		t.Run(name, func(t *testing.T) {
			cmd, ok := r.Lookup(name)
			if !ok {
				t.Fatalf("command %q not registered", name)
			}
			if cmd.Scope != w.scope {
				t.Errorf("scope = %q want %q", cmd.Scope, w.scope)
			}
			if cmd.SignerRequired != w.signed {
				t.Errorf("SignerRequired = %v want %v", cmd.SignerRequired, w.signed)
			}
		})
	}

	if _, ok := r.Lookup("nonexistent_command"); ok {
		t.Fatalf("unknown command should not resolve")
	}
	if got := len(r.Names()); got != len(want) {
		t.Fatalf("Names() count = %d want %d", got, len(want))
	}
}

func TestBuildBodyValidation(t *testing.T) {
	r := NewRegistry()

	tests := []struct {
		name    string
		command string
		params  map[string]any
		wantErr bool
		assert  func(t *testing.T, body []byte)
	}{
		{
			name:    "set_temps driver only mirrors passenger",
			command: "set_temps",
			params:  map[string]any{"driver_temp": 21.0},
			assert: func(t *testing.T, body []byte) {
				var m map[string]any
				mustJSON(t, body, &m)
				if m["driver_temp"] != 21.0 || m["passenger_temp"] != 21.0 {
					t.Fatalf("body = %s", body)
				}
			},
		},
		{
			name:    "set_temps missing driver_temp",
			command: "set_temps",
			params:  map[string]any{},
			wantErr: true,
		},
		{
			name:    "set_temps non-numeric",
			command: "set_temps",
			params:  map[string]any{"driver_temp": "hot"},
			wantErr: true,
		},
		{
			name:    "set_charge_limit valid",
			command: "set_charge_limit",
			params:  map[string]any{"percent": 80.0},
			assert: func(t *testing.T, body []byte) {
				var m map[string]any
				mustJSON(t, body, &m)
				if m["percent"] != 80.0 {
					t.Fatalf("body = %s", body)
				}
			},
		},
		{
			name:    "set_charge_limit out of range",
			command: "set_charge_limit",
			params:  map[string]any{"percent": 20.0},
			wantErr: true,
		},
		{
			name:    "set_charge_limit non-integer",
			command: "set_charge_limit",
			params:  map[string]any{"percent": 80.5},
			wantErr: true,
		},
		{
			name:    "actuate_trunk rear",
			command: "actuate_trunk",
			params:  map[string]any{"which_trunk": "rear"},
		},
		{
			name:    "actuate_trunk invalid value",
			command: "actuate_trunk",
			params:  map[string]any{"which_trunk": "roof"},
			wantErr: true,
		},
		{
			name:    "navigation_gps_request valid",
			command: "navigation_gps_request",
			params:  map[string]any{"lat": 37.7749, "lon": -122.4194},
			assert: func(t *testing.T, body []byte) {
				var m map[string]any
				mustJSON(t, body, &m)
				if m["lat"] != 37.7749 || m["order"] != 1.0 {
					t.Fatalf("body = %s", body)
				}
			},
		},
		{
			name:    "navigation_gps_request out-of-range lat",
			command: "navigation_gps_request",
			params:  map[string]any{"lat": 200.0, "lon": 0.0},
			wantErr: true,
		},
		{
			name:    "navigation_gps_request missing lon",
			command: "navigation_gps_request",
			params:  map[string]any{"lat": 10.0},
			wantErr: true,
		},
		{
			name:    "navigation_request share value",
			command: "navigation_request",
			params:  map[string]any{"value": "1 Market St, San Francisco"},
			assert: func(t *testing.T, body []byte) {
				var m map[string]any
				mustJSON(t, body, &m)
				if m["type"] != "share_ext_content_raw" {
					t.Fatalf("body = %s", body)
				}
			},
		},
		{
			name:    "navigation_request missing value",
			command: "navigation_request",
			params:  map[string]any{},
			wantErr: true,
		},
		{
			name:    "remote_seat_heater_request valid",
			command: "remote_seat_heater_request",
			params:  map[string]any{"seat_position": 1.0, "level": 2.0},
			assert: func(t *testing.T, body []byte) {
				var m map[string]any
				mustJSON(t, body, &m)
				if m["seat_position"] != 1.0 || m["level"] != 2.0 {
					t.Fatalf("body = %s", body)
				}
			},
		},
		{
			name:    "remote_seat_heater_request seat out of range",
			command: "remote_seat_heater_request",
			params:  map[string]any{"seat_position": 9.0, "level": 1.0},
			wantErr: true,
		},
		{
			name:    "remote_seat_heater_request level out of range",
			command: "remote_seat_heater_request",
			params:  map[string]any{"seat_position": 0.0, "level": 4.0},
			wantErr: true,
		},
		{
			name:    "remote_seat_heater_request missing level",
			command: "remote_seat_heater_request",
			params:  map[string]any{"seat_position": 0.0},
			wantErr: true,
		},
		{
			name:    "remote_seat_cooler_request valid",
			command: "remote_seat_cooler_request",
			params:  map[string]any{"seat_position": 2.0, "seat_cooler_level": 3.0},
			assert: func(t *testing.T, body []byte) {
				var m map[string]any
				mustJSON(t, body, &m)
				if m["seat_position"] != 2.0 || m["seat_cooler_level"] != 3.0 {
					t.Fatalf("body = %s", body)
				}
			},
		},
		{
			name:    "remote_seat_cooler_request invalid seat",
			command: "remote_seat_cooler_request",
			params:  map[string]any{"seat_position": 0.0, "seat_cooler_level": 1.0},
			wantErr: true,
		},
		{
			name:    "remote_seat_cooler_request rear seat rejected",
			command: "remote_seat_cooler_request",
			params:  map[string]any{"seat_position": 3.0, "seat_cooler_level": 1.0},
			wantErr: true,
		},
		{
			name:    "remote_seat_cooler_request level below range (0)",
			command: "remote_seat_cooler_request",
			params:  map[string]any{"seat_position": 1.0, "seat_cooler_level": 0.0},
			wantErr: true,
		},
		{
			name:    "remote_seat_cooler_request level boundary min (1=off)",
			command: "remote_seat_cooler_request",
			params:  map[string]any{"seat_position": 1.0, "seat_cooler_level": 1.0},
			assert: func(t *testing.T, body []byte) {
				var m map[string]any
				mustJSON(t, body, &m)
				if m["seat_cooler_level"] != 1.0 {
					t.Fatalf("body = %s", body)
				}
			},
		},
		{
			name:    "remote_seat_cooler_request level boundary max (4=high)",
			command: "remote_seat_cooler_request",
			params:  map[string]any{"seat_position": 2.0, "seat_cooler_level": 4.0},
			assert: func(t *testing.T, body []byte) {
				var m map[string]any
				mustJSON(t, body, &m)
				if m["seat_cooler_level"] != 4.0 {
					t.Fatalf("body = %s", body)
				}
			},
		},
		{
			name:    "remote_seat_cooler_request level above range (5)",
			command: "remote_seat_cooler_request",
			params:  map[string]any{"seat_position": 1.0, "seat_cooler_level": 5.0},
			wantErr: true,
		},
		{
			name:    "adjust_volume valid",
			command: "adjust_volume",
			params:  map[string]any{"volume": 6.5},
			assert: func(t *testing.T, body []byte) {
				var m map[string]any
				mustJSON(t, body, &m)
				if m["volume"] != 6.5 {
					t.Fatalf("body = %s", body)
				}
			},
		},
		{
			name:    "adjust_volume boundary max",
			command: "adjust_volume",
			params:  map[string]any{"volume": 11.0},
		},
		{
			name:    "adjust_volume out of range",
			command: "adjust_volume",
			params:  map[string]any{"volume": 12.0},
			wantErr: true,
		},
		{
			name:    "adjust_volume non-numeric",
			command: "adjust_volume",
			params:  map[string]any{"volume": "loud"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, ok := r.Lookup(tt.command)
			if !ok {
				t.Fatalf("command %q not registered", tt.command)
			}
			if cmd.BuildBody == nil {
				t.Fatalf("command %q has no BuildBody", tt.command)
			}
			body, err := cmd.BuildBody(tt.params)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got body %s", body)
				}
				var cErr *CommandError
				if !asCommandError(err, &cErr) {
					t.Fatalf("error is not *CommandError: %v", err)
				}
				if cErr.Code != "invalid_request" {
					t.Fatalf("code = %q want invalid_request", cErr.Code)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.assert != nil {
				tt.assert(t, body)
			}
		})
	}
}

func mustJSON(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
}
