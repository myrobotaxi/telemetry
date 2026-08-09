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

				// MYR-233: `hasActiveRide` is OPTIONAL in the schema
				// (absence = a server that predates the field), but
				// THIS server always emits it. The seeded vehicle has
				// no ride rows, so it must be present AND false —
				// asserting presence catches an `omitempty` or a
				// §5.2.0 mask allow-list regression that would make a
				// free car read as "availability unknown".
				busy, ok := row["hasActiveRide"]
				if !ok {
					t.Errorf("missing `hasActiveRide` in items[0]; row keys: %v", keysOf(row))
				} else if busy != false {
					t.Errorf("hasActiveRide = %v, want false (vehicle has no rides)", busy)
				}

				// MYR-342: `rideShareEnabled` is OPTIONAL in the schema
				// (absence = a pre-v0.20.0 server, which the contract
				// says MUST read as ENABLED), but THIS server always
				// emits it. The seeded vehicle has no control-state
				// row at all, which is the ordinary state of most cars
				// and the exact case the read's
				// COALESCE(gcs.ride_share_enabled, TRUE) exists for —
				// so it must be present AND true. Asserting presence
				// catches an `omitempty` regression, which would be
				// especially nasty here: an omitted `false` reads as
				// absent, i.e. ENABLED, silently un-pausing a car its
				// owner paused.
				share, ok := row["rideShareEnabled"]
				if !ok {
					t.Errorf("missing `rideShareEnabled` in items[0]; row keys: %v", keysOf(row))
				} else if share != true {
					t.Errorf("rideShareEnabled = %v, want true (no control-state row = enabled)", share)
				}

				// MYR-491: `setupState` is OPTIONAL in the schema
				// (absence = a pre-v0.24.0 server, which MUST be read
				// exactly like null), but THIS server always emits the
				// key. The seeded vehicle has no fleet-config schedule
				// row — the ordinary state — so the honest answer is an
				// explicit null: NO CLAIM, which a consumer must not
				// render as "ready". Asserting null and not merely
				// presence is the guard that matters: a derivation that
				// fabricated a state here would put a "Finish setup"
				// card on every healthy car in the fleet.
				setup, ok := row["setupState"]
				if !ok {
					t.Errorf("missing `setupState` in items[0]; row keys: %v", keysOf(row))
				} else if setup != nil {
					t.Errorf("setupState = %v, want null (no fleet-config schedule row "+
						"means no claim)", setup)
				}

				// MYR-507: `trimLabel` is OPTIONAL in the schema (absence
				// = a pre-v0.31.0 server, read exactly like null), but THIS
				// server always emits the key. The seeded vehicle has no
				// control-state row, so the honest answer is an explicit
				// null — "no trim known", which is NOT the empty string:
				// only null tells a descriptor builder to drop the fragment
				// rather than render a blank one between two separators.
				trim, ok := row["trimLabel"]
				if !ok {
					t.Errorf("missing `trimLabel` in items[0]; row keys: %v", keysOf(row))
				} else if trim != nil {
					t.Errorf("trimLabel = %v, want null (no control-state row means no "+
						"trim has been read)", trim)
				}

				// The identity set MYR-507 exists to complete. `model` and
				// `year` are asserted for VALUE here, not merely presence:
				// they were `required` on this endpoint all along and still
				// shipped as `""` / `0` for every Go-provisioned car,
				// because the provisioning INSERT seeded placeholders no
				// writer ever replaced. Presence was never the failing
				// assertion — content was.
				if got, _ := row["model"].(string); got != "Model 3" {
					t.Errorf("model = %q, want %q — an empty model is the MYR-507 bug, "+
						"and it passed every presence check", got, "Model 3")
				}
				if got, _ := row["year"].(float64); got != 2024 {
					t.Errorf("year = %v, want 2024 — a zero year is the MYR-507 bug", got)
				}

				// Cross-check against the canonical schema so any
				// future field rename / type change surfaces here.
				// The file's root is the LIST ENVELOPE (VehicleListResponse);
				// the per-row object lives under $defs, so the pointer form
				// is what validates a single decoded item.
				validateAgainstSchema(t,
					"docs/contracts/schemas/vehicle-summary.schema.json#/$defs/VehicleSummary",
					marshalRow(t, row))
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
