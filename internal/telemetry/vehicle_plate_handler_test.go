package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// --- fakes -----------------------------------------------------------------

// fakePlateWriter records the exact value handed to the store so the tests can
// assert on the NORMALIZED plate (the whole point of §7.14's write path) rather
// than on the raw request body.
type fakePlateWriter struct {
	updated      bool
	err          error
	called       bool
	gotUserID    string
	gotVehicleID string
	gotPlate     string
}

func (f *fakePlateWriter) UpdateLicensePlate(_ context.Context, userID, vehicleID, plate string) (bool, error) {
	f.called = true
	f.gotUserID = userID
	f.gotVehicleID = vehicleID
	f.gotPlate = plate
	return f.updated, f.err
}

// --- helpers ---------------------------------------------------------------

const (
	plateUserID    = "user-123"
	plateVehicleID = "veh_abc123"
)

func ownedPlateRow() VehicleSnapshotRow {
	return VehicleSnapshotRow{ID: plateVehicleID, UserID: plateUserID}
}

func doPlatePUT(t *testing.T, reader VehicleSnapshotReader, writer VehiclePlateWriter, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doPlateRequest(t, reader, writer, http.MethodPut, body, true)
}

func doPlateRequest(
	t *testing.T,
	reader VehicleSnapshotReader,
	writer VehiclePlateWriter,
	method, body string,
	withAuth bool,
) *httptest.ResponseRecorder {
	t.Helper()
	h := NewVehiclePlateHandler(&stubTokenValidator{userID: plateUserID}, reader, writer, discardLogger())
	mux := http.NewServeMux()
	mux.Handle("/api/tesla/vehicles/{vehicleId}/plate", h)
	req := httptest.NewRequestWithContext(
		context.Background(),
		method,
		"/api/tesla/vehicles/"+plateVehicleID+"/plate",
		strings.NewReader(body),
	)
	if withAuth {
		req.Header.Set("Authorization", "Bearer jwt")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// decodePlateBody decodes a 200 body into the response shape.
func decodePlateBody(t *testing.T, rec *httptest.ResponseRecorder) vehiclePlateResponse {
	t.Helper()
	var got vehiclePlateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, rec.Body.String())
	}
	return got
}

// errorCode pulls `error.code` out of the rest-api.md §4.1 error envelope.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal error envelope: %v (body=%s)", err, rec.Body.String())
	}
	return env.Error.Code
}

// --- tests -----------------------------------------------------------------

// TestVehiclePlateHandler_Normalization is the core write-path contract: the
// STORED value is trim + uppercase of the submission, and validation runs
// AFTER normalization (so "  abc 1234  " is accepted, not rejected for
// lowercase or for its untrimmed length).
func TestVehiclePlateHandler_Normalization(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantPlate string
	}{
		{name: "lowercase with space is uppercased", body: `{"plate":"abc 1234"}`, wantPlate: "ABC 1234"},
		{name: "surrounding whitespace is trimmed", body: `{"plate":"   abc 1234   "}`, wantPlate: "ABC 1234"},
		{name: "interior spacing preserved verbatim", body: `{"plate":"7 XYZ 12"}`, wantPlate: "7 XYZ 12"},
		{name: "hyphen is in the charset", body: `{"plate":"abc-1234"}`, wantPlate: "ABC-1234"},
		{name: "already normalized is idempotent", body: `{"plate":"ABC 1234"}`, wantPlate: "ABC 1234"},
		{name: "exactly ten chars is accepted", body: `{"plate":"abcdefghij"}`, wantPlate: "ABCDEFGHIJ"},
		{
			name:      "trim happens before the length check",
			body:      `{"plate":"    abcdefghij    "}`,
			wantPlate: "ABCDEFGHIJ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := &fakePlateWriter{updated: true}
			rec := doPlatePUT(t, &stubSnapshotReader{row: ownedPlateRow()}, writer, tt.body)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			if !writer.called {
				t.Fatal("writer was not called")
			}
			if writer.gotPlate != tt.wantPlate {
				t.Errorf("stored plate = %q, want %q", writer.gotPlate, tt.wantPlate)
			}
			// The response echoes the normalized value so a client can adopt it
			// optimistically — there is no WS delta for this field.
			if got := decodePlateBody(t, rec); got.LicensePlate != tt.wantPlate {
				t.Errorf("response licensePlate = %q, want %q", got.LicensePlate, tt.wantPlate)
			}
			// The write is owner-scoped at the SQL layer too: the handler must
			// pass the JWT-resolved caller, never anything off the request.
			if writer.gotUserID != plateUserID || writer.gotVehicleID != plateVehicleID {
				t.Errorf("writer got (user=%q, vehicle=%q), want (%q, %q)",
					writer.gotUserID, writer.gotVehicleID, plateUserID, plateVehicleID)
			}
		})
	}
}

// TestVehiclePlateHandler_EmptyClears pins the deliberate contract that an
// empty submission CLEARS the plate — it is an ordinary write of the empty
// string, not a validation failure and not a separate DELETE verb.
func TestVehiclePlateHandler_EmptyClears(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "explicit empty string", body: `{"plate":""}`},
		{name: "whitespace-only normalizes to empty", body: `{"plate":"   "}`},
		{name: "omitted key is the zero value", body: `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := &fakePlateWriter{updated: true}
			rec := doPlatePUT(t, &stubSnapshotReader{row: ownedPlateRow()}, writer, tt.body)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			if !writer.called {
				t.Fatal("clear must still reach the writer (it writes an empty string)")
			}
			if writer.gotPlate != "" {
				t.Errorf("stored plate = %q, want empty string (cleared)", writer.gotPlate)
			}
			if got := decodePlateBody(t, rec); got.LicensePlate != "" {
				t.Errorf("response licensePlate = %q, want empty string", got.LicensePlate)
			}
		})
	}
}

// TestVehiclePlateHandler_Rejects covers the two 400 invalid_request cases,
// both evaluated AFTER normalization. A rejection must never reach the writer.
func TestVehiclePlateHandler_Rejects(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "eleven chars is too long", body: `{"plate":"ABCDEFGHIJK"}`},
		{name: "too long only after trimming is impossible — still too long", body: `{"plate":"  ABCDEFGHIJK  "}`},
		{name: "punctuation is out of charset", body: `{"plate":"ABC.123"}`},
		{name: "underscore is out of charset", body: `{"plate":"ABC_123"}`},
		{name: "non-ascii is out of charset", body: `{"plate":"ÅBC123"}`},
		{name: "interior newline is out of charset", body: "{\"plate\":\"AB\\nCD\"}"},
		{name: "malformed json", body: `{"plate":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := &fakePlateWriter{updated: true}
			rec := doPlatePUT(t, &stubSnapshotReader{row: ownedPlateRow()}, writer, tt.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "invalid_request" {
				t.Errorf("error code = %q, want invalid_request", code)
			}
			if writer.called {
				t.Errorf("rejected plate must not be written (got %q)", writer.gotPlate)
			}
			// P1: the rejected value must never be echoed back in the envelope.
			if strings.Contains(rec.Body.String(), "ABCDEFGHIJK") {
				t.Errorf("error body echoed the submitted plate (P1 leak): %s", rec.Body.String())
			}
		})
	}
}

// TestVehiclePlateHandler_OwnershipAndAuth pins the error semantics against the
// sibling §7.12 teardown convention: unknown vehicle → 404 not_found
// (indistinguishable from ownership-filtered, never leaks existence), real
// ownership mismatch → 403 vehicle_not_owned. Neither writes anything.
func TestVehiclePlateHandler_OwnershipAndAuth(t *testing.T) {
	tests := []struct {
		name        string
		reader      VehicleSnapshotReader
		writer      *fakePlateWriter
		method      string
		body        string
		withAuth    bool
		wantStatus  int
		wantCode    string
		wantWritten bool
	}{
		{
			name:       "unknown vehicle -> 404 not_found",
			reader:     &stubSnapshotReader{err: sdk.ErrNotFound},
			writer:     &fakePlateWriter{updated: true},
			method:     http.MethodPut,
			body:       `{"plate":"ABC 1234"}`,
			withAuth:   true,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "non-owner -> 403 vehicle_not_owned",
			reader:     &stubSnapshotReader{row: VehicleSnapshotRow{ID: plateVehicleID, UserID: "someone-else"}},
			writer:     &fakePlateWriter{updated: true},
			method:     http.MethodPut,
			body:       `{"plate":"ABC 1234"}`,
			withAuth:   true,
			wantStatus: http.StatusForbidden,
			wantCode:   "vehicle_not_owned",
		},
		{
			name:       "missing bearer -> 401 auth_failed",
			reader:     &stubSnapshotReader{row: ownedPlateRow()},
			writer:     &fakePlateWriter{updated: true},
			method:     http.MethodPut,
			body:       `{"plate":"ABC 1234"}`,
			withAuth:   false,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "auth_failed",
		},
		{
			name:       "non-PUT method -> 405 invalid_request",
			reader:     &stubSnapshotReader{row: ownedPlateRow()},
			writer:     &fakePlateWriter{updated: true},
			method:     http.MethodPost,
			body:       `{"plate":"ABC 1234"}`,
			withAuth:   true,
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "invalid_request",
		},
		{
			name:       "lookup failure -> 500 internal_error",
			reader:     &stubSnapshotReader{err: errors.New("db down")},
			writer:     &fakePlateWriter{updated: true},
			method:     http.MethodPut,
			body:       `{"plate":"ABC 1234"}`,
			withAuth:   true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
		},
		{
			name:       "write failure -> 500 internal_error",
			reader:     &stubSnapshotReader{row: ownedPlateRow()},
			writer:     &fakePlateWriter{err: errors.New("db down")},
			method:     http.MethodPut,
			body:       `{"plate":"ABC 1234"}`,
			withAuth:   true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
			// The writer IS reached here; the failure is on its way back.
			wantWritten: true,
		},
		{
			name:       "row vanished between lookup and write -> 404 not_found",
			reader:     &stubSnapshotReader{row: ownedPlateRow()},
			writer:     &fakePlateWriter{updated: false},
			method:     http.MethodPut,
			body:       `{"plate":"ABC 1234"}`,
			withAuth:   true,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
			// Zero rows affected must not be reported as a successful write.
			wantWritten: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doPlateRequest(t, tt.reader, tt.writer, tt.method, tt.body, tt.withAuth)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if code := errorCode(t, rec); code != tt.wantCode {
				t.Errorf("error code = %q, want %q", code, tt.wantCode)
			}
			if tt.writer.called != tt.wantWritten {
				t.Errorf("writer called = %v, want %v", tt.writer.called, tt.wantWritten)
			}
		})
	}
}
