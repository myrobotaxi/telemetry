//go:build contract

package contract_test

import (
	"testing"
)

// Contract conformance for the MYR-184 vehicle-sharing schema surface
// (contracts v0.19.0, docs/contracts/schemas/vehicle-sharing.schema.json).
//
// The file is picked up by the schemas/*.json glob every other test in this
// package relies on, which means a broken $ref inside it would silently poison
// UNRELATED validations rather than failing anything of its own. These tests
// compile it explicitly so the new surface has a failure of its own to point at.

const sharingSchemaID = "https://myrobotaxi.com/schemas/vehicle-sharing.schema.json"

// TestVehicleSharingSchema_Compiles proves the schema and each of its $defs
// resolve — including the cross-file $ref from RedeemShareInviteResponse into
// vehicle-summary.schema.json#/$defs/VehicleSummary, which is the one link that
// spans two files and therefore the one that breaks when either moves.
func TestVehicleSharingSchema_Compiles(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)

	for _, def := range []string{
		"SharePermission",
		"ShareInvite",
		"CreateShareInviteRequest",
		"RedeemShareInviteRequest",
		"RedeemShareInviteResponse",
	} {
		t.Run(def, func(t *testing.T) {
			if _, err := c.Compile(sharingSchemaID + "#/$defs/" + def); err != nil {
				t.Fatalf("compile $defs/%s: %v", def, err)
			}
		})
	}

	t.Run("ShareInviteListResponse envelope", func(t *testing.T) {
		if _, err := c.Compile(sharingSchemaID); err != nil {
			t.Fatalf("compile root envelope: %v", err)
		}
	})
}

// TestShareInviteSchema_StatusAndCodeRules pins the two rules the server's
// serializer implements by hand, so a schema change that relaxed either would
// be caught here rather than by a client.
func TestShareInviteSchema_StatusAndCodeRules(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)
	schema := compileSchema(t, c, sharingSchemaID+"#/$defs/ShareInvite")

	pending := map[string]any{
		"inviteId":   "csh0123456789abcdef0123456789abcd",
		"vehicleId":  "clxyz1234567890abcdef",
		"label":      "Mira Chen",
		"permission": "live_history",
		"status":     "pending",
		"code":       "RBO246",
		"createdAt":  "2026-07-29T15:04:05Z",
		"expiresAt":  "2026-08-05T15:04:05Z",
	}
	accepted := map[string]any{
		"inviteId":   "csh0123456789abcdef0123456789abcd",
		"vehicleId":  "clxyz1234567890abcdef",
		"label":      "Mira Chen",
		"permission": "live_history",
		"status":     "accepted",
		"createdAt":  "2026-07-29T15:04:05Z",
		"acceptedAt": "2026-07-30T09:12:44Z",
	}

	t.Run("a pending row with a code validates", func(t *testing.T) {
		if err := schema.Validate(pending); err != nil {
			t.Fatalf("valid pending row rejected: %v", err)
		}
	})

	t.Run("an accepted row WITHOUT a code validates", func(t *testing.T) {
		if err := schema.Validate(accepted); err != nil {
			t.Fatalf("valid accepted row rejected: %v", err)
		}
	})

	t.Run("'revoked' is not a wire status", func(t *testing.T) {
		// Revoked rows are server-side tombstones and are never serialized;
		// the enum has no member for them, so a server that leaked one would
		// fail this shape.
		row := map[string]any{}
		for k, v := range pending {
			row[k] = v
		}
		row["status"] = "revoked"
		if err := schema.Validate(row); err == nil {
			t.Fatal("status 'revoked' was accepted by the wire schema")
		}
	})

	t.Run("a malformed code is rejected", func(t *testing.T) {
		for _, bad := range []string{"rbo246", "RBO24", "RBO2467", "RB-246"} {
			row := map[string]any{}
			for k, v := range pending {
				row[k] = v
			}
			row["code"] = bad
			if err := schema.Validate(row); err == nil {
				t.Errorf("code %q was accepted; the pattern is ^[A-Z0-9]{6}$", bad)
			}
		}
	})

	t.Run("an unknown permission tier is rejected", func(t *testing.T) {
		row := map[string]any{}
		for k, v := range pending {
			row[k] = v
		}
		row["permission"] = "admin"
		if err := schema.Validate(row); err == nil {
			t.Fatal("permission 'admin' was accepted")
		}
	})
}
