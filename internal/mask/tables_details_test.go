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
// all four of those are viewer-visible.
//
// MYR-435 narrowed the viewer arm hard (media, cabin, and all controls state
// are gone) but deliberately did NOT touch these four: they describe the CAR's
// equipment and software, which is neither media, cabin, nor a control tile.
// Note the contrast with seatCoolingCapable, which IS an equipment fact and was
// still removed — because its only consumer was a control tile viewers no
// longer render. Equipment-ness alone is not the test; having a viewer-facing
// consumer is.
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

// TestVehicleStateRoleListsPartitionOwnerFields is the anti-rot invariant that
// replaced "the viewer list is owner minus vin" when MYR-435 made the viewer
// arm an explicit allow-list.
//
// The MYR-427 audit's finding was that a SUBTRACTION mask rots: every field
// added to the owner list reaches viewers by default, decided by whoever did
// not think about it. An explicit allow-list fixes that but introduces the
// opposite failure — a field added to the owner list and to NEITHER role list
// is simply invisible to viewers, silently, which is safe but undocumented.
//
// So the two lists must PARTITION the owner list exactly:
//
//	set(owner) == set(viewer) ⊎ set(ownerOnly)
//
// Adding a field to vehicleStateOwnerFields now fails this test until it is
// placed in one list or the other. That failure IS the classification
// conversation the audit found had never happened.
func TestVehicleStateRoleListsPartitionOwnerFields(t *testing.T) {
	owner := For(ResourceVehicleState, auth.RoleOwner)
	viewer := For(ResourceVehicleState, auth.RoleViewer)

	ownerOnly := make(map[string]struct{}, len(vehicleStateOwnerOnlyFields))
	for _, f := range vehicleStateOwnerOnlyFields {
		if _, dup := ownerOnly[f]; dup {
			t.Errorf("%q appears twice in vehicleStateOwnerOnlyFields", f)
		}
		ownerOnly[f] = struct{}{}
	}

	// Every owner field is classified exactly once.
	for field := range owner.Allowed {
		_, isViewer := viewer.Allowed[field]
		_, isOwnerOnly := ownerOnly[field]
		switch {
		case isViewer && isOwnerOnly:
			t.Errorf("%q is in BOTH the viewer allow-list and the owner-only list — "+
				"the classification is contradictory", field)
		case !isViewer && !isOwnerOnly:
			t.Errorf("%q is owner-visible but classified NEITHER viewer-visible nor "+
				"owner-only. Add it to vehicleStateViewerFields or to "+
				"vehicleStateOwnerOnlyFields — MYR-435 requires every field to be "+
				"consciously classified, not defaulted", field)
		}
	}

	// No viewer field is invented: a typo in the explicit allow-list would
	// otherwise sit there allowing a name nothing emits.
	for field := range viewer.Allowed {
		if !owner.allows(field) {
			t.Errorf("%q is viewer-visible but owner-denied, which cannot be right — "+
				"likely a typo in vehicleStateViewerFields", field)
		}
	}

	// No owner-only entry names a field that no longer exists upstream.
	for field := range ownerOnly {
		if !owner.allows(field) {
			t.Errorf("%q is listed owner-only but is not in the owner allow-list — "+
				"stale entry, remove it", field)
		}
	}
}
