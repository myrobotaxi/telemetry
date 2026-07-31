//go:build contract

package contract_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Contract conformance for the MYR-184 vehicle-sharing schema surface
// (contracts v0.23.0, docs/contracts/schemas/vehicle-sharing.schema.json).
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
		"PatchShareInviteRequest",
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

// TestShareInviteSchema_ShareURL covers the MYR-368 signed join link. The
// schema can only police the field's presence and type — the signature is
// verified by unit tests in internal/telemetry and, in production, by the web
// join shell. What matters here is that `shareUrl` is genuinely ADDITIVE
// (0.21.0 → 0.22.0 is a minor bump): every 0.21.0-era payload must still
// validate, and the new field must not have been made required.
func TestShareInviteSchema_ShareURL(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)
	schema := compileSchema(t, c, sharingSchemaID+"#/$defs/ShareInvite")

	base := map[string]any{
		"inviteId":   "csh0123456789abcdef0123456789abcd",
		"vehicleId":  "clxyz1234567890abcdef",
		"label":      "Mira Chen",
		"permission": "live_history",
		"status":     "pending",
		"code":       "RBO246",
		"createdAt":  "2026-07-29T15:04:05Z",
		"expiresAt":  "2026-08-05T15:04:05Z",
	}
	withShareURL := func(v any) map[string]any {
		row := map[string]any{}
		for k, val := range base {
			row[k] = val
		}
		row["shareUrl"] = v
		return row
	}

	t.Run("a pending row WITH a shareUrl validates", func(t *testing.T) {
		row := withShareURL("https://myrobotaxi.app/join/RBO246?k=1.1785942245.Zm9v&from=Alex&to=Mira")
		if err := schema.Validate(row); err != nil {
			t.Fatalf("signed row rejected: %v", err)
		}
	})

	t.Run("a pending row WITHOUT a shareUrl still validates", func(t *testing.T) {
		// The additive guarantee, and the keyless-server fallback: a
		// consumer that finds `code` with no `shareUrl` shares the code.
		if err := schema.Validate(base); err != nil {
			t.Fatalf("unsigned row rejected — shareUrl was made required: %v", err)
		}
	})

	t.Run("an accepted row carries neither code nor shareUrl", func(t *testing.T) {
		row := map[string]any{
			"inviteId":   base["inviteId"],
			"vehicleId":  base["vehicleId"],
			"label":      base["label"],
			"permission": base["permission"],
			"status":     "accepted",
			"createdAt":  base["createdAt"],
			"acceptedAt": "2026-07-30T09:12:44Z",
		}
		if err := schema.Validate(row); err != nil {
			t.Fatalf("accepted row rejected: %v", err)
		}
	})

	t.Run("a link with both names omitted validates", func(t *testing.T) {
		// Both display names sanitize away independently, so the shortest
		// legal link is code + k alone.
		row := withShareURL("https://myrobotaxi.app/join/RBO246?k=1.1785942245.Zm9v")
		if err := schema.Validate(row); err != nil {
			t.Fatalf("nameless link rejected: %v", err)
		}
	})

	t.Run("a non-string shareUrl is rejected", func(t *testing.T) {
		for _, bad := range []any{42, true, map[string]any{"url": "x"}, []any{"x"}} {
			if err := schema.Validate(withShareURL(bad)); err == nil {
				t.Errorf("shareUrl %v (%T) was accepted; it is typed string/uri", bad, bad)
			}
		}
	})

	t.Run("shareUrl is classified P1 and typed as a uri", func(t *testing.T) {
		// It embeds the code, so it is the same P1 bearer tier as `code`.
		// An unclassified field on this object is a contract-guard
		// failure, and the annotation is also what the TS/Swift codegen
		// folds into the generated doc comment — losing it breaks no
		// validation at all, which is why it needs its own assertion.
		raw := loadRawSchema(t, root, "vehicle-sharing.schema.json")
		defs, ok := raw["$defs"].(map[string]any)
		if !ok {
			t.Fatal("schema has no $defs")
		}
		invite, ok := defs["ShareInvite"].(map[string]any)
		if !ok {
			t.Fatal("$defs.ShareInvite missing")
		}
		props, ok := invite["properties"].(map[string]any)
		if !ok {
			t.Fatal("$defs.ShareInvite.properties missing")
		}
		prop, ok := props["shareUrl"].(map[string]any)
		if !ok {
			t.Fatal("property shareUrl missing")
		}
		if got := prop["x-classification"]; got != "P1" {
			t.Errorf("x-classification = %v, want P1", got)
		}
		if got := prop["format"]; got != "uri" {
			t.Errorf("format = %v, want uri", got)
		}
	})
}

// TestShareInviteSchema_GrantFlags covers the MYR-369 per-grant flags. What
// matters here is that they are genuinely ADDITIVE (0.22.0 → 0.23.0 is a minor
// bump): every 0.22.0-era payload must still validate, and neither flag may
// have been made required.
func TestShareInviteSchema_GrantFlags(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)
	schema := compileSchema(t, c, sharingSchemaID+"#/$defs/ShareInvite")

	accepted := map[string]any{
		"inviteId":   "csh0123456789abcdef0123456789abcd",
		"vehicleId":  "clxyz1234567890abcdef",
		"label":      "Mira Chen",
		"permission": "rides",
		"status":     "accepted",
		"createdAt":  "2026-07-29T15:04:05Z",
		"acceptedAt": "2026-07-30T09:12:44Z",
	}
	with := func(extra map[string]any) map[string]any {
		row := map[string]any{}
		for k, v := range accepted {
			row[k] = v
		}
		for k, v := range extra {
			row[k] = v
		}
		return row
	}

	t.Run("an accepted row carrying both flags validates", func(t *testing.T) {
		row := with(map[string]any{"allowRides": true, "suspended": false})
		if err := schema.Validate(row); err != nil {
			t.Fatalf("a MYR-369 accepted grant was rejected: %v", err)
		}
	})

	t.Run("a suspended grant validates — the OWNER still sees it", func(t *testing.T) {
		// Only the VIEWER-facing surfaces make a suspended grant disappear;
		// the owner has to be able to see one to lift it.
		row := with(map[string]any{"allowRides": true, "suspended": true})
		if err := schema.Validate(row); err != nil {
			t.Fatalf("a suspended grant was rejected: %v", err)
		}
	})

	t.Run("a 0.22.0-era accepted row without the flags still validates", func(t *testing.T) {
		// The additive guarantee. Absence means "the server predates
		// MYR-369", which a consumer reads as not-suspended.
		if err := schema.Validate(accepted); err != nil {
			t.Fatalf("a pre-MYR-369 accepted row was rejected — a flag was made required: %v", err)
		}
	})

	t.Run("the flags are not in the required list", func(t *testing.T) {
		// Asserted against the raw schema as well as by the payload above,
		// because "still validates without it" and "is not required" fail
		// in the same direction only by luck.
		for _, field := range shareInviteRequiredFields(t) {
			if field == "allowRides" || field == "suspended" {
				t.Errorf("%q is required; that would make 0.23.0 a BREAKING change", field)
			}
		}
	})

	t.Run("a non-boolean flag is rejected", func(t *testing.T) {
		for _, bad := range []any{"true", 1, nil} {
			row := with(map[string]any{"allowRides": bad})
			if err := schema.Validate(row); err == nil {
				t.Errorf("allowRides = %v (%T) was accepted; the field is a boolean", bad, bad)
			}
		}
	})
}

// TestPatchShareInviteRequestSchema pins the partial-edit body: both fields
// optional, at least one required, and nothing else accepted.
func TestPatchShareInviteRequestSchema(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)
	schema := compileSchema(t, c, sharingSchemaID+"#/$defs/PatchShareInviteRequest")

	t.Run("each field alone is a valid patch", func(t *testing.T) {
		for _, body := range []map[string]any{
			{"allowRides": true},
			{"allowRides": false},
			{"suspended": true},
			{"suspended": false},
			{"allowRides": false, "suspended": true},
		} {
			if err := schema.Validate(body); err != nil {
				t.Errorf("valid patch %v rejected: %v", body, err)
			}
		}
	})

	t.Run("an EMPTY body is rejected", func(t *testing.T) {
		// minProperties: 1. A request that asks for nothing must not be
		// able to look like an applied edit — on an access-control surface
		// that is the worst available failure mode.
		if err := schema.Validate(map[string]any{}); err == nil {
			t.Fatal("an empty patch body was accepted")
		}
	})

	t.Run("unknown properties are rejected", func(t *testing.T) {
		// additionalProperties: false. `permission` in particular is NOT
		// patchable — on an accepted row it is derived output, not state.
		for _, key := range []string{"permission", "label", "status", "revoked"} {
			if err := schema.Validate(map[string]any{key: "x"}); err == nil {
				t.Errorf("property %q was accepted on a patch body", key)
			}
		}
	})
}

// TestSharePermissionEnum_RetainsRetiredMember pins that live_history is still
// DECODABLE. Removing it would break every installed client whose decoder lists
// it and would make 0.23.0 a major bump; the member stays on the wire and is
// simply never emitted.
func TestSharePermissionEnum_RetainsRetiredMember(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)
	schema := compileSchema(t, c, sharingSchemaID+"#/$defs/SharePermission")

	for _, member := range []string{"live", "live_history", "rides"} {
		if err := schema.Validate(member); err != nil {
			t.Errorf("SharePermission %q was rejected: %v", member, err)
		}
	}
	if err := schema.Validate("history"); err == nil {
		t.Error("an invented member was accepted")
	}
}

// shareInviteRequiredFields reads ShareInvite's `required` list straight out of
// the schema file, so an additive-guarantee assertion cannot pass merely because
// a fixture happened to omit the field.
func shareInviteRequiredFields(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), schemasDir, "vehicle-sharing.schema.json"))
	if err != nil {
		t.Fatalf("read vehicle-sharing.schema.json: %v", err)
	}
	var doc struct {
		Defs struct {
			ShareInvite struct {
				Required []string `json:"required"`
			} `json:"ShareInvite"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal vehicle-sharing.schema.json: %v", err)
	}
	if len(doc.Defs.ShareInvite.Required) == 0 {
		t.Fatal("read zero required fields from ShareInvite — the walk is broken")
	}
	return doc.Defs.ShareInvite.Required
}
