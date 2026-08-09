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

// TestMYR320DetailFieldsSplitOnTheVehiclesList pins the other half of the
// contract, which MYR-507 SPLIT.
//
// This test used to assert that BOTH trimLabel and fsdVersion were detail-sheet
// only, on the reasoning that "nobody renders a trim label in a catalog row" and
// that widening the most-expensive-per-byte response was not worth it. The first
// clause turned out to be false, and in the one place it mattered most: a VIEWER
// never fetches a /snapshot at all, so for a rider the catalog row is the ONLY
// surface on which a car can say what it is. With the trim withheld, a shared
// car's descriptor degraded to the raw colour token — a lock-screen Live
// Activity that read "UltraRed", and a review sheet whose model slot rendered
// the bare make "Tesla".
//
// So `trimLabel` is now on the catalog, in BOTH role allow-lists, classified
// with the identity siblings `model` / `year` / `color` it belongs to. The
// byte-cost argument survives intact and is simply outweighed: one short text
// column against a rider being unable to identify the car pulling up.
//
// `fsdVersion` STAYS OFF, and the split is the point of this test. It is a
// software designation, not an identity fact; no catalog row renders it, no
// descriptor ladder degrades without it, and nothing about MYR-507 argues for
// it. Keeping the assertion here means a later "well, we added trimLabel"
// cannot carry fsdVersion along by analogy.
func TestMYR320DetailFieldsSplitOnTheVehiclesList(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleViewer} {
		m := For(ResourceVehicleSummary, role)

		// MYR-507 — identity, and the viewer's only source of it.
		if !m.allows("trimLabel") {
			t.Errorf("%s: trimLabel denied on the vehicles-list — MYR-507 puts it on "+
				"the catalog for BOTH roles, because a rider never reads a snapshot", role)
		}

		// Still detail-sheet only.
		if m.allows("fsdVersion") {
			t.Errorf("%s: fsdVersion reached the vehicles-list — it is a software "+
				"designation with no catalog consumer, and MYR-507 deliberately did "+
				"NOT bring it along with trimLabel", role)
		}

		// The RAW BADGE CODE never reaches the catalog under either role: it is
		// not display-safe, and `trimLabel` exists precisely so that nothing has
		// to render it.
		if m.allows("trim") {
			t.Errorf("%s: trim (the raw badge code, e.g. \"p74d\") reached the "+
				"vehicles-list — only its display-safe sibling trimLabel may", role)
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
