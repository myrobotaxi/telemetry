package telemetry

import (
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// MYR-581 — `VehicleSummary.ownerFirstName` on §7.0.
//
// The wire arms these tests own: the key is ALWAYS present, a resolved name
// reaches it verbatim, an unresolved one is an explicit null, and BOTH role masks
// carry it. The FIRST-TOKEN REDUCTION and the empty-string collapse happen in the
// store (internal/store/owner_name.go, reusing MYR-229's `firstNameToken`) and are
// covered by TestOwnerFirstNameToken there — this file's job is to prove that
// whatever the store resolved survives the projection and the mask unmangled,
// including the nil.

// catalogRowWithOwner is the owner-side catalog row with the owner name set (or
// cleared, for a nil argument).
func catalogRowWithOwner(ownerFirst *string) VehicleCatalogRow {
	row := sharedCatalogRow(true).VehicleCatalogRow
	row.OwnerFirstName = ownerFirst
	return row
}

func strptr(s string) *string { return &s }

// TestVehicleSummary_OwnerFirstNameArms is the table over what the store can hand
// the projection. The `null` arm is the load-bearing one: the key must be PRESENT
// and null rather than missing, because an absent key on this contract means "a
// server predating v0.37.0" and a client would be right to ignore it.
func TestVehicleSummary_OwnerFirstNameArms(t *testing.T) {
	tests := []struct {
		name  string
		owner *string
		want  any
	}{
		{
			name:  "a resolved first name reaches the wire verbatim",
			owner: strptr("Amruth"),
			want:  "Amruth",
		},
		{
			name:  "a nameless owner is an explicit null, not a missing key",
			owner: nil,
			want:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := catalogRowWithOwner(tc.owner)
			summary := newVehicleSummary(&row, auth.RoleOwner, auth.ShareGrant{}, time.Now())

			if tc.owner == nil {
				if summary.OwnerFirstName != nil {
					t.Fatalf("OwnerFirstName = %q, want nil", *summary.OwnerFirstName)
				}
			} else if summary.OwnerFirstName == nil || *summary.OwnerFirstName != *tc.owner {
				t.Fatalf("OwnerFirstName = %v, want %q", summary.OwnerFirstName, *tc.owner)
			}

			m := summary.toMaskMap()
			got, present := m["ownerFirstName"]
			if !present {
				t.Fatal("ownerFirstName must ALWAYS be present in the mask map — " +
					"an absent key means 'server predates v0.37.0', not 'no name'")
			}
			if got != tc.want {
				t.Errorf("mask-map ownerFirstName = %v (%T), want %v", got, got, tc.want)
			}
			// The nil must be an UNTYPED nil, not a typed (*string)(nil): the
			// latter marshals to `null` but is not `== nil` to any predicate
			// reading the map, which is the trap `derefOrNil` exists to avoid.
			if tc.want == nil && got != nil {
				t.Errorf("ownerFirstName nil is typed (%T) — mask predicates and tests will not see it as nil", got)
			}
		})
	}
}

// TestVehicleSummary_OwnerFirstNameSurvivesBothMasks is the RBAC arm. Both role
// allow-lists carry the field, and the VIEWER is the party it exists for — a
// viewer row that lost it would put the platform straight back to rendering the
// make where a person's name belongs.
func TestVehicleSummary_OwnerFirstNameSurvivesBothMasks(t *testing.T) {
	for _, tc := range []struct {
		role  auth.Role
		grant auth.ShareGrant
	}{
		{auth.RoleOwner, auth.ShareGrant{}},
		{auth.RoleViewer, auth.ShareGrant{AllowRides: true}},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			row := catalogRowWithOwner(strptr("Amruth"))
			summary := newVehicleSummary(&row, tc.role, tc.grant, time.Now())
			projected, _ := mask.Apply(summary.toMaskMap(), mask.For(mask.ResourceVehicleSummary, tc.role))

			got, ok := projected["ownerFirstName"]
			if !ok {
				t.Fatalf("the %s mask dropped ownerFirstName — it is in both allow-lists", tc.role)
			}
			if got != "Amruth" {
				t.Errorf("ownerFirstName = %v, want %q", got, "Amruth")
			}
		})
	}
}

// TestViewerSummaryMap_CarriesOwnerFirstName pins the field on the ACTUAL viewer
// projection — the one function both the §7.0 viewer merge and the redeem response
// go through, so this covers both surfaces at once.
//
// Separate from the mask test above because it exercises the real call path rather
// than a hand-assembled projection: a field can be in the allow-list and still be
// missing from the map the viewer path builds.
func TestViewerSummaryMap_CarriesOwnerFirstName(t *testing.T) {
	row := catalogRowWithOwner(strptr("Amruth"))
	projected := viewerSummaryMap(row, auth.ShareGrant{AllowRides: true}, time.Now())

	got, ok := projected["ownerFirstName"]
	if !ok {
		t.Fatal("the viewer projection omits ownerFirstName — the viewer is the role it exists for")
	}
	if got != "Amruth" {
		t.Errorf("ownerFirstName = %v, want %q", got, "Amruth")
	}

	// And the nameless viewer row keeps the key, as an explicit null.
	nameless := viewerSummaryMap(catalogRowWithOwner(nil), auth.ShareGrant{AllowRides: true}, time.Now())
	v, ok := nameless["ownerFirstName"]
	if !ok {
		t.Fatal("a nameless viewer row must still carry the key")
	}
	if v != nil {
		t.Errorf("nameless viewer row ownerFirstName = %v, want nil", v)
	}
}
