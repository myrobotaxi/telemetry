//go:build contract

// Contract tests for GET /api/vehicles/{vehicleId}/snapshot
// (rest-api.md §7.1).
//
// Scope (MYR-141 Phase 1):
//   - 200 owner happy path: body validates against the canonical
//     `vehicle-state.schema.json`.
//   - 200 owner happy path: body carries every MYR-24 promoted field
//     (`model`, `year`, `color`, `fsdMilesSinceReset`, `locationName`,
//     `locationAddress`). These were the spec-only fields the snapshot
//     read path started writing in MYR-24 (2026-04-23); if a future
//     refactor stops loading them, this test fails loudly.
//   - 404 not_found for an unknown vehicleId.
//   - 403 vehicle_not_owned for a vehicle owned by someone else.
//   - 401 on missing Authorization header.
package contract_test

import (
	"context"
	"net/http"
	"testing"
)

func TestContract_GETVehicleSnapshot(t *testing.T) {
	ctx := context.Background()

	const (
		ownerID        = "user_owner_snap"
		otherOwnerID   = "user_other_snap"
		vehicleID      = "veh_snap_001"
		otherVehicleID = "veh_snap_other_001"
		ownerVIN       = "5YJ3E1EA1PF000201"
		otherVIN       = "5YJ3E1EA1PF000299"
		unknownVehicle = "veh_does_not_exist"
		locName        = "Home"
		locAddr        = "123 Market St, San Francisco, CA"
		modelStr       = "Model 3"
		colorStr       = "Midnight Silver Metallic"
		modelYear      = 2024
		fsdMilesSince  = 412.7
		chargeLvl      = 78
		rangeMiles     = 245
	)

	tests := []struct {
		name       string
		path       string
		seed       func(t *testing.T, h *seedHelpers)
		token      func(t *testing.T) string
		wantStatus int
		wantCode   string
		assertBody func(t *testing.T, body []byte)
	}{
		{
			name: "owner happy path validates VehicleState schema and carries MYR-24 fields",
			path: "/api/vehicles/" + vehicleID + "/snapshot",
			seed: func(t *testing.T, h *seedHelpers) {
				h.seedUser(ctx, t, ownerID)
				h.seedVehicle(ctx, t, vehicleSeed{
					ID:              vehicleID,
					UserID:          ownerID,
					VIN:             ownerVIN,
					Name:            "Stumpy",
					Model:           modelStr,
					Year:            modelYear,
					Color:           colorStr,
					Status:          "parked",
					ChargeLevel:     chargeLvl,
					EstimatedRange:  rangeMiles,
					LocationName:    locName,
					LocationAddress: locAddr,
					FsdMilesReset:   fsdMilesSince,
				})
			},
			token:      func(t *testing.T) string { return mintToken(t, ownerID, nil) },
			wantStatus: http.StatusOK,
			assertBody: func(t *testing.T, body []byte) {
				// Canonical-shape gate: catches any future change to the
				// snapshot wire format that drifts from the SDK schema.
				validateAgainstSchema(t,
					"docs/contracts/schemas/vehicle-state.schema.json", body)

				var resp map[string]any
				decodeJSON(t, body, &resp)

				// MYR-24 promoted fields — every one of these MUST be
				// present and non-empty after MYR-24 closed.
				promoted := []string{
					"model", "year", "color", "fsdMilesSinceReset",
					"locationName", "locationAddress",
				}
				for _, k := range promoted {
					if _, ok := resp[k]; !ok {
						t.Errorf("MYR-24 promoted field %q missing from snapshot", k)
					}
				}

				// MYR-342: the owner's ride-sharing switch is carried on
				// the snapshot as well as the catalog row, because the
				// owner's toggle lives on this surface and a control
				// whose position can only be learned from a different
				// endpoint renders wrong on a cold open. The seeded car
				// has no control-state row, which the read COALESCEs to
				// enabled — the ordinary state of most cars.
				share, ok := resp["rideShareEnabled"]
				if !ok {
					t.Error("MYR-342 field `rideShareEnabled` missing from snapshot")
				} else if share != true {
					t.Errorf("rideShareEnabled = %v, want true (no control-state row = enabled)", share)
				}

				// Spot-check that values round-trip end-to-end (DB
				// SELECT -> scan -> handler -> JSON encode) rather
				// than just being present-but-zero.
				if got, _ := resp["model"].(string); got != modelStr {
					t.Errorf("model = %q, want %q", got, modelStr)
				}
				if got, _ := resp["color"].(string); got != colorStr {
					t.Errorf("color = %q, want %q", got, colorStr)
				}
				if got, _ := resp["locationName"].(string); got != locName {
					t.Errorf("locationName = %q, want %q", got, locName)
				}
				if got, _ := resp["locationAddress"].(string); got != locAddr {
					t.Errorf("locationAddress = %q, want %q", got, locAddr)
				}
				// year and fsdMilesSinceReset come back as float64
				// after json.Unmarshal — both are JSON numbers per the
				// schema.
				if got, _ := resp["year"].(float64); int(got) != modelYear {
					t.Errorf("year = %v, want %d", got, modelYear)
				}
				if got, _ := resp["fsdMilesSinceReset"].(float64); got != fsdMilesSince {
					t.Errorf("fsdMilesSinceReset = %v, want %v", got, fsdMilesSince)
				}

				// vehicleId in the body MUST match the path param —
				// catches the MYR-139 class of identity drift.
				if got, _ := resp["vehicleId"].(string); got != vehicleID {
					t.Errorf("vehicleId = %q, want %q", got, vehicleID)
				}
			},
		},
		{
			name: "unknown vehicleId returns 404 not_found",
			path: "/api/vehicles/" + unknownVehicle + "/snapshot",
			seed: func(t *testing.T, h *seedHelpers) {
				h.seedUser(ctx, t, ownerID)
			},
			token:      func(t *testing.T) string { return mintToken(t, ownerID, nil) },
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name: "vehicle owned by another user returns 403 vehicle_not_owned",
			path: "/api/vehicles/" + otherVehicleID + "/snapshot",
			seed: func(t *testing.T, h *seedHelpers) {
				h.seedUser(ctx, t, ownerID)
				h.seedUser(ctx, t, otherOwnerID)
				h.seedVehicle(ctx, t, vehicleSeed{
					ID:             otherVehicleID,
					UserID:         otherOwnerID,
					VIN:            otherVIN,
					Name:           "Not Mine",
					Model:          "Model Y",
					Year:           2023,
					Color:          "Pearl White Multi-Coat",
					Status:         "parked",
					ChargeLevel:    50,
					EstimatedRange: 200,
				})
			},
			token:      func(t *testing.T) string { return mintToken(t, ownerID, nil) },
			wantStatus: http.StatusForbidden,
			wantCode:   "vehicle_not_owned",
		},
		{
			name: "missing Authorization header returns 401",
			path: "/api/vehicles/" + vehicleID + "/snapshot",
			seed: func(t *testing.T, h *seedHelpers) {
				h.seedUser(ctx, t, ownerID)
				h.seedVehicle(ctx, t, vehicleSeed{
					ID:             vehicleID,
					UserID:         ownerID,
					VIN:            ownerVIN,
					Name:           "Stumpy",
					Model:          modelStr,
					Year:           modelYear,
					Color:          colorStr,
					Status:         "parked",
					ChargeLevel:    chargeLvl,
					EstimatedRange: rangeMiles,
				})
			},
			token:      func(t *testing.T) string { return "" },
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

			resp := doGET(t, srv, tc.path, tok)
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
