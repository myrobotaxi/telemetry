package config

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDuration_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr bool
	}{
		{name: "seconds", input: `"5s"`, want: 5 * time.Second},
		{name: "minutes", input: `"2m"`, want: 2 * time.Minute},
		{name: "milliseconds", input: `"500ms"`, want: 500 * time.Millisecond},
		{name: "hours", input: `"1h"`, want: time.Hour},
		{name: "compound", input: `"1m30s"`, want: 90 * time.Second},
		{name: "zero", input: `"0s"`, want: 0},
		{name: "invalid string", input: `"notaduration"`, wantErr: true},
		{name: "number not string", input: `5`, wantErr: true},
		{name: "empty string", input: `""`, wantErr: true},
		{name: "null", input: `null`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d Duration
			err := json.Unmarshal([]byte(tt.input), &d)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Unmarshal(%s) expected error, got nil", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s) unexpected error: %v", tt.input, err)
			}
			if d.Dur() != tt.want {
				t.Errorf("Unmarshal(%s).Dur() = %v, want %v", tt.input, d.Dur(), tt.want)
			}
		})
	}
}

func TestDuration_MarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		dur  time.Duration
		want string
	}{
		{name: "seconds", dur: 5 * time.Second, want: `"5s"`},
		{name: "minutes", dur: 2 * time.Minute, want: `"2m0s"`},
		{name: "compound", dur: 90 * time.Second, want: `"1m30s"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Duration{d: tt.dur}
			got, err := json.Marshal(d)
			if err != nil {
				t.Fatalf("Marshal() unexpected error: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Marshal() = %s, want %s", string(got), tt.want)
			}
		})
	}
}

func TestDuration_RoundTrip(t *testing.T) {
	original := Duration{d: 15 * time.Second}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() unexpected error: %v", err)
	}

	var decoded Duration
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal() unexpected error: %v", err)
	}

	if decoded.Dur() != original.Dur() {
		t.Errorf("round-trip: got %v, want %v", decoded.Dur(), original.Dur())
	}
}
