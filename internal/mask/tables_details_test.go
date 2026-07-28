package mask

import (
	"testing"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// TestMYR320DetailFieldsAreBothRoles pins the MYR-320 mask decision at its
// enforcement point, so a later "tighten the viewer mask" change has to argue
// with it explicitly rather than sliding through.
//
// trimLabel and fsdVersion go to BOTH roles because they MATCH THEIR DIRECT
// SIBLINGS, which is how the choice was made rather than reasoning about them
// afresh: trimLabel is an equipment fact of the same tier as trim/model/year,
// fsdVersion a software designation of the same tier as softwareVersion — and
// all four of those are already viewer-visible, because the viewer list is
// owner-minus-`vin` and nothing else.
//
// The `vin` assertion is in the same test on purpose. It is the ONE owner-only
// snapshot field, precisely because it links to the physical car and its
// location history (data-classification.md §1.3, §2.1), and none of these four
// does. Losing that contrast is how a field ends up owner-only by analogy
// instead of by argument.
func TestMYR320DetailFieldsAreBothRoles(t *testing.T) {
	detailFields := []string{"trimLabel", "fsdVersion", "trim", "softwareVersion"}

	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleViewer} {
		m := For(ResourceVehicleState, role)
		for _, field := range detailFields {
			if !m.allows(field) {
				t.Errorf("%s: %s denied on the snapshot, want allowed", role, field)
			}
		}
	}

	ownerMask := For(ResourceVehicleState, auth.RoleOwner)
	viewerMask := For(ResourceVehicleState, auth.RoleViewer)
	if !ownerMask.allows("vin") {
		t.Error("owner: vin denied, want allowed")
	}
	if viewerMask.allows("vin") {
		t.Error("viewer: vin ALLOWED — it must stay the one owner-only snapshot field")
	}
}

// TestMYR320DetailFieldsAreNotOnTheVehiclesList pins the other half of the
// contract: both fields are deliberately DETAIL-SHEET ONLY. The vehicles-list is
// a thin catalog whose rows nobody renders a trim label or an FSD version in,
// and adding them there would widen every row of a response that already costs
// the most per byte.
func TestMYR320DetailFieldsAreNotOnTheVehiclesList(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleViewer} {
		m := For(ResourceVehicleSummary, role)
		for _, field := range []string{"trimLabel", "fsdVersion"} {
			if m.allows(field) {
				t.Errorf("%s: %s reached the vehicles-list — MYR-320 is detail-sheet-only",
					role, field)
			}
		}
	}
}

// The viewer list is built by EXCLUSION from the owner list (removeField), which
// is what stops the two drifting apart every time a field is added. This asserts
// the invariant directly rather than trusting the construction to stay that way.
func TestVehicleStateViewerIsOwnerMinusVINOnly(t *testing.T) {
	owner := For(ResourceVehicleState, auth.RoleOwner)
	viewer := For(ResourceVehicleState, auth.RoleViewer)

	for field := range owner.Allowed {
		if field == "vin" {
			continue
		}
		if !viewer.allows(field) {
			t.Errorf("%s is owner-visible but viewer-denied — the viewer list must be "+
				"owner minus `vin` and NOTHING else", field)
		}
	}
	for field := range viewer.Allowed {
		if !owner.allows(field) {
			t.Errorf("%s is viewer-visible but owner-denied, which cannot be right", field)
		}
	}
}
