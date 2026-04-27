package mask

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
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

func TestApply_PartialMask_StripsLicensePlate(t *testing.T) {
	// Viewer projection of a vehicle_state payload that happens to
	// carry a licensePlate (forward-looking). The viewer allow-list
	// excludes licensePlate; verify it is stripped and reported in
	// fieldsMasked.
	mask := For(ResourceVehicleState, auth.RoleViewer)
	input := map[string]any{
		"speed":        65,
		"chargeLevel":  82,
		"licensePlate": "ABC-123",
	}

	out, masked := Apply(input, mask)

	if _, present := out["licensePlate"]; present {
		t.Error("viewer projection still contains licensePlate")
	}
	if out["speed"] != 65 {
		t.Errorf("speed lost: got %v", out["speed"])
	}
	if out["chargeLevel"] != 82 {
		t.Errorf("chargeLevel lost: got %v", out["chargeLevel"])
	}
	if !reflect.DeepEqual(masked, []string{"licensePlate"}) {
		t.Errorf("fieldsMasked = %v, want [licensePlate]", masked)
	}
}

func TestApply_AbsentNotNulled_OnJSONSerialization(t *testing.T) {
	// rest-api.md §5.1 requires denied fields to be ABSENT from the
	// JSON, not emitted with a null value. Verify by round-tripping
	// the projected map and inspecting raw JSON for the key name.
	mask := For(ResourceVehicleState, auth.RoleViewer)
	input := map[string]any{
		"speed":        65,
		"licensePlate": "ABC-123",
	}

	out, _ := Apply(input, mask)

	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(encoded); !contains(got, "speed") {
		t.Errorf("expected JSON to contain speed: %s", got)
	}
	if got := string(encoded); contains(got, "licensePlate") {
		t.Errorf("JSON must NOT contain licensePlate (absent, not nulled): %s", got)
	}
	if got := string(encoded); contains(got, "null") {
		t.Errorf("JSON must NOT contain null for stripped key: %s", got)
	}
}

func TestApply_Idempotent(t *testing.T) {
	mask := For(ResourceVehicleState, auth.RoleViewer)
	input := map[string]any{
		"speed":        65,
		"chargeLevel":  82,
		"licensePlate": "ABC-123",
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
		"speed":        65,
		"licensePlate": "ABC-123",
	}
	before := map[string]any{
		"speed":        65,
		"licensePlate": "ABC-123",
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

func TestFor_VehicleState_OwnerHasLicensePlate(t *testing.T) {
	owner := For(ResourceVehicleState, auth.RoleOwner)
	if _, ok := owner.Allowed["licensePlate"]; !ok {
		t.Error("owner mask must contain licensePlate (forward-looking)")
	}
	if _, ok := owner.Allowed["speed"]; !ok {
		t.Error("owner mask missing speed")
	}
}

func TestFor_VehicleState_ViewerLacksLicensePlate(t *testing.T) {
	viewer := For(ResourceVehicleState, auth.RoleViewer)
	if _, ok := viewer.Allowed["licensePlate"]; ok {
		t.Error("viewer mask must NOT contain licensePlate")
	}
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
	// And explicitly check the deliberately-omitted detail fields.
	for _, f := range []string{"startAddress", "endAddress", "startLocation", "endLocation"} {
		if _, ok := owner.Allowed[f]; ok {
			t.Errorf("drive_summary MUST NOT include %q (drive_detail field)", f)
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
