package telemetry

import "testing"

// TestBuildSnapshotResponse_VehicleDetails covers MYR-279: the full vin plus the
// nullable softwareVersion / trim read-backs are mapped onto the snapshot
// response and surface in the mask map under their wire names.
func TestBuildSnapshotResponse_VehicleDetails(t *testing.T) {
	ver := "2026.20.1"
	trim := "Performance"
	row := VehicleSnapshotRow{
		ID:              "veh_1",
		VIN:             "7SAYGDET7TA613795",
		SoftwareVersion: &ver,
		Trim:            &trim,
	}
	resp := buildSnapshotResponse(row)
	if resp.VIN != "7SAYGDET7TA613795" {
		t.Errorf("VIN = %q, want full vin", resp.VIN)
	}
	if resp.SoftwareVersion == nil || *resp.SoftwareVersion != ver {
		t.Errorf("SoftwareVersion = %v, want %q", resp.SoftwareVersion, ver)
	}
	if resp.Trim == nil || *resp.Trim != trim {
		t.Errorf("Trim = %v, want %q", resp.Trim, trim)
	}

	m := resp.toMaskMap()
	if m["vin"] != "7SAYGDET7TA613795" {
		t.Errorf("mask map vin = %v", m["vin"])
	}
	if m["softwareVersion"] != ver {
		t.Errorf("mask map softwareVersion = %v", m["softwareVersion"])
	}
	if m["trim"] != trim {
		t.Errorf("mask map trim = %v", m["trim"])
	}
}

// TestBuildSnapshotResponse_NilDetailsAreNull asserts a never-read software
// version / trim surfaces as JSON null (nil in the mask map), never a fabricated
// value.
func TestBuildSnapshotResponse_NilDetailsAreNull(t *testing.T) {
	m := buildSnapshotResponse(VehicleSnapshotRow{ID: "veh_1", VIN: "7SAYGDET7TA613795"}).toMaskMap()
	if m["softwareVersion"] != nil {
		t.Errorf("nil softwareVersion should map to nil, got %v", m["softwareVersion"])
	}
	if m["trim"] != nil {
		t.Errorf("nil trim should map to nil, got %v", m["trim"])
	}
	// vin is always present (non-nullable string on the snapshot).
	if m["vin"] != "7SAYGDET7TA613795" {
		t.Errorf("vin should always be present, got %v", m["vin"])
	}
}
