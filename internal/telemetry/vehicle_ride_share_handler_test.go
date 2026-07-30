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

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

const (
	rsUserID   = "usr_rs_owner"
	rsOtherUsr = "usr_rs_stranger"
	rsAuthOK   = "Bearer good-token"
)

// fakeRideShareWriter records the write, keeping the VALUE distinguishable from
// "never called" — for a boolean toggle, "wrote false" and "did not write" are
// the two failures most easily confused with each other.
type fakeRideShareWriter struct {
	calls int
	last  bool
	err   error
}

func (f *fakeRideShareWriter) SetRideShareEnabled(_ context.Context, _ string, enabled bool) error {
	f.calls++
	f.last = enabled
	return f.err
}

// newRideShareHandler builds the handler with rsUserID as the AUTHENTICATED
// caller. The caller is deliberately fixed rather than parameterised: the
// non-owner cases in this file are expressed by changing whom the VEHICLE
// belongs to (the reader's row), not whom the token names, which is the shape a
// real ownership mismatch arrives in.
func newRideShareHandler(
	reader *stubVehicleSnapshotReader,
	writer *fakeRideShareWriter,
) *VehicleRideShareHandler {
	return NewVehicleRideShareHandler(
		&stubTokenValidator{userID: rsUserID},
		reader,
		writer,
		discardLogger(),
	)
}

func rsMux(h *VehicleRideShareHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/tesla/vehicles/{vehicleId}/ride-share", h.ServeHTTP)
	return mux
}

func rsPath() string {
	return "/api/tesla/vehicles/" + fixtureSnapshotRowID + "/ride-share"
}

func doRideShare(h *VehicleRideShareHandler, method, path, authHeader, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(context.Background(), method, path, nil)
	} else {
		r = httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	}
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	rsMux(h).ServeHTTP(rec, r)
	return rec
}

// TestVehicleRideShareHandler_AuthAndOwnership covers the access matrix. The
// row that matters most is the LAST one: a stranger and an unknown vehicle must
// be distinguishable to nobody but the server, so the unknown-id case returns
// the same 404 an ownership-filtered vehicle would — the endpoint never
// confirms that somebody else's car exists.
func TestVehicleRideShareHandler_AuthAndOwnership(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		auth        string
		body        string
		reader      *stubVehicleSnapshotReader
		wantStatus  int
		wantErrCode wserrors.ErrorCode
	}{
		{
			name:        "missing auth",
			method:      http.MethodPut,
			auth:        "",
			body:        `{"enabled":false}`,
			reader:      &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rsUserID)},
			wantStatus:  http.StatusUnauthorized,
			wantErrCode: wserrors.ErrCodeAuthFailed,
		},
		{
			name:   "wrong method",
			method: http.MethodPost,
			auth:   rsAuthOK,
			body:   `{"enabled":false}`,
			reader: &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rsUserID)},
			// No wantErrCode: the production mux routes PUT only, so a POST is
			// refused by net/http's own 405 before the handler's belt-and-braces
			// method guard is reached, and that 405 carries no JSON envelope.
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:        "unknown vehicle is 404, indistinguishable from filtered",
			method:      http.MethodPut,
			auth:        rsAuthOK,
			body:        `{"enabled":false}`,
			reader:      &stubVehicleSnapshotReader{err: fmt.Errorf("get vehicle: %w", sdk.ErrNotFound)},
			wantStatus:  http.StatusNotFound,
			wantErrCode: wserrors.ErrCodeNotFound,
		},
		{
			name:        "a non-owner is 403 vehicle_not_owned",
			method:      http.MethodPut,
			auth:        rsAuthOK,
			body:        `{"enabled":false}`,
			reader:      &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rsOtherUsr)},
			wantStatus:  http.StatusForbidden,
			wantErrCode: wserrors.ErrCodeVehicleNotOwned,
		},
		{
			name:        "a vehicle lookup failure is 500, never a silent write",
			method:      http.MethodPut,
			auth:        rsAuthOK,
			body:        `{"enabled":false}`,
			reader:      &stubVehicleSnapshotReader{err: errors.New("db down")},
			wantStatus:  http.StatusInternalServerError,
			wantErrCode: wserrors.ErrCodeInternalError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := &fakeRideShareWriter{}
			h := newRideShareHandler(tt.reader, writer)

			rec := doRideShare(h, tt.method, rsPath(), tt.auth, tt.body)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d want %d. body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantErrCode != "" {
				var env wserrors.ErrorEnvelope
				if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
					t.Fatalf("decode envelope: %v", err)
				}
				if env.Error.Code != tt.wantErrCode {
					t.Errorf("error.code: got %q want %q", env.Error.Code, tt.wantErrCode)
				}
			}
			// The invariant behind every one of these rows: a refused request
			// writes NOTHING.
			if writer.calls != 0 {
				t.Errorf("a refused request must not write, got %d calls", writer.calls)
			}
		})
	}
}

// TestVehicleRideShareHandler_Body covers decoding. `enabled` is REQUIRED here,
// unlike the §7.16 service window where absence means "clear": this field has no
// clear, so a body naming neither value is a client bug rather than a third
// state, and guessing would either un-pause a car its owner paused or withdraw
// one they never touched.
func TestVehicleRideShareHandler_Body(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantWrites int
	}{
		{name: "explicit false pauses", body: `{"enabled":false}`, wantStatus: http.StatusOK, wantWrites: 1},
		{name: "explicit true re-enables", body: `{"enabled":true}`, wantStatus: http.StatusOK, wantWrites: 1},
		{name: "absent key is a 400, not a default", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "explicit null is a 400", body: `{"enabled":null}`, wantStatus: http.StatusBadRequest},
		{name: "empty body is a 400 (contrast §7.16, where it clears)", body: "", wantStatus: http.StatusBadRequest},
		{name: "unknown keys are refused", body: `{"enabled":false,"forever":true}`, wantStatus: http.StatusBadRequest},
		{name: "a non-boolean is a 400", body: `{"enabled":"false"}`, wantStatus: http.StatusBadRequest},
		{name: "malformed JSON is a 400", body: `{"enabled":`, wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := &fakeRideShareWriter{}
			h := newRideShareHandler(&stubVehicleSnapshotReader{row: fixtureSnapshotRow(rsUserID)}, writer)

			rec := doRideShare(h, http.MethodPut, rsPath(), rsAuthOK, tt.body)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d want %d. body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if writer.calls != tt.wantWrites {
				t.Errorf("writes: got %d want %d", writer.calls, tt.wantWrites)
			}
		})
	}
}

// TestVehicleRideShareHandler_WritesAndEchoesTheValue pins the two halves that
// a boolean endpoint gets wrong most easily: the value actually reaching the
// writer, and the response echoing the RESOLVED state so a client can adopt it
// without a re-read (there is no WebSocket delta for this field, so the echo is
// the only immediate confirmation the toggle has).
func TestVehicleRideShareHandler_WritesAndEchoesTheValue(t *testing.T) {
	for _, want := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%v", want), func(t *testing.T) {
			writer := &fakeRideShareWriter{}
			h := newRideShareHandler(&stubVehicleSnapshotReader{row: fixtureSnapshotRow(rsUserID)}, writer)

			rec := doRideShare(h, http.MethodPut, rsPath(), rsAuthOK,
				fmt.Sprintf(`{"enabled":%v}`, want))

			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d want 200. body=%s", rec.Code, rec.Body.String())
			}
			if writer.calls != 1 || writer.last != want {
				t.Fatalf("writer: calls=%d last=%v, want 1 call with %v", writer.calls, writer.last, want)
			}

			var body struct {
				VehicleID string `json:"vehicleId"`
				Enabled   bool   `json:"enabled"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.VehicleID != fixtureSnapshotRowID {
				t.Errorf("vehicleId: got %q want %q", body.VehicleID, fixtureSnapshotRowID)
			}
			if body.Enabled != want {
				t.Errorf("echoed enabled: got %v want %v", body.Enabled, want)
			}
		})
	}
}

// TestVehicleRideShareHandler_WriteFailureIsA500 makes sure a failed write is
// never reported as success. The toggle's whole value is that its position is
// trustworthy; a 200 over a failed write would leave the owner believing their
// car is paused when it is still taking requests.
func TestVehicleRideShareHandler_WriteFailureIsA500(t *testing.T) {
	writer := &fakeRideShareWriter{err: errors.New("db down")}
	h := newRideShareHandler(&stubVehicleSnapshotReader{row: fixtureSnapshotRow(rsUserID)}, writer)

	rec := doRideShare(h, http.MethodPut, rsPath(), rsAuthOK, `{"enabled":false}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500. body=%s", rec.Code, rec.Body.String())
	}
	var env wserrors.ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Code != wserrors.ErrCodeInternalError {
		t.Errorf("error.code: got %q want %q", env.Error.Code, wserrors.ErrCodeInternalError)
	}
}

// TestVehicleRideShareHandler_NoShareTierGrantsTheToggle is the counter-
// assertion that matters most for this endpoint specifically. Every OTHER
// rider-adjacent surface admits a viewer at some share tier; this one admits
// none, at any tier, because the pause is the owner's switch and a rider able to
// flip it would invert the feature. The handler consults no share reader at all,
// so the assertion is that a non-owner is refused with the ownership 403 and
// never reaches the write.
func TestVehicleRideShareHandler_NoShareTierGrantsTheToggle(t *testing.T) {
	writer := &fakeRideShareWriter{}
	// The caller is a legitimately authenticated user who is simply not the
	// owner — the shape a top-tier `rides` viewer arrives in.
	h := newRideShareHandler(&stubVehicleSnapshotReader{row: fixtureSnapshotRow(rsOtherUsr)}, writer)

	rec := doRideShare(h, http.MethodPut, rsPath(), rsAuthOK, `{"enabled":false}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a share tier must not grant the toggle: got %d. body=%s", rec.Code, rec.Body.String())
	}
	if writer.calls != 0 {
		t.Errorf("a non-owner must not write, got %d calls", writer.calls)
	}
}
