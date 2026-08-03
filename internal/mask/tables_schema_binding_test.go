package mask

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// Schema-bound tripwires for the vehicle_state role tables (MYR-435,
// adversarial-review follow-up).
//
// TestVehicleStateRoleListsPartitionOwnerFields proves the two role lists
// partition the OWNER LIST — but the owner list is itself hand-maintained, so
// that check is self-referential at the edge: a field added to
// vehicle-state.schema.json and to the response struct, but forgotten in
// tables.go, is invisible to it. It would simply never reach any role, which is
// fail-closed and safe, but silent — and "silent" is the property the MYR-427
// audit was written about.
//
// These tests close that edge by binding the tables to the SCHEMA, so a new
// wire field cannot ship unclassified. Same idiom as the MYR-298 mirror
// tripwires: the assertion exists to land the diff in front of a reviewer with
// the reasoning written directly above it.

// wireOnlyOwnerFields are owner-list entries that are deliberately NOT
// properties of vehicle-state.schema.json. Each needs a reason, because the
// default expectation is that a masked wire field is a schema field.
//
// Listing them explicitly is what lets the tripwire below be strict: anything
// in the owner list that is neither a schema property nor named here is a typo
// or an undocumented wire name, and fails.
var wireOnlyOwnerFields = map[string]string{
	// rest-api.md §7.1 emits the navigation group under these aliased names on
	// the snapshot; the mask carries both spellings so it is resilient to
	// whichever shape the handler produces. The canonical schema names
	// (destinationName, etaMinutes, …) are present as schema properties.
	"navDestinationName":       "§7.1 snapshot alias of destinationName",
	"navDestinationLocation":   "§7.1 snapshot alias of destinationLatitude/Longitude",
	"navOriginLocation":        "§7.1 snapshot alias of originLatitude/Longitude",
	"navEtaMinutes":            "§7.1 snapshot alias of etaMinutes",
	"navTripDistanceRemaining": "§7.1 snapshot alias of tripDistanceRemaining",

	// WS-only accumulated GPS trail emitted by internal/ws/route_broadcast.go
	// (websocket-protocol.md §4.1.6). It is a frame field, not part of the
	// REST VehicleState document, so it has no schema property.
	"driveTrailCoordinates": "WS-only live trail (websocket-protocol.md §4.1.6)",

	// Pairing flag carried on the wire but never added to the VehicleState
	// schema. MYR-435 classifies it owner-only (controls infrastructure); the
	// missing schema property predates this issue and is left alone
	// deliberately rather than fixed by a mask-narrowing PR.
	"virtualKeyPaired": "wire-only pairing flag; absent from the schema pre-MYR-435",
}

// vehicleStateSchemaProperties reads the property names of the canonical
// VehicleState schema.
func vehicleStateSchemaProperties(t *testing.T) map[string]map[string]any {
	t.Helper()

	path := filepath.Join(contractsRoot(t), "docs", "contracts", "schemas", "vehicle-state.schema.json")
	data, err := os.ReadFile(path) //nolint:gosec // test-only read of a repo-relative contract schema
	if err != nil {
		t.Fatalf("read vehicle-state.schema.json: %v", err)
	}
	var doc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if len(doc.Properties) == 0 {
		t.Fatal("read zero properties from vehicle-state.schema.json — the walk is broken")
	}
	return doc.Properties
}

// contractsRoot walks up to the repo root (the directory holding go.mod).
func contractsRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (no go.mod found)")
		}
		dir = parent
	}
}

// TestEverySchemaFieldIsClassified is the tripwire that makes "a new field
// cannot ship unclassified" true rather than merely intended.
//
// Every property of vehicle-state.schema.json must appear in the owner list AND
// be classified into exactly one of the two role lists. Adding a property to
// the schema therefore fails this test until someone decides whether viewers
// see it — which is the decision the MYR-427 audit found had never been made
// for media and cabin state.
func TestEverySchemaFieldIsClassified(t *testing.T) {
	props := vehicleStateSchemaProperties(t)

	owner := For(ResourceVehicleState, auth.RoleOwner)
	viewer := For(ResourceVehicleState, auth.RoleViewer)

	ownerOnly := make(map[string]struct{}, len(vehicleStateOwnerOnlyFields))
	for _, f := range vehicleStateOwnerOnlyFields {
		ownerOnly[f] = struct{}{}
	}

	var unclassified []string
	for field := range props {
		if !owner.allows(field) {
			t.Errorf("%q is a property of vehicle-state.schema.json but is absent from "+
				"vehicleStateOwnerFields — it would reach NO role. Add it to the owner "+
				"list, then classify it viewer-visible or owner-only (MYR-435)", field)
			continue
		}
		_, isViewer := viewer.Allowed[field]
		_, isOwnerOnly := ownerOnly[field]
		if !isViewer && !isOwnerOnly {
			unclassified = append(unclassified, field)
		}
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Errorf("schema fields reaching owners but classified for NEITHER role: %v — "+
			"add each to vehicleStateViewerFields or vehicleStateOwnerOnlyFields",
			unclassified)
	}
}

// TestOwnerListCarriesNoUndocumentedWireName is the other direction: an owner
// entry that is not a schema property must be a KNOWN wire-only name. Without
// this, a typo in the owner list ("interiorTemperature") sits there forever
// allowing a key nothing emits, and looks like coverage.
func TestOwnerListCarriesNoUndocumentedWireName(t *testing.T) {
	props := vehicleStateSchemaProperties(t)

	for _, field := range vehicleStateOwnerFields {
		if _, inSchema := props[field]; inSchema {
			continue
		}
		if _, documented := wireOnlyOwnerFields[field]; documented {
			continue
		}
		t.Errorf("%q is in vehicleStateOwnerFields but is neither a property of "+
			"vehicle-state.schema.json nor listed in wireOnlyOwnerFields — likely a typo, "+
			"or a wire-only field that needs a documented reason", field)
	}

	// Keep the allowlist itself honest: an entry that has since become a real
	// schema property, or was removed from the owner list, is stale.
	for field := range wireOnlyOwnerFields {
		if _, inSchema := props[field]; inSchema {
			t.Errorf("%q is listed in wireOnlyOwnerFields but IS now a schema property — "+
				"remove the exemption", field)
		}
		found := false
		for _, f := range vehicleStateOwnerFields {
			if f == field {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q is listed in wireOnlyOwnerFields but is no longer in the owner "+
				"list — stale entry", field)
		}
	}
}

// TestViewerMaskNeverSplitsAnAtomicGroup is the mechanical guard for the rule
// MYR-435 reasoned about by hand.
//
// vehicle-state-schema.md §2.4 requires the members of an `x-atomic-group` to
// be emitted together — a consumer that receives `status` without its companion
// `gearPosition` cannot render either honestly. A ROLE MASK can break that
// invariant just as easily as a broadcast bug can, by allowing some members and
// denying others, and the narrowing in MYR-435 came close: `gearPosition` was
// kept specifically because splitting the `gear` group would have been its own
// contract violation.
//
// That reasoning was prose in a comment. This asserts it against the schema, so
// a future narrowing cannot split a group by inspection failure. Today: the nav
// / gear / gps / charge groups are entirely viewer-visible, and no declared
// group is entirely owner-only — but the test states the invariant, not today's
// arrangement, so either outcome passes as long as the group is not SPLIT.
func TestViewerMaskNeverSplitsAnAtomicGroup(t *testing.T) {
	props := vehicleStateSchemaProperties(t)
	viewer := For(ResourceVehicleState, auth.RoleViewer)

	groups := make(map[string][]string)
	for field, spec := range props {
		group, ok := spec["x-atomic-group"].(string)
		if !ok || group == "" {
			continue
		}
		groups[group] = append(groups[group], field)
	}
	if len(groups) == 0 {
		t.Fatal("found no x-atomic-group declarations in vehicle-state.schema.json — " +
			"the guard is not actually reading the groups")
	}

	for group, members := range groups {
		sort.Strings(members)
		var visible, hidden []string
		for _, m := range members {
			if _, ok := viewer.Allowed[m]; ok {
				visible = append(visible, m)
			} else {
				hidden = append(hidden, m)
			}
		}
		if len(visible) > 0 && len(hidden) > 0 {
			t.Errorf("viewer mask SPLITS the %q atomic group: visible=%v hidden=%v. "+
				"vehicle-state-schema.md §2.4 requires members to travel together — a "+
				"viewer receiving part of a group cannot render any of it honestly. "+
				"Either keep the whole group or withhold the whole group.",
				group, visible, hidden)
		}
	}
}
