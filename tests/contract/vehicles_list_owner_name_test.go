//go:build contract

package contract_test

import (
	"context"
	"net/http"
	"testing"
)

// Contract conformance for MYR-581's `VehicleSummary.ownerFirstName` (§7.0,
// contracts v0.37.0) — the real handler, the real lean catalog query, and the real
// three-rung identity ladder, against a real Postgres.
//
// WHY THIS EXISTS AS A CONTRACT TEST and not only as unit tests. The unit tests
// cover the projection and the first-token reduction in isolation, with the store
// stubbed; what they structurally cannot cover is whether the SQL RUNS and whether
// `COALESCE` picks the rung the contract says it picks. That gap is not theoretical
// — this suite is what caught the shipped version of exactly it: the harness had no
// `go_identity_apple` table, so the ladder's middle rung named a missing relation
// and every §7.0 and §7.1 request answered `500`. A unit test could not have seen
// it, and neither could a reader of the SQL.
//
// SINCE MYR-583 EVERY NAMED ARM ALSO CONFIRMS. A name on a rung is no longer
// enough to be published to a counterparty: the owner must have confirmed it
// through §7.26, because the Apple rung is a first-consent payload nobody approved
// and the `"User"` rung may hold a legacy web placeholder. The ladder arms
// therefore call `confirmName` alongside the rung they are about — the confirmation
// is a precondition of the ladder mattering, not one of the rungs — and a new final
// arm pins the unconfirmed case, which is the one the client's ruling is about.
//
// The arms are ordered by PRECEDENCE, and each proves a different claim.

func TestContract_GETVehicles_OwnerFirstNameLadder(t *testing.T) {
	ctx := context.Background()

	const (
		ownerID    = "usr_ladder_owner"
		vehicleID  = "veh_ladder_00001"
		vehicleVIN = "5YJ3E1EA1PF0L0101"
	)

	tests := []struct {
		name string
		// seedIdentity populates whichever rungs this arm is about. The `"User"`
		// row itself is always seeded — the token validator requires it — so this
		// only decides which NAMES exist.
		seedIdentity func(t *testing.T, h *seedHelpers)
		// want is the expected wire value; nil means an explicit JSON null.
		want any
	}{
		{
			// The legacy web owner: the easy case, and the only one a
			// `"User".name`-only lookup would have got right.
			name: "the Prisma User rung resolves, reduced to a first name",
			seedIdentity: func(t *testing.T, h *seedHelpers) {
				h.setUserName(ctx, t, ownerID, "Amruth Kelkar")
				h.confirmName(ctx, t, ownerID)
			},
			want: "Amruth",
		},
		{
			// THE ARM THE LADDER EXISTS FOR. No name on the top rung — the state
			// `queryProvisionUser`'s `ON CONFLICT DO NOTHING` leaves behind — while
			// the Apple identity knows the person perfectly well. A one-table
			// lookup calls this owner nameless and refuses their rides.
			name: "the APPLE rung resolves when the User row has a NULL name",
			seedIdentity: func(t *testing.T, h *seedHelpers) {
				h.seedAppleBinding(ctx, t, "sub_ladder_1", ownerID, "James Guan")
				h.confirmName(ctx, t, ownerID)
			},
			want: "James",
		},
		{
			// Proves the COALESCE falls ALL the way through rather than stopping
			// at the first NULL.
			name: "the go_users rung resolves when both rungs above are empty",
			seedIdentity: func(t *testing.T, h *seedHelpers) {
				h.setGoUserName(ctx, t, ownerID, "Nabil Rahman")
				h.confirmName(ctx, t, ownerID)
			},
			want: "Nabil",
		},
		{
			// The precedence assertions. Only one of three answers can be right,
			// and an arm that populated a single rung could never tell a correct
			// ladder from an accidentally reordered one.
			name: "PRECEDENCE: the Prisma User rung out-ranks the other two",
			seedIdentity: func(t *testing.T, h *seedHelpers) {
				h.setUserName(ctx, t, ownerID, "Top Rung")
				h.seedAppleBinding(ctx, t, "sub_ladder_2", ownerID, "Middle Rung")
				h.setGoUserName(ctx, t, ownerID, "Bottom Rung")
				h.confirmName(ctx, t, ownerID)
			},
			want: "Top",
		},
		{
			name: "PRECEDENCE: the Apple rung out-ranks go_users",
			seedIdentity: func(t *testing.T, h *seedHelpers) {
				h.seedAppleBinding(ctx, t, "sub_ladder_3", ownerID, "Middle Rung")
				h.setGoUserName(ctx, t, ownerID, "Bottom Rung")
				h.confirmName(ctx, t, ownerID)
			},
			want: "Middle",
		},
		{
			// A whitespace-only name is NOT a name, and this is the arm that keeps
			// the wire value and the OFFERABILITY GATE honest with each other. The
			// two are derived from one SQL expression, and the ladder's `TRIM` is
			// what makes both read "nameless" here. Without it the gate would
			// answer NAMED — offering a car — while this row emitted null.
			name: "a whitespace-only name on the top rung falls through to null",
			seedIdentity: func(t *testing.T, h *seedHelpers) {
				h.setUserName(ctx, t, ownerID, "   ")
				h.confirmName(ctx, t, ownerID)
			},
			want: nil,
		},
		{
			// THE MYR-583 ARM, and the one the client's ruling is about: a name
			// the ladder resolves perfectly well and its owner never confirmed.
			// It must read exactly like no name at all, so the client renders
			// "Setting Up" and the car refuses ride requests. Note this arm
			// seeds the TOP rung — the one a legacy web account carries — and
			// asserts the strongest rung on the ladder loses to an absent
			// confirmation.
			name: "a resolvable name NOBODY CONFIRMED reads as null (MYR-583)",
			seedIdentity: func(t *testing.T, h *seedHelpers) {
				h.setUserName(ctx, t, ownerID, "Amruth Kelkar")
			},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, seeder := setupTestServer(t)
			seeder.seedUser(ctx, t, ownerID)
			tc.seedIdentity(t, seeder)
			seeder.seedVehicle(ctx, t, vehicleSeed{
				ID:     vehicleID,
				UserID: ownerID,
				VIN:    vehicleVIN,
				Name:   "Stumpy",
				Model:  "Model 3",
				Year:   2024,
				Status: "parked",
			})

			resp := doGET(t, srv, "/api/vehicles", mintToken(t, ownerID, nil))
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, string(body))
			}

			var decoded struct {
				Items []map[string]any `json:"items"`
			}
			decodeJSON(t, body, &decoded)
			if len(decoded.Items) != 1 {
				t.Fatalf("expected 1 item, got %d; body: %s", len(decoded.Items), string(body))
			}

			got, ok := decoded.Items[0]["ownerFirstName"]
			if !ok {
				t.Fatalf("missing `ownerFirstName`; row keys: %v", keysOf(decoded.Items[0]))
			}
			if got != tc.want {
				t.Errorf("ownerFirstName = %v, want %v — the ladder picked the wrong rung "+
					"(or the reduction kept more than the first token)", got, tc.want)
			}

			// Every arm re-validates the row against the canonical schema. The
			// field is nullable there, so a null arm and a named arm must BOTH
			// satisfy it — a `type: string` that forgot `null` would pass five of
			// these six arms.
			validateAgainstSchema(t,
				"docs/contracts/schemas/vehicle-summary.schema.json#/$defs/VehicleSummary",
				marshalRow(t, decoded.Items[0]))
		})
	}
}
