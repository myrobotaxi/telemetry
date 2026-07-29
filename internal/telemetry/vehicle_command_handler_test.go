package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// fakeCommandExecutor records the request it received and returns a scripted
// result/error — the handler is tested without touching internal/commands'
// transport or any Tesla endpoint.
type fakeCommandExecutor struct {
	result commands.Result
	err    error
	got    commands.Request
	calls  int
}

func (f *fakeCommandExecutor) Execute(_ context.Context, req commands.Request) (commands.Result, error) {
	f.calls++
	f.got = req
	return f.result, f.err
}

func newCommandHandler(exec commandExecutor, reader *stubVehicleSnapshotReader, validator *stubTokenValidator) *VehicleCommandHandler {
	return NewVehicleCommandHandler(
		validator,
		reader,
		validTeslaToken(),
		exec,
		discardLogger(),
	)
}

func postCommand(h *VehicleCommandHandler, vehicleID, name, body, authHeader string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/vehicles/"+vehicleID+"/command/"+name, reader)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	req.SetPathValue("vehicleId", vehicleID)
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeErrCode(t *testing.T, rec *httptest.ResponseRecorder) wserrors.ErrorCode {
	t.Helper()
	var env wserrors.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, rec.Body.String())
	}
	return env.Error.Code
}

func TestVehicleCommandHandler_MethodNotAllowed(t *testing.T) {
	h := newCommandHandler(&fakeCommandExecutor{}, &stubVehicleSnapshotReader{}, &stubTokenValidator{userID: "u1"})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/vehicles/v1/command/door_lock", nil)
	req.SetPathValue("vehicleId", "v1")
	req.SetPathValue("name", "door_lock")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d want 405", rec.Code)
	}
}

func TestVehicleCommandHandler_MissingAuth(t *testing.T) {
	h := newCommandHandler(&fakeCommandExecutor{}, &stubVehicleSnapshotReader{}, &stubTokenValidator{userID: "u1"})
	rec := postCommand(h, "v1", "door_lock", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d want 401", rec.Code)
	}
	if decodeErrCode(t, rec) != wserrors.ErrCodeAuthFailed {
		t.Fatalf("code = %q", decodeErrCode(t, rec))
	}
}

func TestVehicleCommandHandler_InvalidToken(t *testing.T) {
	h := newCommandHandler(&fakeCommandExecutor{}, &stubVehicleSnapshotReader{}, &stubTokenValidator{err: errors.New("bad token")})
	rec := postCommand(h, "v1", "door_lock", "", "Bearer x")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d want 401", rec.Code)
	}
}

func TestVehicleCommandHandler_OwnershipMismatch(t *testing.T) {
	reader := &stubVehicleSnapshotReader{row: fixtureSnapshotRow("owner-1")}
	h := newCommandHandler(&fakeCommandExecutor{}, reader, &stubTokenValidator{userID: "not-owner"})
	rec := postCommand(h, fixtureSnapshotRowID, "door_lock", "", "Bearer x")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d want 403", rec.Code)
	}
	if decodeErrCode(t, rec) != wserrors.ErrCodeVehicleNotOwned {
		t.Fatalf("code = %q want vehicle_not_owned", decodeErrCode(t, rec))
	}
}

func TestVehicleCommandHandler_VehicleNotFound(t *testing.T) {
	reader := &stubVehicleSnapshotReader{err: sdk.ErrNotFound}
	h := newCommandHandler(&fakeCommandExecutor{}, reader, &stubTokenValidator{userID: "u1"})
	rec := postCommand(h, "v1", "door_lock", "", "Bearer x")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d want 404", rec.Code)
	}
}

func TestVehicleCommandHandler_MalformedBody(t *testing.T) {
	reader := &stubVehicleSnapshotReader{row: fixtureSnapshotRow("u1")}
	h := newCommandHandler(&fakeCommandExecutor{}, reader, &stubTokenValidator{userID: "u1"})
	rec := postCommand(h, fixtureSnapshotRowID, "set_temps", "{not json", "Bearer x")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400", rec.Code)
	}
	if decodeErrCode(t, rec) != wserrors.ErrCodeInvalidRequest {
		t.Fatalf("code = %q", decodeErrCode(t, rec))
	}
}

func TestVehicleCommandHandler_Success(t *testing.T) {
	reader := &stubVehicleSnapshotReader{row: fixtureSnapshotRow("u1")}
	exec := &fakeCommandExecutor{result: commands.Result{Command: "door_lock", Applied: true}}
	h := newCommandHandler(exec, reader, &stubTokenValidator{userID: "u1"})

	rec := postCommand(h, fixtureSnapshotRowID, "door_lock", "", "Bearer x")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	// The executor received the full VIN and the caller's Tesla access token.
	if exec.got.VIN != "5YJ3E1EA1PF000001" || exec.got.Command != "door_lock" {
		t.Fatalf("executor request = %+v", exec.got)
	}
	if exec.got.AccessToken != "tesla-oauth-token-abc" {
		t.Fatalf("executor got wrong token: %q", exec.got.AccessToken)
	}
	// The success body redacts the VIN to last-4.
	var body vehicleCommandResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "applied" || strings.Contains(body.VIN, "5YJ3E1") {
		t.Fatalf("response = %+v (VIN must be redacted)", body)
	}
}

func TestVehicleCommandHandler_TypedCommandErrorPropagates(t *testing.T) {
	reader := &stubVehicleSnapshotReader{row: fixtureSnapshotRow("u1")}
	// The executor returns the pre-pairing key_not_paired error.
	exec := &fakeCommandExecutor{err: &commands.CommandError{
		Code:    wserrors.ErrCodeKeyNotPaired,
		Status:  http.StatusForbidden,
		Message: "virtual key not paired with vehicle",
	}}
	h := newCommandHandler(exec, reader, &stubTokenValidator{userID: "u1"})
	rec := postCommand(h, fixtureSnapshotRowID, "door_lock", "", "Bearer x")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d want 403", rec.Code)
	}
	if decodeErrCode(t, rec) != wserrors.ErrCodeKeyNotPaired {
		t.Fatalf("code = %q want key_not_paired", decodeErrCode(t, rec))
	}
}

func TestVehicleCommandHandler_PerVehicleCooldown(t *testing.T) {
	reader := &stubVehicleSnapshotReader{row: fixtureSnapshotRow("u1")}
	exec := &fakeCommandExecutor{result: commands.Result{Command: "door_lock", Applied: true}}
	h := newCommandHandler(exec, reader, &stubTokenValidator{userID: "u1"})

	// Burst is defaultCommandBurst (2); the third rapid command trips the cap.
	var lastCode int
	for i := 0; i < defaultCommandBurst+1; i++ {
		rec := postCommand(h, fixtureSnapshotRowID, "door_lock", "", "Bearer x")
		lastCode = rec.Code
	}
	if lastCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after burst, got %d", lastCode)
	}
}

// inServiceTransport is a commands.Transport that answers every command the way
// the tesla-http-proxy answers one sent to a car in service mode: HTTP 200 with
// a negative car response whose reason is the car's own plain text, relayed
// under the proxy's "car could not execute command: " prefix.
type inServiceTransport struct{}

func (inServiceTransport) Command(_ context.Context, _ commands.TransportRequest) (commands.TransportResult, error) {
	return commands.TransportResult{
		Outcome: commands.OutcomeFailed,
		Reason:  "car could not execute command: vehicle is in service",
	}, nil
}
func (inServiceTransport) Wake(_ context.Context, _, _ string) error { return nil }
func (inServiceTransport) Enabled() bool                            { return true }
func (inServiceTransport) RESTEnabled() bool                        { return true }

// TestVehicleCommandHandler_RejectionReasonReachesTheWire is MYR-329 end to
// end, through the REAL commands.Executor rather than a scripted CommandError:
// the client's own Jul 28 situation (climate off, car in service) must arrive
// at the app as a 502 command_failed whose message NAMES the reason, so the
// owner is not left guessing at his battery.
func TestVehicleCommandHandler_RejectionReasonReachesTheWire(t *testing.T) {
	reader := &stubVehicleSnapshotReader{row: fixtureSnapshotRow("u1")}
	exec := commands.NewExecutor(inServiceTransport{}, discardLogger())
	h := newCommandHandler(exec, reader, &stubTokenValidator{userID: "u1"})

	rec := postCommand(h, fixtureSnapshotRowID, "auto_conditioning_stop", "", "Bearer x")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d want 502", rec.Code)
	}
	if got := decodeErrCode(t, rec); got != wserrors.ErrCodeCommandFailed {
		t.Fatalf("code = %q want command_failed", got)
	}

	var env wserrors.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, rec.Body.String())
	}
	if !strings.Contains(env.Error.Message, commands.ReasonVehicleInService) {
		t.Errorf("message = %q, want it to name %q", env.Error.Message, commands.ReasonVehicleInService)
	}
	// The car's own prose must NOT ride out to the client — only our token.
	if strings.Contains(env.Error.Message, "could not execute") {
		t.Errorf("message = %q leaks upstream prose", env.Error.Message)
	}
	// P1 hygiene: nothing in the error body may carry the VIN.
	if strings.Contains(rec.Body.String(), "5YJ3E1") {
		t.Errorf("body leaks the VIN: %s", rec.Body.String())
	}
}

// TestVehicleCommandHandler_UnknownRejectionStaysGeneric is the other half of
// the allow-list: a refusal we do not recognize must keep today's generic
// message, so the app keeps its generic copy rather than naming a cause we are
// not sure of.
func TestVehicleCommandHandler_UnknownRejectionStaysGeneric(t *testing.T) {
	reader := &stubVehicleSnapshotReader{row: fixtureSnapshotRow("u1")}
	exec := commands.NewExecutor(unknownReasonTransport{}, discardLogger())
	h := newCommandHandler(exec, reader, &stubTokenValidator{userID: "u1"})

	rec := postCommand(h, fixtureSnapshotRowID, "auto_conditioning_stop", "", "Bearer x")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d want 502", rec.Code)
	}
	var env wserrors.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, rec.Body.String())
	}
	if env.Error.Message != "vehicle command failed" {
		t.Errorf("message = %q want the unchanged generic sentence", env.Error.Message)
	}
	for _, token := range commands.KnownRejectionReasons() {
		if strings.Contains(env.Error.Message, token) {
			t.Errorf("message = %q named %q for an unrecognized reason", env.Error.Message, token)
		}
	}
}

// unknownReasonTransport answers with a real refusal that is deliberately
// outside the allow-list (the SDK's own fallback when the car sends no prose).
type unknownReasonTransport struct{ inServiceTransport }

func (unknownReasonTransport) Command(_ context.Context, _ commands.TransportRequest) (commands.TransportResult, error) {
	return commands.TransportResult{
		Outcome: commands.OutcomeFailed,
		Reason:  "car could not execute command: unspecified error",
	}, nil
}
