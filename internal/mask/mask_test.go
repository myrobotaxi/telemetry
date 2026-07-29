package mask

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

func TestApply_DenyAllMask(t *testing.T) {
	input := map[string]any{
		"speed":        65,
		"chargeLevel":  82,
		"licensePlate": "ABC-123",
	}
	out, masked := Apply(input, ResourceMask{}) // zero-value = deny-all

	if len(out) != 0 {
		t.Errorf("deny-all mask: expected empty output, got %v", out)
	}
	sort.Strings(masked)
	want := []string{"chargeLevel", "licensePlate", "speed"}
	if !reflect.DeepEqual(masked, want) {
		t.Errorf("fieldsMasked = %v, want %v", masked, want)
	}
}

func TestApply_FullAllowList(t *testing.T) {
	mask := setFromFields([]string{"speed", "chargeLevel"})
	input := map[string]any{
		"speed":       65,
		"chargeLevel": 82,
	}
	out, masked := Apply(input, mask)

	if !reflect.DeepEqual(out, input) {
		t.Errorf("full allow: out = %v, want %v", out, input)
	}
	if len(masked) != 0 {
		t.Errorf("full allow: fieldsMasked = %v, want []", masked)
	}
}

func TestApply_PartialMask_StripsVIN(t *testing.T) {
	// Viewer projection of a vehicle_state payload carrying the full vin.
	// The viewer allow-list excludes `vin` (MYR-279); verify it is stripped
	// and reported in fieldsMasked.
	//
	// This case used `licensePlate` until MYR-286 moved that field into BOTH
	// role allow-lists, leaving `vin` as the only owner-only VehicleState
	// field and therefore the canonical partial-mask fixture.
	mask := For(ResourceVehicleState, auth.RoleViewer)
	input := map[string]any{
		"speed":       65,
		"chargeLevel": 82,
		"vin":         "7SAYGDET7TA613795",
	}

	out, masked := Apply(input, mask)

	if _, present := out["vin"]; present {
		t.Error("viewer projection still contains the full vin")
	}
	if out["speed"] != 65 {
		t.Errorf("speed lost: got %v", out["speed"])
	}
	if out["chargeLevel"] != 82 {
		t.Errorf("chargeLevel lost: got %v", out["chargeLevel"])
	}
	if !reflect.DeepEqual(masked, []string{"vin"}) {
		t.Errorf("fieldsMasked = %v, want [vin]", masked)
	}
}

func TestApply_AbsentNotNulled_OnJSONSerialization(t *testing.T) {
	// rest-api.md §5.1 requires denied fields to be ABSENT from the
	// JSON, not emitted with a null value. Verify by round-tripping
	// the projected map and inspecting raw JSON for the key name.
	mask := For(ResourceVehicleState, auth.RoleViewer)
	input := map[string]any{
		"speed": 65,
		"vin":   "7SAYGDET7TA613795",
	}

	out, _ := Apply(input, mask)

	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(encoded); !contains(got, "speed") {
		t.Errorf("expected JSON to contain speed: %s", got)
	}
	if got := string(encoded); contains(got, "vin") {
		t.Errorf("JSON must NOT contain vin (absent, not nulled): %s", got)
	}
	if got := string(encoded); contains(got, "null") {
		t.Errorf("JSON must NOT contain null for stripped key: %s", got)
	}
}

func TestApply_Idempotent(t *testing.T) {
	mask := For(ResourceVehicleState, auth.RoleViewer)
	input := map[string]any{
		"speed":       65,
		"chargeLevel": 82,
		"vin":         "7SAYGDET7TA613795",
	}

	first, _ := Apply(input, mask)
	second, secondMasked := Apply(first, mask)

	if !reflect.DeepEqual(first, second) {
		t.Errorf("Apply not idempotent: first=%v, second=%v", first, second)
	}
	if len(secondMasked) != 0 {
		t.Errorf("second pass should mask nothing, got %v", secondMasked)
	}
}

func TestApply_DoesNotMutateInput(t *testing.T) {
	mask := For(ResourceVehicleState, auth.RoleViewer)
	input := map[string]any{
		"speed": 65,
		"vin":   "7SAYGDET7TA613795",
	}
	before := map[string]any{
		"speed": 65,
		"vin":   "7SAYGDET7TA613795",
	}
	_, _ = Apply(input, mask)
	if !reflect.DeepEqual(input, before) {
		t.Errorf("Apply mutated input: now=%v, was=%v", input, before)
	}
}

func TestFor_FailClosed(t *testing.T) {
	tests := []struct {
		name     string
		resource ResourceType
		role     auth.Role
		wantSize int
	}{
		{
			name:     "unknown resource -> deny-all",
			resource: ResourceType("not_a_resource"),
			role:     auth.RoleOwner,
			wantSize: 0,
		},
		{
			name:     "unknown role -> deny-all",
			resource: ResourceVehicleState,
			role:     auth.Role("admin"),
			wantSize: 0,
		},
		{
			name:     "empty role sentinel -> deny-all",
			resource: ResourceVehicleState,
			role:     auth.Role(""),
			wantSize: 0,
		},
		{
			name:     "viewer + invite (intentionally absent) -> deny-all",
			resource: ResourceInvite,
			role:     auth.RoleViewer,
			wantSize: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := For(tt.resource, tt.role)
			if len(m.Allowed) != tt.wantSize {
				t.Errorf("Allowed size = %d, want %d", len(m.Allowed), tt.wantSize)
			}
		})
	}
}

// TestFor_VehicleState_BothRolesHaveLicensePlate pins the MYR-286 product
// decision: the owner-entered plate is visible to the OWNER **and** the
// VIEWER/rider. The plate exists so a rider can identify the correct car at
// pickup, which fails if only the owner can see it — so a change that moves
// this field back to owner-only must break a test, not ship quietly.
//
// Contrast TestFor_VehicleState_MYR279 below: `vin` stays owner-only.
func TestFor_VehicleState_BothRolesHaveLicensePlate(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleViewer} {
		m := For(ResourceVehicleState, role)
		if _, ok := m.Allowed["licensePlate"]; !ok {
			t.Errorf("%s mask must contain licensePlate (MYR-286: deliberately both roles)", role)
		}
	}
}

// TestFor_VehicleSummary_BothRolesHaveLicensePlate is the vehicles-list half of
// the same MYR-286 decision.
func TestFor_VehicleSummary_BothRolesHaveLicensePlate(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleViewer} {
		m := For(ResourceVehicleSummary, role)
		if _, ok := m.Allowed["licensePlate"]; !ok {
			t.Errorf("%s summary mask must contain licensePlate (MYR-286)", role)
		}
	}
}

// TestFor_VehicleSummary_ViewerIsOwnerPlusSharePermission pins the MYR-184
// shape of the vehicles-list viewer mask: it subtracts NOTHING from the owner
// list and adds exactly one field.
//
// `name` is the field this test exists for. It used to be subtracted, which
// made every viewer row invalid against vehicle-summary.schema.json (`name` is
// in that schema's `required` list) while every field-by-field assertion still
// passed. The product answer is that viewers SEE the nickname — the rider UI
// renders "{Owner}'s {Vehicle}" — so re-introducing the subtraction has to
// break a test rather than ship quietly. Any future subtraction here MUST be
// paired with making the field optional in the schema.
func TestFor_VehicleSummary_ViewerIsOwnerPlusSharePermission(t *testing.T) {
	owner := For(ResourceVehicleSummary, auth.RoleOwner)
	viewer := For(ResourceVehicleSummary, auth.RoleViewer)

	for field := range owner.Allowed {
		if _, ok := viewer.Allowed[field]; !ok {
			t.Errorf("%q is owner-visible but viewer-denied — the vehicles-list viewer mask "+
				"subtracts nothing from the owner list", field)
		}
	}
	for field := range viewer.Allowed {
		if field == "sharePermission" {
			continue
		}
		if _, ok := owner.Allowed[field]; !ok {
			t.Errorf("%q is viewer-visible but owner-denied; sharePermission is the only "+
				"viewer-only field on this resource", field)
		}
	}
	if _, ok := viewer.Allowed["name"]; !ok {
		t.Error("viewer summary mask is missing `name`, which vehicle-summary.schema.json " +
			"marks required — every viewer row would fail its own schema")
	}
	if _, ok := owner.Allowed["sharePermission"]; ok {
		t.Error("owner summary mask contains sharePermission — an owner is not on a tier")
	}
}

func TestFor_VehicleState_OwnerHasSpeed(t *testing.T) {
	owner := For(ResourceVehicleState, auth.RoleOwner)
	if _, ok := owner.Allowed["speed"]; !ok {
		t.Error("owner mask missing speed")
	}
}

func TestFor_VehicleState_ViewerRetainsSharedFields(t *testing.T) {
	viewer := For(ResourceVehicleState, auth.RoleViewer)
	if _, ok := viewer.Allowed["speed"]; !ok {
		t.Error("viewer mask missing speed")
	}
	// Viewer should retain GPS / nav per FR-5.1 sharing use case.
	for _, f := range []string{"latitude", "longitude", "destinationName", "navRouteCoordinates"} {
		if _, ok := viewer.Allowed[f]; !ok {
			t.Errorf("viewer mask missing %q (required for FR-5.1)", f)
		}
	}
}

func TestFor_DriveSummary_OwnerAndViewerIdentical(t *testing.T) {
	owner := For(ResourceDriveSummary, auth.RoleOwner)
	viewer := For(ResourceDriveSummary, auth.RoleViewer)
	if !reflect.DeepEqual(owner.Allowed, viewer.Allowed) {
		t.Errorf("drive_summary owner != viewer:\nowner=%v\nviewer=%v", owner.Allowed, viewer.Allowed)
	}
	// Spot-check a few canonical fields per rest-api.md §5.2.2.
	for _, f := range []string{"id", "startTime", "distanceMiles"} {
		if _, ok := owner.Allowed[f]; !ok {
			t.Errorf("drive_summary missing %q", f)
		}
	}
	// MYR-145: start/end Location + Address are part of the lean
	// projection now (rest-api.md §5.2.2). Owners and viewers see them
	// on the list per the FR-5.1 sharing use case.
	for _, f := range []string{"startAddress", "endAddress", "startLocation", "endLocation"} {
		if _, ok := owner.Allowed[f]; !ok {
			t.Errorf("drive_summary missing %q (MYR-145)", f)
		}
	}
}

func TestFor_DriveDetail_HasAddresses(t *testing.T) {
	owner := For(ResourceDriveDetail, auth.RoleOwner)
	for _, f := range []string{"startAddress", "endAddress", "startLocation", "endLocation"} {
		if _, ok := owner.Allowed[f]; !ok {
			t.Errorf("drive_detail missing %q (required by §5.2.3)", f)
		}
	}
}

func TestFor_DriveRoute_OnlyRoutePoints(t *testing.T) {
	mask := For(ResourceDriveRoute, auth.RoleOwner)
	if len(mask.Allowed) != 1 {
		t.Errorf("drive_route should expose exactly one field, got %d: %v", len(mask.Allowed), mask.Allowed)
	}
	if _, ok := mask.Allowed["routePoints"]; !ok {
		t.Error("drive_route missing routePoints")
	}
}

// contains is a tiny helper to avoid importing strings just for this.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestFor_VehicleState_MYR279 asserts the MYR-279 gating: the owner sees the full
// vin plus softwareVersion / trim; a viewer sees softwareVersion / trim (P0,
// non-identifying) but NOT the full vin (party-scoped, owner-only).
func TestFor_VehicleState_MYR279(t *testing.T) {
	owner := For(ResourceVehicleState, auth.RoleOwner)
	for _, f := range []string{"vin", "softwareVersion", "trim"} {
		if _, ok := owner.Allowed[f]; !ok {
			t.Errorf("owner mask must contain %q", f)
		}
	}

	viewer := For(ResourceVehicleState, auth.RoleViewer)
	if _, ok := viewer.Allowed["vin"]; ok {
		t.Error("viewer mask must NOT contain the full vin (owner-only, MYR-279)")
	}
	for _, f := range []string{"softwareVersion", "trim"} {
		if _, ok := viewer.Allowed[f]; !ok {
			t.Errorf("viewer mask should retain %q (P0, non-identifying)", f)
		}
	}
}
