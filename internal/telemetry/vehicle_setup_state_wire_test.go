package telemetry

import (
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// End-to-end wire coverage for MYR-491. The derivation itself is pinned by
// vehicle_setup_state_test.go; these assert the two things the derivation
// cannot: that the value survives the role masks on BOTH surfaces, and that the
// two surfaces produce the SAME object for the same car.

// awaitingKeySchedule is the MYR-503 car: linked minutes ago, Tesla refused the
// config for a missing virtual key, never streamed.
func awaitingKeySchedule(now time.Time) VehicleSetupSchedule {
	return VehicleSetupSchedule{
		Present:       true,
		LastOutcome:   setupOutcomeAwaitingKey,
		LastAttemptAt: now.Add(-4 * time.Minute),
	}
}

// The whole point of the field: an owner opening the app on a car that has not
// come up must be told what to finish, on the surface the card lives on.
func TestSnapshotCarriesSetupStateForBothRoles(t *testing.T) {
	now := time.Now()

	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			row := fixtureSnapshotRow(mediaFixtureUserID)
			row.Status = "offline"
			row.LastUpdated = now.Add(-4 * time.Minute)
			row.SetupSchedule = awaitingKeySchedule(now)

			body := decodeSnapshotBodyForRole(t, row, role)

			raw, present := body["setupState"]
			if !present {
				t.Fatalf("snapshot omits the setupState key entirely for role %s — "+
					"the mask allow-lists carry it for BOTH roles (MYR-437: a viewer's "+
					"picker must be able to say 'setting up' instead of 'offline')", role)
			}
			obj, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("setupState = %#v, want an object", raw)
			}
			if obj["state"] != SetupStateAwaitingVirtualKey {
				t.Errorf("setupState.state = %v, want %q", obj["state"], SetupStateAwaitingVirtualKey)
			}
			if _, ok := obj["since"].(string); !ok {
				t.Errorf("setupState.since = %#v, want an RFC 3339 string", obj["since"])
			}
		})
	}
}

// The sibling convention (serviceEstimatedEndAt, the cabin read-backs): the key
// is ALWAYS written, so "nothing to finish" is an explicit null rather than an
// omitted key. Absence must only ever mean "server predates MYR-491".
func TestSnapshotEmitsExplicitNullSetupStateForAHealthyCar(t *testing.T) {
	// fixtureSnapshotRow carries no schedule row — the ordinary case.
	body := decodeSnapshotBodyForRole(t, fixtureSnapshotRow(mediaFixtureUserID), auth.RoleOwner)

	got, present := body["setupState"]
	if !present {
		t.Fatal("setupState key missing; siblings keep the key with a null value")
	}
	if got != nil {
		t.Errorf("setupState = %#v, want explicit null — a car with no schedule row "+
			"makes NO CLAIM, and null must not be rendered as 'ready'", got)
	}
}

// setupState must be classified in exactly one arm of the vehicle_state mask,
// and that arm must be the viewer-visible one. The partition test in
// internal/mask proves "exactly one"; this proves "the right one" at the point
// where the choice has consequences.
func TestSetupStateIsNotOwnerOnly(t *testing.T) {
	for _, f := range mask.OwnerOnlyVehicleStateFields() {
		if f == "setupState" {
			t.Fatal("setupState is listed owner-only — a viewer whose shared car has " +
				"never streamed then sees only status 'offline', which is the MYR-437 " +
				"defect this field exists to fix")
		}
	}
}

// One derivation, two surfaces. If these ever disagree, an owner sees one story
// in their garage list and a different one on the car's detail sheet.
func TestCatalogRowAndSnapshotAgreeOnSetupState(t *testing.T) {
	now := time.Now()
	sched := awaitingKeySchedule(now)
	lastUpdated := now.Add(-4 * time.Minute)

	snapRow := fixtureSnapshotRow(mediaFixtureUserID)
	snapRow.Status = "offline"
	snapRow.LastUpdated = lastUpdated
	snapRow.SetupSchedule = sched
	fromSnapshot := buildSnapshotResponse(snapRow, now).SetupState

	catalogRow := VehicleCatalogRow{
		ID:            snapRow.ID,
		Status:        "offline",
		LastUpdated:   lastUpdated,
		SetupSchedule: sched,
	}
	fromCatalog := newVehicleSummary(&catalogRow, auth.RoleOwner, auth.ShareGrant{}, now).SetupState

	if fromSnapshot == nil || fromCatalog == nil {
		t.Fatalf("snapshot = %+v, catalog = %+v — both surfaces must carry the state",
			fromSnapshot, fromCatalog)
	}
	if *fromSnapshot != *fromCatalog {
		t.Errorf("snapshot = %+v, catalog = %+v — the two surfaces MUST call the same "+
			"derivation over the same inputs; a divergence here is a client seeing two "+
			"different answers for one car", *fromSnapshot, *fromCatalog)
	}
}

// The catalog row is the surface MYR-437's rider-side picker reads, and the
// viewer projection is where a mask omission would silently bite.
func TestViewerCatalogRowCarriesSetupState(t *testing.T) {
	now := time.Now()
	row := VehicleCatalogRow{
		ID:            "veh_shared_1",
		VIN:           "5YJSA1E26MF000491",
		Name:          "Sam's Model Y",
		Status:        "offline",
		LastUpdated:   now.Add(-4 * time.Minute),
		SetupSchedule: awaitingKeySchedule(now),
	}

	projected := viewerSummaryMap(row, auth.ShareGrant{AllowRides: true}, now)

	raw, present := projected["setupState"]
	if !present {
		t.Fatal("the viewer catalog row omits setupState — the rider-side picker reads " +
			"THIS row, and without the field it renders a never-streamed shared car as " +
			"permanently offline (MYR-437)")
	}
	obj, ok := raw.(*SetupState)
	if !ok || obj == nil {
		t.Fatalf("setupState = %#v, want a populated *SetupState", raw)
	}
	if obj.State != SetupStateAwaitingVirtualKey {
		t.Errorf("setupState.state = %q, want %q", obj.State, SetupStateAwaitingVirtualKey)
	}
}

// A nil *SetupState must reach the mask map as an UNTYPED nil. A typed
// (*SetupState)(nil) boxed in an `any` is not equal to nil, so any consumer
// doing `if v == nil` downstream would conclude a healthy car has a setup
// state — while JSON still encoded it as null, making the bug invisible in the
// response body.
func TestAbsentSetupStateIsAnUntypedNilInTheMaskMap(t *testing.T) {
	row := fixtureSnapshotRow(mediaFixtureUserID)
	m := buildSnapshotResponse(row, time.Now()).toMaskMap()

	got, present := m["setupState"]
	if !present {
		t.Fatal("setupState key missing from the mask map")
	}
	if got != nil {
		t.Errorf("setupState = %#v (type %T), want an untyped nil", got, got)
	}
}
