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

// TestBuildSnapshotResponse_ClimateMode covers MYR-274: the nullable hvacAutoMode
// (string) / hvacAcEnabled (bool) read-backs backing the owner Auto/Cool/Heat
// segment are mapped onto the snapshot response and surface in the mask map under
// their wire names.
func TestBuildSnapshotResponse_ClimateMode(t *testing.T) {
	mode := "Override"
	acOn := true
	row := VehicleSnapshotRow{
		ID:            "veh_1",
		HvacAutoMode:  &mode,
		HvacAcEnabled: &acOn,
	}
	resp := buildSnapshotResponse(row)
	if resp.HvacAutoMode == nil || *resp.HvacAutoMode != mode {
		t.Errorf("HvacAutoMode = %v, want %q", resp.HvacAutoMode, mode)
	}
	if resp.HvacAcEnabled == nil || !*resp.HvacAcEnabled {
		t.Errorf("HvacAcEnabled = %v, want true", resp.HvacAcEnabled)
	}

	m := resp.toMaskMap()
	if m["hvacAutoMode"] != mode {
		t.Errorf("mask map hvacAutoMode = %v, want %q", m["hvacAutoMode"], mode)
	}
	if m["hvacAcEnabled"] != true {
		t.Errorf("mask map hvacAcEnabled = %v, want true", m["hvacAcEnabled"])
	}
}

// TestBuildSnapshotResponse_NilClimateModeIsNull asserts a never-read climate mode
// surfaces as JSON null (nil in the mask map), never a fabricated Auto/Cool/Heat.
func TestBuildSnapshotResponse_NilClimateModeIsNull(t *testing.T) {
	m := buildSnapshotResponse(VehicleSnapshotRow{ID: "veh_1"}).toMaskMap()
	if m["hvacAutoMode"] != nil {
		t.Errorf("nil hvacAutoMode should map to nil, got %v", m["hvacAutoMode"])
	}
	if m["hvacAcEnabled"] != nil {
		t.Errorf("nil hvacAcEnabled should map to nil, got %v", m["hvacAcEnabled"])
	}
}

// TestBuildSnapshotResponse_SeatVentMedia covers MYR-298: the persisted
// seatVentEnabled / mediaPlaybackStatus read-backs are mapped onto the snapshot
// response and surface in the mask map under their live WS wire names.
func TestBuildSnapshotResponse_SeatVentMedia(t *testing.T) {
	vent := true
	media := "Playing"
	row := VehicleSnapshotRow{
		ID:                  "veh_1",
		SeatVentEnabled:     &vent,
		MediaPlaybackStatus: &media,
	}

	resp := buildSnapshotResponse(row)
	if resp.SeatVentEnabled == nil || !*resp.SeatVentEnabled {
		t.Errorf("SeatVentEnabled = %v, want true", resp.SeatVentEnabled)
	}
	if resp.MediaPlaybackStatus == nil || *resp.MediaPlaybackStatus != media {
		t.Errorf("MediaPlaybackStatus = %v, want %q", resp.MediaPlaybackStatus, media)
	}

	m := resp.toMaskMap()
	if m["seatVentEnabled"] != true {
		t.Errorf("mask map seatVentEnabled = %v, want true", m["seatVentEnabled"])
	}
	if m["mediaPlaybackStatus"] != media {
		t.Errorf("mask map mediaPlaybackStatus = %v, want %q", m["mediaPlaybackStatus"], media)
	}
}

// TestBuildSnapshotResponse_NilSeatVentMediaIsNull asserts a never-read seat-vent
// or media status surfaces as JSON null (nil in the mask map) — the honest-unknown
// contract, never a fabricated false/"Stopped". Matches the MYR-274 siblings.
func TestBuildSnapshotResponse_NilSeatVentMediaIsNull(t *testing.T) {
	m := buildSnapshotResponse(VehicleSnapshotRow{ID: "veh_1"}).toMaskMap()
	if m["seatVentEnabled"] != nil {
		t.Errorf("nil seatVentEnabled should map to nil, got %v", m["seatVentEnabled"])
	}
	if m["mediaPlaybackStatus"] != nil {
		t.Errorf("nil mediaPlaybackStatus should map to nil, got %v", m["mediaPlaybackStatus"])
	}
}
