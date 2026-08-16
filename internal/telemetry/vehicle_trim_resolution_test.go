package telemetry

import (
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// MYR-578 — the WIRING arms. internal/trim's own suite proves the resolver;
// these prove both emission surfaces actually consult it, over the client's own
// production case: a Model X streaming the raw badge `p100d` with an EMPTY
// stored label must emit `trimLabel: "Plaid"` — on the catalog AND the
// snapshot, identically.

func TestCatalogTrimLabelIsResolvedNotEchoed(t *testing.T) {
	badge := "p100d"
	row := VehicleCatalogRow{
		ID:    "veh_x",
		VIN:   "7SAXCDE6TF1234567", // digit 8 = '6', the tri-motor Plaid
		Model: "Model X",
		Year:  2026,
		Trim:  &badge,
		// TrimLabel deliberately nil — Tesla never supplied one for this car.
		LastUpdated: time.Now(),
	}
	got := newVehicleSummary(&row, auth.RoleOwner, auth.ShareGrant{}, time.Now()).TrimLabel
	if got == nil || *got != "Plaid" {
		t.Fatalf("catalog trimLabel = %v, want Plaid", got)
	}

	// The junk stored label falls through to the badge instead of leaking.
	junk := VehicleCatalogRow{
		ID: "veh_y", VIN: "7SAYGDEDTF1234567", Model: "Model Y", Year: 2026,
		TrimLabel: strPtr("Base2024"), Trim: strPtr("62"),
		LastUpdated: time.Now(),
	}
	got = newVehicleSummary(&junk, auth.RoleOwner, auth.ShareGrant{}, time.Now()).TrimLabel
	if got == nil || *got != "Standard" {
		t.Fatalf("catalog trimLabel = %v, want Standard (Base2024 must never leak)", got)
	}

	// A car with nothing honest to say emits an explicit null, never "".
	bare := VehicleCatalogRow{
		ID: "veh_3", VIN: "5YJ3E1EATF1234567", Model: "Model 3", Year: 2026,
		LastUpdated: time.Now(),
	}
	if got := newVehicleSummary(&bare, auth.RoleOwner, auth.ShareGrant{}, time.Now()).TrimLabel; got != nil {
		t.Fatalf("catalog trimLabel = %q, want nil (single motor is not a coin toss)", *got)
	}
}

func TestSnapshotTrimLabelRunsTheSameResolver(t *testing.T) {
	badge := "p100d"
	row := VehicleSnapshotRow{
		ID:    "veh_x",
		VIN:   "7SAXCDE6TF1234567",
		Model: "Model X",
		Year:  2026,
		Trim:  &badge,
	}
	resp := buildSnapshotResponse(row, time.Now())
	if resp.TrimLabel == nil || *resp.TrimLabel != "Plaid" {
		t.Fatalf("snapshot trimLabel = %v, want Plaid", resp.TrimLabel)
	}
	// The RAW badge stays raw — the resolver replaces nothing Tesla said.
	if resp.Trim == nil || *resp.Trim != "p100d" {
		t.Fatalf("snapshot trim = %v, want the raw p100d untouched", resp.Trim)
	}
	// Tesla's own display-safe label still outranks everything.
	labelled := VehicleSnapshotRow{
		ID: "veh_l", VIN: "7SAYGDETTF1234567", Model: "Model Y", Year: 2026,
		Trim: strPtr("p74d"), TrimLabel: strPtr("Performance"),
	}
	if got := buildSnapshotResponse(labelled, time.Now()).TrimLabel; got == nil || *got != "Performance" {
		t.Fatalf("snapshot trimLabel = %v, want Tesla's own Performance", got)
	}
}
