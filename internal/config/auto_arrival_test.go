package config

import (
	"errors"
	"os"
	"testing"
)

// The MYR-538 auto-arrival kill-switch (AUTO_ARRIVAL_ENABLED).
//
// Same two properties as its neighbours — ON when unset, and the whole load
// FAILS on anything ParseBool rejects — and the fail-fast half matters more
// here than anywhere else in the file. This is the only switch that gates a
// feature which writes a ride's lifecycle without a person asking for it, so an
// operator throwing it because the detector is misbehaving must not be left
// believing they had.

func TestLoad_AutoArrivalEnabledParsing(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		value   string
		want    bool
		wantErr bool
	}{
		{name: "unset defaults to true", set: false, want: true},
		{name: "false disables", set: true, value: "false", want: false},
		{name: "0 disables", set: true, value: "0", want: false},
		{name: "False disables", set: true, value: "False", want: false},
		{name: "true enables", set: true, value: "true", want: true},
		{name: "1 enables", set: true, value: "1", want: true},
		{name: "TRUE enables", set: true, value: "TRUE", want: true},
		{name: "empty is invalid", set: true, value: "", wantErr: true},
		{name: "no is invalid", set: true, value: "no", wantErr: true},
		{name: "off is invalid", set: true, value: "off", wantErr: true},
		{name: "garbage is invalid", set: true, value: "maybe", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTestConfig(t, dir, nil)
			setRequiredEnv(t)
			if tt.set {
				t.Setenv("AUTO_ARRIVAL_ENABLED", tt.value)
			} else {
				os.Unsetenv("AUTO_ARRIVAL_ENABLED")
			}

			cfg, err := Load(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() expected error for AUTO_ARRIVAL_ENABLED=%q, got nil", tt.value)
				}
				if !errors.Is(err, ErrInvalidValue) {
					t.Errorf("error should wrap ErrInvalidValue, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.AutoArrivalEnabled() != tt.want {
				t.Errorf("AutoArrivalEnabled() = %v, want %v", cfg.AutoArrivalEnabled(), tt.want)
			}
		})
	}
}

// TestLoad_AutoArrivalIsIndependentOfDispatchSwitches pins the separation. The
// three gate one pipeline at three points — whether a scheduled car is sent,
// whether any nav is pushed at all, and whether the server may notice the car
// arrived — and an operator stopping one must never silently stop another.
func TestLoad_AutoArrivalIsIndependentOfDispatchSwitches(t *testing.T) {
	tests := []struct {
		name                            string
		dispatch, reservation, arrival  string
		wantDispatch, wantRes, wantArrv bool
	}{
		{
			name:     "auto-arrival off leaves both dispatch paths running",
			dispatch: "true", reservation: "true", arrival: "false",
			wantDispatch: true, wantRes: true, wantArrv: false,
		},
		{
			name:     "dispatch off leaves auto-arrival watching",
			dispatch: "false", reservation: "true", arrival: "true",
			wantDispatch: false, wantRes: true, wantArrv: true,
		},
		{
			name:     "reservation dispatch off leaves auto-arrival watching",
			dispatch: "true", reservation: "false", arrival: "true",
			wantDispatch: true, wantRes: false, wantArrv: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTestConfig(t, dir, nil)
			setRequiredEnv(t)
			t.Setenv("DISPATCH_ENABLED", tt.dispatch)
			t.Setenv("RESERVATION_DISPATCH_ENABLED", tt.reservation)
			t.Setenv("AUTO_ARRIVAL_ENABLED", tt.arrival)

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.DispatchEnabled() != tt.wantDispatch {
				t.Errorf("DispatchEnabled() = %v, want %v", cfg.DispatchEnabled(), tt.wantDispatch)
			}
			if cfg.ReservationDispatchEnabled() != tt.wantRes {
				t.Errorf("ReservationDispatchEnabled() = %v, want %v", cfg.ReservationDispatchEnabled(), tt.wantRes)
			}
			if cfg.AutoArrivalEnabled() != tt.wantArrv {
				t.Errorf("AutoArrivalEnabled() = %v, want %v", cfg.AutoArrivalEnabled(), tt.wantArrv)
			}
		})
	}
}
