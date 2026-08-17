package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// --- Test doubles for VehicleSnapshotReader ---

type stubVehicleSnapshotReader struct {
	row VehicleSnapshotRow
	err error
	// calls counts GetByID invocations — MYR-313 asserts that an accept exempt
	// from the availability gate never asks the question at all.
	calls int
}

func (s *stubVehicleSnapshotReader) GetByID(_ context.Context, _ string) (VehicleSnapshotRow, error) {
	s.calls++
	if s.err != nil {
		return VehicleSnapshotRow{}, s.err
	}
	return s.row, nil
}

// availableSnapshotRow is the MINIMAL row for a test that does not care about
// the vehicle at all — every field zero EXCEPT the one whose zero value lies.
//
// MYR-342 made `VehicleSnapshotRow{}` an unsafe default in the ride tests:
// RideShareEnabled false reads as "the owner has PAUSED this car", so a bare
// zero row silently turned every accept/create test into a test of the pause
// gate. The store can never produce that combination — its
// COALESCE(gcs.ride_share_enabled, TRUE) makes a car with no control-state row
// enabled — so the zero row was never a state the server sees. This helper is
// the honest stand-in.
//
// MYR-581 added the SECOND field whose zero value lies, for the same structural
// reason: `OwnerNamed` false reads as "this car's owner has no name at all", so a
// bare zero row would quietly turn every accept/create test into a test of the
// nameless-owner gate. Both are spelled out here so "a car a rider may actually
// request" has exactly one definition in the test suite.
func availableSnapshotRow() VehicleSnapshotRow {
	return VehicleSnapshotRow{RideShareEnabled: true, OwnerNamed: true}
}

// fixtureSnapshotRowID is the canonical vehicleId used by every
// snapshot-handler test fixture. Pinning it here keeps the unparam
// linter from flagging fixtureSnapshotRow's vehicleID param as
// constant — every caller passes the same value, by design.
const fixtureSnapshotRowID = "clxyz1234567890abcdef"

// fixtureSnapshotRow returns a populated snapshot row owned by
// ownerID. The vehicleId is pinned to fixtureSnapshotRowID. The
// values mirror the rest-api.md §7.1 example body where they overlap
// with the schema.
func fixtureSnapshotRow(ownerID string) VehicleSnapshotRow {
	gear := "P"
	chargeState := "Disconnected"
	return VehicleSnapshotRow{
		ID: fixtureSnapshotRowID,
		// MYR-342: the fixture is an ORDINARY, un-paused car, matching what the
		// store's COALESCE(gcs.ride_share_enabled, TRUE) produces for a vehicle
		// nobody has touched. Spelled out rather than left to the zero value
		// because for THIS field the Go default points the wrong way — a false
		// here means PAUSED, and every ride test would silently start asserting
		// against a withdrawn vehicle.
		RideShareEnabled: true,
		// MYR-581: and the fixture's owner HAS a name, for the same
		// zero-value-points-the-wrong-way reason. A false here means the store
		// found no name on any of the three identity rungs, which would refuse
		// every ride request in the suite.
		OwnerNamed:         true,
		UserID:             ownerID,
		VIN:                "5YJ3E1EA1PF000001",
		Name:               "Stumpy",
		Model:              "Model 3",
		Year:               2024,
		Color:              "Midnight Silver Metallic",
		Status:             "parked",
		ChargeLevel:        78,
		EstimatedRange:     245,
		ChargeState:        &chargeState,
		Speed:              0,
		GearPosition:       &gear,
		Heading:            180,
		Latitude:           10.0,
		Longitude:          20.0,
		LocationName:       "Home",
		LocationAddress:    "123 Market St, San Francisco, CA",
		InteriorTemp:       68,
		ExteriorTemp:       55,
		OdometerMiles:      12458,
		FsdMilesSinceReset: 412.7,
		LastUpdated:        time.Date(2026, 5, 31, 18, 22, 1, 0, time.UTC),
	}
}

// --- Tests ---

func TestVehicleSnapshotHandler_ServeHTTP(t *testing.T) {
	const (
		vehicleID = "clxyz1234567890abcdef"
		userID    = "user-123"
		authToken = "valid-token"
	)

	tests := []struct {
		name           string
		vehicleIDPath  string
		authHeader     string
		tokenValidator *stubTokenValidator
		reader         *stubVehicleSnapshotReader
		wantStatus     int
		wantErrCode    wserrors.ErrorCode
		wantErrSubstr  string
	}{
		{
			name:           "missing Authorization header",
			vehicleIDPath:  vehicleID,
			authHeader:     "",
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			wantStatus:     http.StatusUnauthorized,
			wantErrCode:    wserrors.ErrCodeAuthFailed,
			wantErrSubstr:  "missing Authorization header",
		},
		{
			name:           "invalid token returns 401",
			vehicleIDPath:  vehicleID,
			authHeader:     "Bearer bad",
			tokenValidator: &stubTokenValidator{err: errors.New("token expired")},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			wantStatus:     http.StatusUnauthorized,
			wantErrCode:    wserrors.ErrCodeAuthFailed,
			wantErrSubstr:  "invalid or expired token",
		},
		{
			name:           "unknown vehicleId returns 404",
			vehicleIDPath:  vehicleID,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{err: fmt.Errorf("VehicleRepo.GetByID: %w", sdk.ErrNotFound)},
			wantStatus:     http.StatusNotFound,
			wantErrCode:    wserrors.ErrCodeNotFound,
			wantErrSubstr:  "vehicle not found",
		},
		{
			name:           "non-owner returns 403",
			vehicleIDPath:  vehicleID,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow("other-user")},
			wantStatus:     http.StatusForbidden,
			wantErrCode:    wserrors.ErrCodeVehicleNotOwned,
			wantErrSubstr:  "you do not have access to this vehicle",
		},
		{
			name:           "store internal error returns 500",
			vehicleIDPath:  vehicleID,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{err: errors.New("db down")},
			wantStatus:     http.StatusInternalServerError,
			wantErrCode:    wserrors.ErrCodeInternalError,
			wantErrSubstr:  "internal error",
		},
		{
			name:           "owner happy path returns 200",
			vehicleIDPath:  vehicleID,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			wantStatus:     http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewVehicleSnapshotHandler(tt.tokenValidator, tt.reader, discardLogger())

			mux := http.NewServeMux()
			mux.Handle("GET /api/vehicles/{vehicleId}/snapshot", h)

			req := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				"/api/vehicles/"+tt.vehicleIDPath+"/snapshot",
				nil,
			)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d. Body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantErrCode != "" {
				var env wserrors.ErrorEnvelope
				if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
					t.Fatalf("decode error envelope: %v", err)
				}
				if env.Error.Code != tt.wantErrCode {
					t.Errorf("error.code: got %q, want %q", env.Error.Code, tt.wantErrCode)
				}
				if tt.wantErrSubstr != "" && !strings.Contains(env.Error.Message, tt.wantErrSubstr) {
					t.Errorf("error.message: got %q, want substring %q", env.Error.Message, tt.wantErrSubstr)
				}
				return
			}

			var body map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body["vehicleId"] != vehicleID {
				t.Errorf("vehicleId: got %v, want %q", body["vehicleId"], vehicleID)
			}
			if body["name"] != "Stumpy" {
				t.Errorf("name: got %v, want Stumpy", body["name"])
			}
			if body["model"] != "Model 3" {
				t.Errorf("model: got %v, want Model 3", body["model"])
			}
			if body["chargeLevel"] != float64(78) {
				t.Errorf("chargeLevel: got %v, want 78", body["chargeLevel"])
			}
			if body["lastUpdated"] != "2026-05-31T18:22:01Z" {
				t.Errorf("lastUpdated: got %v, want 2026-05-31T18:22:01Z", body["lastUpdated"])
			}
		})
	}
}

// TestVehicleSnapshotHandler_OwnerMaskIsIdentity confirms that with
// the role plumbing wired, an owner caller still sees every
// VehicleState wire field the mask allow-list permits. Mirrors the
// vehicles list handler's owner-projection test.
func TestVehicleSnapshotHandler_OwnerMaskIsIdentity(t *testing.T) {
	const (
		vehicleID = "clxyz1234567890abcdef"
		userID    = "user-1"
	)

	h := NewVehicleSnapshotHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
		discardLogger(),
		WithSnapshotRoleResolver(&stubRoleResolver{role: "owner"}),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicles/{vehicleId}/snapshot", h)

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/api/vehicles/"+vehicleID+"/snapshot",
		nil,
	)
	req.Header.Set("Authorization", "Bearer t")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Every required (non-null) snapshot field from rest-api.md §7.1
	// must be present after owner projection.
	requiredKeys := []string{
		"vehicleId", "name", "model", "year", "color", "status",
		"chargeLevel", "estimatedRange", "speed", "heading",
		"latitude", "longitude", "locationName", "locationAddress",
		"interiorTemp", "exteriorTemp", "odometerMiles",
		"fsdMilesSinceReset", "lastUpdated",
	}
	for _, k := range requiredKeys {
		if _, ok := body[k]; !ok {
			t.Errorf("owner projection: missing required field %q", k)
		}
	}
}

// TestVehicleSnapshotHandler_NoLatLngInLogs is a smoke check that an
// owner request body still encodes the lat/lng for the wire (so the
// SDK can render the snapshot) — paired with the contract-guard
// expectation that we never log P1 values. We do not log lat/lng in
// any handler path; this test pins the wire shape, not the log path.
func TestVehicleSnapshotHandler_LatLngOnWire(t *testing.T) {
	const (
		vehicleID = "clxyz1234567890abcdef"
		userID    = "user-1"
	)

	h := NewVehicleSnapshotHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
		discardLogger(),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicles/{vehicleId}/snapshot", h)

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/api/vehicles/"+vehicleID+"/snapshot",
		nil,
	)
	req.Header.Set("Authorization", "Bearer t")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["latitude"] != 10.0 {
		t.Errorf("latitude: got %v, want 10.0", body["latitude"])
	}
	if body["longitude"] != 20.0 {
		t.Errorf("longitude: got %v, want 20.0", body["longitude"])
	}
}

// TestVehicleSnapshotHandler_SeatVentMediaOnWire is the MYR-298 handler-level
// proof: seatVentEnabled and mediaPlaybackStatus were contracted vehicle_update
// fields that never reached the DB-backed /snapshot, so a client that missed the
// live WS frame could not learn them. They now travel end-to-end through the
// owner projection.
//
// Both sub-cases assert the SIBLING absent-vs-null behaviour exactly (verified
// against hvacAutoMode/hvacAcEnabled, MYR-274): toMaskMap always writes the key,
// so the owner wire ALWAYS carries it — a never-read value is an explicit JSON
// null (honest-unknown), never an omitted key and never a fabricated
// false/"Stopped".
func TestVehicleSnapshotHandler_SeatVentMediaOnWire(t *testing.T) {
	const userID = "user-1"

	decodeBody := func(t *testing.T, row VehicleSnapshotRow) map[string]any {
		t.Helper()
		h := NewVehicleSnapshotHandler(
			&stubTokenValidator{userID: userID},
			&stubVehicleSnapshotReader{row: row},
			discardLogger(),
			WithSnapshotRoleResolver(&stubRoleResolver{role: "owner"}),
		)
		mux := http.NewServeMux()
		mux.Handle("GET /api/vehicles/{vehicleId}/snapshot", h)

		req := httptest.NewRequestWithContext(
			context.Background(),
			http.MethodGet,
			"/api/vehicles/"+fixtureSnapshotRowID+"/snapshot",
			nil,
		)
		req.Header.Set("Authorization", "Bearer t")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200. Body: %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}

	t.Run("persisted values reach the owner wire", func(t *testing.T) {
		row := fixtureSnapshotRow(userID)
		vent := true
		media := "Playing"
		row.SeatVentEnabled = &vent
		row.MediaPlaybackStatus = &media

		body := decodeBody(t, row)
		if got, ok := body["seatVentEnabled"]; !ok || got != true {
			t.Errorf("seatVentEnabled: got %v (present=%t), want true", got, ok)
		}
		if got, ok := body["mediaPlaybackStatus"]; !ok || got != media {
			t.Errorf("mediaPlaybackStatus: got %v (present=%t), want %q", got, ok, media)
		}
	})

	t.Run("never-seen values are explicit null, not fabricated", func(t *testing.T) {
		// fixtureSnapshotRow leaves both nil — the "never streamed, never
		// persisted" car. Same shape the MYR-274 siblings produce.
		body := decodeBody(t, fixtureSnapshotRow(userID))

		for _, key := range []string{"seatVentEnabled", "mediaPlaybackStatus"} {
			got, ok := body[key]
			if !ok {
				t.Errorf("%s: key missing from owner projection; siblings keep the key with a null value", key)
				continue
			}
			if got != nil {
				t.Errorf("%s: got %v, want explicit null (honest-unknown)", key, got)
			}
		}
	})
}

// TestVehicleSnapshotHandler_SeatCoolerPresenceOnWire is the MYR-299 handler-level
// proof that the seat-cooler CAPABILITY signal survives the wire.
//
// The client derives "this car has ventilated seats" from the PRESENCE of
// seatCoolerLeft/seatCoolerRight, not from their value: a car without cooled
// seats never emits protos 237/238, while a car with them emits values that
// include 0 (present-but-off). That inference only holds if the owner projection
// distinguishes the two cases faithfully:
//
//   - a persisted 0 MUST arrive as the JSON number 0 — not omitted, not null, or
//     a vented car with both seats off reads as "no cooled seats" and the owner is
//     locked out of Cool (the client-reported defect);
//   - a never-read value MUST arrive as an explicit null — never a fabricated 0,
//     or every car in the fleet would advertise cooled seats it may not have.
func TestVehicleSnapshotHandler_SeatCoolerPresenceOnWire(t *testing.T) {
	const userID = "user-1"

	decodeBody := func(t *testing.T, row VehicleSnapshotRow) map[string]any {
		t.Helper()
		h := NewVehicleSnapshotHandler(
			&stubTokenValidator{userID: userID},
			&stubVehicleSnapshotReader{row: row},
			discardLogger(),
			WithSnapshotRoleResolver(&stubRoleResolver{role: "owner"}),
		)
		mux := http.NewServeMux()
		mux.Handle("GET /api/vehicles/{vehicleId}/snapshot", h)

		req := httptest.NewRequestWithContext(
			context.Background(),
			http.MethodGet,
			"/api/vehicles/"+fixtureSnapshotRowID+"/snapshot",
			nil,
		)
		req.Header.Set("Authorization", "Bearer t")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200. Body: %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}

	t.Run("present-but-off reaches the wire as 0, not null", func(t *testing.T) {
		row := fixtureSnapshotRow(userID)
		row.SeatCoolerLeft = intPtr(0)
		row.SeatCoolerRight = intPtr(0)

		body := decodeBody(t, row)
		for _, key := range []string{"seatCoolerLeft", "seatCoolerRight"} {
			got, ok := body[key]
			if !ok {
				t.Errorf("%s: key missing; a vented car with the seat off would read as "+
					"having no cooled seats", key)
				continue
			}
			if got != 0.0 {
				t.Errorf("%s: got %v, want 0 (present-but-off is the capability signal)", key, got)
			}
		}
	})

	t.Run("non-zero levels reach the wire unchanged", func(t *testing.T) {
		row := fixtureSnapshotRow(userID)
		row.SeatCoolerLeft = intPtr(3)
		row.SeatCoolerRight = intPtr(1)

		body := decodeBody(t, row)
		if got := body["seatCoolerLeft"]; got != 3.0 {
			t.Errorf("seatCoolerLeft: got %v, want 3", got)
		}
		if got := body["seatCoolerRight"]; got != 1.0 {
			t.Errorf("seatCoolerRight: got %v, want 1", got)
		}
	})

	t.Run("never-seen values are explicit null, never a fabricated 0", func(t *testing.T) {
		// fixtureSnapshotRow leaves both nil — the heat-only car, or one that has
		// simply never streamed. Absence here is what lets the client withhold the
		// Cool affordance honestly.
		body := decodeBody(t, fixtureSnapshotRow(userID))

		for _, key := range []string{"seatCoolerLeft", "seatCoolerRight"} {
			got, ok := body[key]
			if !ok {
				t.Errorf("%s: key missing from owner projection; siblings keep the key "+
					"with a null value", key)
				continue
			}
			if got != nil {
				t.Errorf("%s: got %v, want explicit null — a fabricated 0 would advertise "+
					"cooled seats the car may not have", key, got)
			}
		}
	})
}

// TestVehicleSnapshotHandler_LicensePlateOnWire pins the MYR-286 read path on
// /snapshot for BOTH roles.
//
// Two things are load-bearing here:
//
//   - Empty-value convention: the plate mirrors its sibling identity field
//     `color` exactly — a plain string with NO omitempty, so the key is ALWAYS
//     present and "no plate set" is an empty string rather than a missing key.
//     Clients read an ABSENT key as "server predates MYR-286" and a present-but-
//     empty one as "owner has not entered a plate"; collapsing the two would
//     erase that distinction.
//   - Visibility: the VIEWER receives it too. That is the deliberate product
//     decision behind the field (a rider must be able to identify the car
//     pulling up), and it is the one place it differs from the owner-only `vin`
//     the same projection strips.
func TestVehicleSnapshotHandler_LicensePlateOnWire(t *testing.T) {
	const userID = "user-1"

	decodeBody := func(t *testing.T, row VehicleSnapshotRow, role auth.Role) map[string]any {
		t.Helper()
		h := NewVehicleSnapshotHandler(
			&stubTokenValidator{userID: userID},
			&stubVehicleSnapshotReader{row: row},
			discardLogger(),
			WithSnapshotRoleResolver(&stubRoleResolver{role: role}),
		)
		mux := http.NewServeMux()
		mux.Handle("GET /api/vehicles/{vehicleId}/snapshot", h)

		req := httptest.NewRequestWithContext(
			context.Background(),
			http.MethodGet,
			"/api/vehicles/"+fixtureSnapshotRowID+"/snapshot",
			nil,
		)
		req.Header.Set("Authorization", "Bearer t")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200. Body: %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}

	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleViewer} {
		t.Run(string(role)+" sees a set plate", func(t *testing.T) {
			row := fixtureSnapshotRow(userID)
			row.LicensePlate = "ABC 1234"

			body := decodeBody(t, row, role)
			got, ok := body["licensePlate"]
			if !ok {
				t.Fatalf("licensePlate missing from the %s projection", role)
			}
			if got != "ABC 1234" {
				t.Errorf("licensePlate = %v, want %q", got, "ABC 1234")
			}
		})

		t.Run(string(role)+" gets an empty string when unset, not a missing key", func(t *testing.T) {
			// fixtureSnapshotRow leaves LicensePlate at its zero value — the
			// car whose owner never entered a plate.
			body := decodeBody(t, fixtureSnapshotRow(userID), role)

			got, ok := body["licensePlate"]
			if !ok {
				t.Fatalf("licensePlate key missing; it must be always-emitted like its sibling `color`")
			}
			if got != "" {
				t.Errorf("licensePlate = %v, want the empty string", got)
			}
			// Same shape as the sibling it mirrors.
			if _, ok := body["color"]; !ok {
				t.Error("sibling `color` is missing; the convention this field mirrors has changed")
			}
		})
	}

	t.Run("viewer keeps the plate but still loses the owner-only vin", func(t *testing.T) {
		row := fixtureSnapshotRow(userID)
		row.LicensePlate = "ABC 1234"

		body := decodeBody(t, row, auth.RoleViewer)
		if _, ok := body["licensePlate"]; !ok {
			t.Error("viewer must receive licensePlate (MYR-286: deliberately both roles)")
		}
		if _, ok := body["vin"]; ok {
			t.Error("viewer must NOT receive the full vin (MYR-279: owner-only)")
		}
	})
}
