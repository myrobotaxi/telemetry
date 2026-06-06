//go:build contract

// Contract tests for GET /api/vehicles (rest-api.md §7.0).
//
// Scope (MYR-141 Phase 1):
//   - Empty list shape: authenticated user with zero vehicles -> 200 {items: []}.
//   - Single-vehicle owner: items[0] carries every VehicleSummary field
//     declared in §7.0 / vehicle-summary.schema.json.
//   - 401 on missing Authorization header.
//   - 401 on malformed bearer token (wrong shape).
//   - 401 on expired bearer token.
//
// Anchors MYR-132's class of failure (missing route returns 404) and
// MYR-139's class (shape drift between the wire and the schema): if
// the route stops being mounted or a required field disappears, this
// suite fails on every PR.
package contract_test

import (
	"context"
	"net/http"
	"testing"
)

func TestContract_GETVehicles(t *testing.T) {
	ctx := context.Background()

	const (
		ownerID    = "user_owner_list"
		vehicleID  = "veh_list_owner_001"
		vehicleVIN = "5YJ3E1EA1PF000101"
	)

	tests := []struct {
		name       string
		seed       func(t *testing.T, h *seedHelpers)
		token      func(t *testing.T) string
		authHeader string // if non-empty, overrides whatever token() returns
		wantStatus int
		wantCode   string // rest-api.md error.code, empty for 200
		assertBody func(t *testing.T, body []byte)
	}{
		{
			name: "empty list returns 200 with items: []",
			seed: func(t *testing.T, h *seedHelpers) {
				h.seedUser(ctx, t, ownerID)
			},
			token:      func(t *testing.T) string { return mintToken(t, ownerID, nil) },
			wantStatus: http.StatusOK,
			assertBody: func(t *testing.T, body []byte) {
				var resp struct {
					Items []map[string]any `json:"items"`
				}
				decodeJSON(t, body, &resp)
				if resp.Items == nil {
					t.Errorf("items must be a JSON array (not null) even when empty; body: %s", string(body))
				}
				if len(resp.Items) != 0 {
					t.Errorf("expected 0 items, got %d", len(resp.Items))
				}
			},
		},
		{
			name: "owner sees own vehicle with all VehicleSummary fields",
			seed: func(t *testing.T, h *seedHelpers) {
				h.seedUser(ctx, t, ownerID)
				h.seedVehicle(ctx, t, vehicleSeed{
					ID:             vehicleID,
					UserID:         ownerID,
					VIN:            vehicleVIN,
					Name:           "Stumpy",
					Model:          "Model 3",
					Year:           2024,
					Color:          "Midnight Silver Metallic",
					Status:         "parked",
					ChargeLevel:    78,
					EstimatedRange: 245,
				})
			},
			token:      func(t *testing.T) string { return mintToken(t, ownerID, nil) },
			wantStatus: http.StatusOK,
			assertBody: func(t *testing.T, body []byte) {
				var resp struct {
					Items []map[string]any `json:"items"`
				}
				decodeJSON(t, body, &resp)
				if len(resp.Items) != 1 {
					t.Fatalf("expected 1 item, got %d; body: %s", len(resp.Items), string(body))
				}
				row := resp.Items[0]

				// rest-api.md §7.0 declares ten required + one P1
				// `name` field for owners. Assert presence for every
				// required field — a missing key is the MYR-139 class
				// of bug this suite exists to catch.
				required := []string{
					"vehicleId", "model", "year", "color", "vinLast4",
					"status", "chargeLevel", "estimatedRange",
					"lastUpdated", "role",
				}
				for _, k := range required {
					if _, ok := row[k]; !ok {
						t.Errorf("missing required field %q in items[0]; row keys: %v", k, keysOf(row))
					}
				}
				// owner sees `name` per §5.2.0
				if _, ok := row["name"]; !ok {
					t.Errorf("owner-tier row missing P1 field `name`; row keys: %v", keysOf(row))
				}
				// VIN is redacted per data-classification.md §1.5: only
				// the last 4 chars surface on the wire. The full VIN
				// MUST NOT appear in the payload.
				if got, _ := row["vinLast4"].(string); got != "0101" {
					t.Errorf("vinLast4 = %q, want %q", got, "0101")
				}
				if got, _ := row["role"].(string); got != "owner" {
					t.Errorf("role = %q, want %q", got, "owner")
				}

				// Cross-check against the canonical schema so any
				// future field rename / type change surfaces here.
				validateAgainstSchema(t, "docs/contracts/schemas/vehicle-summary.schema.json", marshalRow(t, row))
			},
		},
		{
			name: "missing Authorization header returns 401",
			seed: func(t *testing.T, h *seedHelpers) {
				h.seedUser(ctx, t, ownerID)
			},
			token:      func(t *testing.T) string { return "" },
			wantStatus: http.StatusUnauthorized,
			wantCode:   "auth_failed",
		},
		{
			name: "malformed bearer token returns 401",
			seed: func(t *testing.T, h *seedHelpers) {
				h.seedUser(ctx, t, ownerID)
			},
			token:      func(t *testing.T) string { return "not.a.real.jwt" },
			wantStatus: http.StatusUnauthorized,
			wantCode:   "auth_failed",
		},
		{
			name: "expired bearer token returns 401",
			seed: func(t *testing.T, h *seedHelpers) {
				h.seedUser(ctx, t, ownerID)
			},
			token:      func(t *testing.T) string { return mintExpiredToken(t, ownerID) },
			wantStatus: http.StatusUnauthorized,
			wantCode:   "auth_failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, seeder := setupTestServer(t)
			if tc.seed != nil {
				tc.seed(t, seeder)
			}

			tok := ""
			if tc.token != nil {
				tok = tc.token(t)
			}

			resp := doGET(t, srv, "/api/vehicles", tok)
			body := readBody(t, resp)

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, string(body))
			}
			if tc.wantCode != "" {
				assertErrorCode(t, body, tc.wantCode)
			}
			if tc.assertBody != nil && resp.StatusCode == tc.wantStatus {
				tc.assertBody(t, body)
			}
		})
	}
}

// keysOf returns the sorted-irrelevant keys of m for error messages.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// marshalRow re-encodes a single VehicleSummary row so it can be passed
// to the schema validator. The list-level decode (above) parsed the
// whole envelope; the schema validator wants the row body on its own.
func marshalRow(t *testing.T, row map[string]any) []byte {
	t.Helper()
	out, err := jsonMarshal(row)
	if err != nil {
		t.Fatalf("re-marshal row: %v", err)
	}
	return out
}
