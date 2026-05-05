package wserrors

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// reachability is one entry in the MYR-47 closed-enum reachability
// matrix: "code C is emitted by triggering scenario S, asserted at the
// wire shape produced by S."
//
// The scenario field is a documentation string pointing to where the
// per-code emission test lives. Reachability is asserted by the
// per-code subtests in their natural test packages — internal/telemetry/
// for REST codes, internal/ws/ for WS frame codes. This matrix exists
// so the closed-enum walk in TestErrorCodeCatalog_AllReachable proves
// the set is exhaustive (a new ErrorCode cannot be added without also
// adding a row here).
type reachability struct {
	code      ErrorCode
	scenario  string
	skipUntil string // non-empty -> not yet emittable; cite the blocking DV
}

var reachabilityMatrix = []reachability{
	{code: ErrCodeAuthFailed, scenario: "internal/telemetry/vehicle_status_handler_test.go (REST 401), internal/ws/hub_test.go (WS frame)"},
	{code: ErrCodeAuthTimeout, scenario: "internal/ws/hub_test.go (WS frame)"},
	{code: ErrCodeInvalidRequest, scenario: "internal/telemetry/vehicle_status_handler_test.go (REST 400 invalid VIN)"},
	{code: ErrCodeNotFound, scenario: "internal/telemetry/vehicle_status_handler_test.go (REST 404 unknown vehicle)"},
	{code: ErrCodeVehicleNotOwned, scenario: "internal/telemetry/vehicle_status_handler_test.go (REST 403 ownership mismatch)"},
	{code: ErrCodeInternalError, scenario: "internal/telemetry/vehicle_status_handler_test.go (REST 500 lookup failure)"},
	{code: ErrCodeRateLimited, scenario: "internal/ws/handler_ratelimit_test.go (HTTP 429 envelope on upgrade)"},
	{code: ErrCodePermissionDenied, skipUntil: "DV-07 — emitted alongside MYR-46 per-vehicle subscribe"},
	{code: ErrCodeServiceUnavailable, skipUntil: "REST-only forward-compatibility code; not yet emitted (rest-api.md §4.1.1.a)"},
	{code: ErrCodeSnapshotRequired, skipUntil: "DV-02 — envelope sequence numbers"},
}

// TestErrorCodeCatalog_AllReachable asserts the closed catalog matches
// the reachability matrix one-to-one. Drift detector: a new ErrorCode
// added to AllCodes() without a matrix row fails here, and a matrix
// row referring to a missing code also fails.
func TestErrorCodeCatalog_AllReachable(t *testing.T) {
	covered := make(map[ErrorCode]reachability, len(reachabilityMatrix))
	for _, r := range reachabilityMatrix {
		if existing, dup := covered[r.code]; dup {
			t.Fatalf("duplicate matrix entry for %q: %+v vs %+v", r.code, existing, r)
		}
		covered[r.code] = r
	}

	known := make(map[ErrorCode]struct{}, len(AllCodes()))
	for _, code := range AllCodes() {
		known[code] = struct{}{}
		r, ok := covered[code]
		if !ok {
			t.Errorf("ErrorCode %q has no reachability matrix row — add one to reachabilityMatrix", code)
			continue
		}
		if r.skipUntil != "" {
			t.Logf("ErrorCode %q PLANNED: %s", code, r.skipUntil)
		}
	}

	for _, r := range reachabilityMatrix {
		if _, ok := known[r.code]; !ok {
			t.Errorf("reachability matrix row references unknown ErrorCode %q", r.code)
		}
	}
}

// TestWriteErrorEnvelope_Shape asserts the wire shape produced by
// WriteErrorEnvelope matches the rest-api.md §4.1 contract:
// {error: {code, message, subCode}}, with subCode null in v1.
func TestWriteErrorEnvelope_Shape(t *testing.T) {
	rec := httptest.NewRecorder()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	WriteErrorEnvelope(rec, logger, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid VIN")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content-type: got %q", got)
	}
	var env ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != ErrCodeInvalidRequest {
		t.Fatalf("code: got %q, want %q", env.Error.Code, ErrCodeInvalidRequest)
	}
	if env.Error.Message != "invalid VIN" {
		t.Fatalf("message: got %q, want %q", env.Error.Message, "invalid VIN")
	}
	if env.Error.SubCode != nil {
		t.Fatalf("subCode: got %q, want nil", *env.Error.SubCode)
	}

	// Belt-and-suspenders: re-marshal the envelope and assert the literal
	// JSON keys are the contract-canonical names. Defends against an
	// accidental json-tag rename on ErrorEnvelopeBody.
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"error"`, `"code"`, `"message"`, `"subCode"`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("envelope %s missing key %s", out, want)
		}
	}
}
