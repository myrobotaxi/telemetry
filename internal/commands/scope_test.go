package commands

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// makeJWT builds an unsigned JWT-shaped string (header.payload.sig) with the
// given payload claims. Only the payload segment is decoded by ParseScopes,
// so the header and signature are placeholders.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	seg := base64.RawURLEncoding.EncodeToString(payload)
	return "eyJhbGciOiJSUzI1NiJ9." + seg + ".sig"
}

func TestParseScopes(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		wantKnown  bool
		wantHasCmd bool
		wantHasChg bool
	}{
		{
			name:       "scp array",
			token:      makeJWTForTest(t, map[string]any{"scp": []string{"openid", "vehicle_device_data", "vehicle_cmds"}}),
			wantKnown:  true,
			wantHasCmd: true,
			wantHasChg: false,
		},
		{
			name:       "scp array with both command scopes",
			token:      makeJWTForTest(t, map[string]any{"scp": []string{"vehicle_cmds", "vehicle_charging_cmds"}}),
			wantKnown:  true,
			wantHasCmd: true,
			wantHasChg: true,
		},
		{
			name:       "space-delimited scope string",
			token:      makeJWTForTest(t, map[string]any{"scope": "openid vehicle_charging_cmds"}),
			wantKnown:  true,
			wantHasCmd: false,
			wantHasChg: true,
		},
		{
			name:      "no scope claim -> unknown",
			token:     makeJWTForTest(t, map[string]any{"sub": "abc"}),
			wantKnown: false,
		},
		{
			name:      "not a jwt -> unknown",
			token:     "not-a-jwt",
			wantKnown: false,
		},
		{
			name:      "malformed payload -> unknown",
			token:     "a.$$$.c",
			wantKnown: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := ParseScopes(tt.token)
			if s.Known() != tt.wantKnown {
				t.Fatalf("Known()=%v want %v", s.Known(), tt.wantKnown)
			}
			if !tt.wantKnown {
				if s.Has(ScopeVehicleCmds) {
					t.Fatalf("unknown set must not report Has")
				}
				return
			}
			if s.Has(ScopeVehicleCmds) != tt.wantHasCmd {
				t.Fatalf("Has(vehicle_cmds)=%v want %v", s.Has(ScopeVehicleCmds), tt.wantHasCmd)
			}
			if s.Has(ScopeChargingCmds) != tt.wantHasChg {
				t.Fatalf("Has(vehicle_charging_cmds)=%v want %v", s.Has(ScopeChargingCmds), tt.wantHasChg)
			}
		})
	}
}

// makeJWTForTest is a thin wrapper kept separate so the helper name reads
// clearly at the call sites above.
func makeJWTForTest(t *testing.T, claims map[string]any) string {
	t.Helper()
	return makeJWT(t, claims)
}
