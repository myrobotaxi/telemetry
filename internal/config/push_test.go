package config

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const testP8PEM = "-----BEGIN PRIVATE KEY-----\nMIGHAgEA\n-----END PRIVATE KEY-----\n"

// applyPush runs the push env overrides against a fresh fileConfig.
func applyPush(t *testing.T) (*fileConfig, error) {
	t.Helper()
	fc := &fileConfig{}
	return fc, applyPushEnvOverrides(fc)
}

// TestPushEnabledParsing pins the kill-switch semantics: default on, every
// ParseBool spelling accepted, and a FAIL-FAST on anything else so a typo
// cannot leave an operator believing push is stopped when it is not.
func TestPushEnabledParsing(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		value   string
		want    bool
		wantErr bool
	}{
		{name: "unset defaults to enabled", want: true},
		{name: "false", set: true, value: "false"},
		{name: "0", set: true, value: "0"},
		{name: "FALSE", set: true, value: "FALSE"},
		{name: "true", set: true, value: "true", want: true},
		{name: "1", set: true, value: "1", want: true},
		{name: "off is rejected", set: true, value: "off", wantErr: true},
		{name: "no is rejected", set: true, value: "no", wantErr: true},
		{name: "empty is rejected", set: true, value: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv("PUSH_ENABLED", tt.value)
			}
			fc, err := applyPush(t)

			if tt.wantErr {
				if err == nil {
					t.Fatal("error = nil, want a fail-fast on a non-boolean")
				}
				if !errors.Is(err, ErrInvalidValue) {
					t.Errorf("error = %v, want ErrInvalidValue", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if fc.pushEnabled != tt.want {
				t.Errorf("pushEnabled = %v, want %v", fc.pushEnabled, tt.want)
			}
		})
	}
}

// TestPushAPNsCredentials covers the key/topic/team resolution including the
// keyless default the service must run in before the secrets are set.
func TestPushAPNsCredentials(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantKey    string
		wantKeyID  string
		wantTeam   string
		wantTopic  string
		wantErr    bool
		configured bool
	}{
		{
			name:      "keyless by default",
			wantTeam:  defaultAPNSTeamID,
			wantTopic: defaultAPNSTopic,
		},
		{
			name:       "raw pem",
			env:        map[string]string{"APNS_KEY_P8": testP8PEM, "APNS_KEY_ID": "ABC1234567"},
			wantKey:    testP8PEM,
			wantKeyID:  "ABC1234567",
			wantTeam:   defaultAPNSTeamID,
			wantTopic:  defaultAPNSTopic,
			configured: true,
		},
		{
			name: "base64 pem",
			env: map[string]string{
				"APNS_KEY_P8_B64": base64.StdEncoding.EncodeToString([]byte(testP8PEM)),
				"APNS_KEY_ID":     "ABC1234567",
			},
			wantKey:    testP8PEM,
			wantKeyID:  "ABC1234567",
			wantTeam:   defaultAPNSTeamID,
			wantTopic:  defaultAPNSTopic,
			configured: true,
		},
		{
			name: "raw wins over base64",
			env: map[string]string{
				"APNS_KEY_P8":     testP8PEM,
				"APNS_KEY_P8_B64": base64.StdEncoding.EncodeToString([]byte("other")),
				"APNS_KEY_ID":     "ABC1234567",
			},
			wantKey:    testP8PEM,
			wantKeyID:  "ABC1234567",
			wantTeam:   defaultAPNSTeamID,
			wantTopic:  defaultAPNSTopic,
			configured: true,
		},
		{
			name:      "team and topic overrides",
			env:       map[string]string{"APNS_TEAM_ID": "TEAM123456", "APNS_TOPIC": "app.myrobotaxi.staging"},
			wantTeam:  "TEAM123456",
			wantTopic: "app.myrobotaxi.staging",
		},
		{
			name:      "key without key id is not configured",
			env:       map[string]string{"APNS_KEY_P8": testP8PEM},
			wantKey:   testP8PEM,
			wantTeam:  defaultAPNSTeamID,
			wantTopic: defaultAPNSTopic,
		},
		{
			name:    "undecodable base64 fails fast",
			env:     map[string]string{"APNS_KEY_P8_B64": "not!base64!"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			fc, err := applyPush(t)

			if tt.wantErr {
				if err == nil {
					t.Fatal("error = nil, want a decode failure")
				}
				// The key is P0 secret material — the error must not carry it.
				if strings.Contains(err.Error(), "not!base64!") {
					t.Errorf("error %q echoes the raw env value", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}

			if fc.apnsKeyP8 != tt.wantKey {
				t.Errorf("key configured = %v, want %v", fc.apnsKeyP8 != "", tt.wantKey != "")
			}
			if fc.apnsKeyID != tt.wantKeyID {
				t.Errorf("apnsKeyID = %q, want %q", fc.apnsKeyID, tt.wantKeyID)
			}
			if fc.apnsTeamID != tt.wantTeam {
				t.Errorf("apnsTeamID = %q, want %q", fc.apnsTeamID, tt.wantTeam)
			}
			if fc.apnsTopic != tt.wantTopic {
				t.Errorf("apnsTopic = %q, want %q", fc.apnsTopic, tt.wantTopic)
			}

			pc := PushConfig{KeyP8PEM: fc.apnsKeyP8, KeyID: fc.apnsKeyID}
			if pc.Configured() != tt.configured {
				t.Errorf("Configured() = %v, want %v", pc.Configured(), tt.configured)
			}
		})
	}
}
