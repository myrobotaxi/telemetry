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
//   - 200 owner happy path: the four geocoded labels (MYR-447) are
//     seeded as ciphertext only and MUST come back on the wire
//     unchanged — plus the absent case, where the two NOT NULL labels
//     stay `""` and the two nullable ones stay `null`.
//   - 404 not_found for an unknown vehicleId.
//   - 403 vehicle_not_owned for a vehicle owned by someone else.
//   - 401 on missing Authorization header.
package contract_test

import (
	"context"
	"net/http"
	"testing"
	"time"
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
		destName       = "Ferry Building"
		destAddr       = "1 Ferry Building, San Francisco, CA 94111"
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
				// MYR-447: seedVehicle plants all four labels as
				// ciphertext ONLY (the plaintext columns get '' / NULL,
				// as production writes them). Every label assertion
				// below therefore fails unless the read path decrypts.
				h.seedVehicle(ctx, t, vehicleSeed{
					ID:                 vehicleID,
					UserID:             ownerID,
					VIN:                ownerVIN,
					Name:               "Stumpy",
					Model:              modelStr,
					Year:               modelYear,
					Color:              colorStr,
					Status:             "parked",
					ChargeLevel:        chargeLvl,
					EstimatedRange:     rangeMiles,
					LocationName:       locName,
					LocationAddress:    locAddr,
					DestinationName:    destName,
					DestinationAddress: destAddr,
					FsdMilesReset:      fsdMilesSince,
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

				// MYR-491: the setup state is ALWAYS keyed, and for a
				// vehicle with no fleet-config schedule row it is an
				// explicit null — the server makes NO CLAIM. Asserting
				// presence catches an omitempty regression; asserting
				// null catches the opposite and worse failure, a
				// derivation that invents a setup card for a car that
				// does not need one. (Null here is NOT "ready" — see
				// rest-api.md §7.1.)
				setup, ok := resp["setupState"]
				if !ok {
					t.Error("MYR-491 field `setupState` missing from snapshot; " +
						"the key is always emitted, null when there is nothing to say")
				} else if setup != nil {
					t.Errorf("setupState = %v, want null (no fleet-config schedule row "+
						"means no claim)", setup)
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
				// MYR-447: the nullable half of the label set. These come
				// back as JSON strings when sealed values exist — same
				// keys, same values, same types as before sealing.
				if got, _ := resp["destinationName"].(string); got != destName {
					t.Errorf("destinationName = %v, want %q", resp["destinationName"], destName)
				}
				if got, _ := resp["destinationAddress"].(string); got != destAddr {
					t.Errorf("destinationAddress = %v, want %q", resp["destinationAddress"], destAddr)
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
			// MYR-447 nullability guard. Sealing changed the storage of
			// the four labels, and the absent case is where a change of
			// storage most easily changes the WIRE: `NULL` ciphertext has
			// to keep producing `""` for the two NOT NULL labels and
			// `null` for the two nullable ones — never an empty string
			// where a null belonged, and never a missing key.
			// MYR-491 / MYR-503, the whole point of the field. This is the
			// tester's car verbatim: linked minutes ago, Tesla refused the
			// telemetry config for a missing virtual key, `status` still the
			// write-once `offline` schema default, `lastUpdated` FRESH because
			// the provisioning INSERT just wrote it.
			//
			// That last detail is what this case exists to pin. A derivation
			// that treated a fresh `lastUpdated` as "this car is streaming"
			// would suppress the state for the first thirty minutes of a new
			// owner's life — precisely the window in which they open the app,
			// tap Lock, and meet a dead button.
			name: "a linked car whose virtual key was never paired says so on the wire",
			path: "/api/vehicles/" + vehicleID + "/snapshot",
			seed: func(t *testing.T, h *seedHelpers) {
				h.seedUser(ctx, t, ownerID)
				h.seedVehicle(ctx, t, vehicleSeed{
					ID:          vehicleID,
					UserID:      ownerID,
					VIN:         ownerVIN,
					Name:        "Amruth's X Plaid",
					Model:       "Model X",
					Year:        2025,
					Status:      "offline",
					LastUpdated: time.Now().UTC(),
				})
				h.seedFleetConfigAttempt(ctx, t, vehicleID,
					"awaiting_virtual_key", time.Now().UTC().Add(-4*time.Minute))
			},
			token:      func(t *testing.T) string { return mintToken(t, ownerID, nil) },
			wantStatus: http.StatusOK,
			assertBody: func(t *testing.T, body []byte) {
				validateAgainstSchema(t,
					"docs/contracts/schemas/vehicle-state.schema.json", body)

				var resp map[string]any
				decodeJSON(t, body, &resp)

				raw, ok := resp["setupState"]
				if !ok {
					t.Fatal("setupState missing from the snapshot")
				}
				obj, ok := raw.(map[string]any)
				if !ok {
					t.Fatalf("setupState = %#v, want an object", raw)
				}
				if obj["state"] != "awaiting_virtual_key" {
					t.Errorf("setupState.state = %v, want awaiting_virtual_key — a fresh "+
						"lastUpdated on a never-streamed car must NOT suppress the state",
						obj["state"])
				}
				since, ok := obj["since"].(string)
				if !ok {
					t.Fatalf("setupState.since = %#v, want an RFC 3339 string", obj["since"])
				}
				at, err := time.Parse(time.RFC3339, since)
				if err != nil {
					t.Errorf("setupState.since = %q is not RFC 3339: %v", since, err)
				} else if at.After(time.Now().Add(time.Minute)) {
					t.Errorf("setupState.since = %q is in the future; the server floors it "+
						"at the read instant so clients can subtract it safely", since)
				}
			},
		},
		{
			name: "vehicle with no geocoded labels keeps the pre-sealing null shape",
			path: "/api/vehicles/" + vehicleID + "/snapshot",
			seed: func(t *testing.T, h *seedHelpers) {
				h.seedUser(ctx, t, ownerID)
				h.seedVehicle(ctx, t, vehicleSeed{
					ID:             vehicleID,
					UserID:         ownerID,
					VIN:            ownerVIN,
					Name:           "Ungeocoded",
					Model:          modelStr,
					Year:           modelYear,
					Color:          colorStr,
					Status:         "parked",
					ChargeLevel:    chargeLvl,
					EstimatedRange: rangeMiles,
				})
			},
			token:      func(t *testing.T) string { return mintToken(t, ownerID, nil) },
			wantStatus: http.StatusOK,
			assertBody: func(t *testing.T, body []byte) {
				validateAgainstSchema(t,
					"docs/contracts/schemas/vehicle-state.schema.json", body)

				var resp map[string]any
				decodeJSON(t, body, &resp)

				for _, k := range []string{"locationName", "locationAddress"} {
					got, ok := resp[k]
					if !ok {
						t.Errorf("%s: key missing; want present and empty", k)
						continue
					}
					if got != "" {
						t.Errorf("%s = %v, want \"\" (no ciphertext = no label)", k, got)
					}
				}
				for _, k := range []string{"destinationName", "destinationAddress"} {
					got, ok := resp[k]
					if !ok {
						t.Errorf("%s: key missing; want present and null", k)
						continue
					}
					if got != nil {
						t.Errorf("%s = %v, want null (nullable label, no ciphertext)", k, got)
					}
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
