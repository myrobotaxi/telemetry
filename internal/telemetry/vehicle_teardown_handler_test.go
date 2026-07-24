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

	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// --- fakes -----------------------------------------------------------------

type fakeTokenResolver struct {
	tok TeslaToken
	err error
}

func (f *fakeTokenResolver) Resolve(_ context.Context, _ string) (TeslaToken, error) {
	return f.tok, f.err
}

// fakeConfigDeleter records whether DeleteTelemetryConfig was called and with
// what VIN, and returns a configurable error. It NEVER makes a network call —
// proving no real Tesla request fires from a unit test.
type fakeConfigDeleter struct {
	err      error
	called   bool
	gotVIN   string
	gotToken string
	order    *int // shared call-order counter
	seq      int  // order value captured when called
}

func (f *fakeConfigDeleter) DeleteTelemetryConfig(_ context.Context, token, vin string) error {
	f.called = true
	f.gotVIN = vin
	f.gotToken = token
	if f.order != nil {
		*f.order++
		f.seq = *f.order
	}
	return f.err
}

type fakeTeardownWriter struct {
	result        VehicleTeardownResult
	err           error
	called        bool
	gotUserID     string
	gotVehicleID  string
	order         *int
	seq           int
}

func (f *fakeTeardownWriter) RemoveVehicle(_ context.Context, userID, vehicleID string) (VehicleTeardownResult, error) {
	f.called = true
	f.gotUserID = userID
	f.gotVehicleID = vehicleID
	if f.order != nil {
		*f.order++
		f.seq = *f.order
	}
	return f.result, f.err
}

// --- helpers ---------------------------------------------------------------

const (
	teardownUserID    = "user-123"
	teardownVehicleID = "veh_abc123"
	teardownVIN       = "5YJ3E1EA1PF000001"
	teardownClientID  = "tesla-client-xyz"
)

func ownedTeardownRow() VehicleSnapshotRow {
	return VehicleSnapshotRow{ID: teardownVehicleID, UserID: teardownUserID, VIN: teardownVIN}
}

func newTeardownHandler(
	reader VehicleSnapshotReader,
	resolver teslaTokenResolver,
	deleter TelemetryConfigDeleter,
	writer VehicleTeardownWriter,
) *VehicleTeardownHandler {
	return NewVehicleTeardownHandler(
		&stubTokenValidator{userID: teardownUserID},
		reader,
		resolver,
		deleter,
		writer,
		VehicleTeardownConfig{RevokeClientID: teardownClientID, RevokeBackURL: "myrobotaxi://tesla-unlinked"},
		discardLogger(),
	)
}

func doTeardown(t *testing.T, h *VehicleTeardownHandler, method, vehicleID string, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/api/tesla/vehicles/{vehicleId}", h)
	req := httptest.NewRequestWithContext(context.Background(), method, "/api/tesla/vehicles/"+vehicleID, nil)
	if withAuth {
		req.Header.Set("Authorization", "Bearer jwt")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func validResolver() *fakeTokenResolver {
	return &fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}} //nolint:gosec // test fixture
}

// --- tests -----------------------------------------------------------------

func TestVehicleTeardownHandler_Sequence(t *testing.T) {
	tests := []struct {
		name                string
		reader              VehicleSnapshotReader
		resolver            teslaTokenResolver
		deleter             *fakeConfigDeleter
		writer              *fakeTeardownWriter
		wantStatus          int
		wantDeleterCalled   bool
		wantWriterCalled    bool
		wantStreamDeleted   bool
		wantWasLastVehicle  bool
		wantTokensCleared   bool
	}{
		{
			name:               "happy path last vehicle",
			reader:             &stubSnapshotReader{row: ownedTeardownRow()},
			resolver:           validResolver(),
			deleter:            &fakeConfigDeleter{},
			writer:             &fakeTeardownWriter{result: VehicleTeardownResult{Removed: true, WasLastVehicle: true, TeslaTokensCleared: true, DriveCount: 3}},
			wantStatus:         http.StatusOK,
			wantDeleterCalled:  true,
			wantWriterCalled:   true,
			wantStreamDeleted:  true,
			wantWasLastVehicle: true,
			wantTokensCleared:  true,
		},
		{
			name:               "non-last removal keeps account",
			reader:             &stubSnapshotReader{row: ownedTeardownRow()},
			resolver:           validResolver(),
			deleter:            &fakeConfigDeleter{},
			writer:             &fakeTeardownWriter{result: VehicleTeardownResult{Removed: true, WasLastVehicle: false, TeslaTokensCleared: false}},
			wantStatus:         http.StatusOK,
			wantDeleterCalled:  true,
			wantWriterCalled:   true,
			wantStreamDeleted:  true,
			wantWasLastVehicle: false,
			wantTokensCleared:  false,
		},
		{
			name:              "config delete failure is non-fatal — teardown still runs",
			reader:            &stubSnapshotReader{row: ownedTeardownRow()},
			resolver:          validResolver(),
			deleter:           &fakeConfigDeleter{err: errors.New("tesla 500")},
			writer:            &fakeTeardownWriter{result: VehicleTeardownResult{Removed: true, WasLastVehicle: true, TeslaTokensCleared: true}},
			wantStatus:        http.StatusOK,
			wantDeleterCalled: true,
			wantWriterCalled:  true,
			wantStreamDeleted: false, // failed → false, but teardown proceeded
			wantWasLastVehicle: true,
			wantTokensCleared:  true,
		},
		{
			name:              "no tesla token — config delete skipped, teardown still runs",
			reader:            &stubSnapshotReader{row: ownedTeardownRow()},
			resolver:          &fakeTokenResolver{err: errors.New("no token")},
			deleter:           &fakeConfigDeleter{},
			writer:            &fakeTeardownWriter{result: VehicleTeardownResult{Removed: true, WasLastVehicle: true, TeslaTokensCleared: true}},
			wantStatus:        http.StatusOK,
			wantDeleterCalled: false,
			wantWriterCalled:  true,
			wantStreamDeleted: false,
			wantWasLastVehicle: true,
			wantTokensCleared:  true,
		},
		{
			name:              "idempotent — teardown reports already-gone, still 200 success",
			reader:            &stubSnapshotReader{row: ownedTeardownRow()},
			resolver:          validResolver(),
			deleter:           &fakeConfigDeleter{},
			writer:            &fakeTeardownWriter{result: VehicleTeardownResult{AlreadyGone: true}},
			wantStatus:        http.StatusOK,
			wantDeleterCalled: true,
			wantWriterCalled:  true,
			wantStreamDeleted: true,
		},
		{
			name:              "cross-user removal rejected — no delete, no teardown",
			reader:            &stubSnapshotReader{row: VehicleSnapshotRow{ID: teardownVehicleID, UserID: "someone-else", VIN: teardownVIN}},
			resolver:          validResolver(),
			deleter:           &fakeConfigDeleter{},
			writer:            &fakeTeardownWriter{},
			wantStatus:        http.StatusForbidden,
			wantDeleterCalled: false,
			wantWriterCalled:  false,
		},
		{
			name:              "unknown vehicle — 404, no teardown",
			reader:            &stubSnapshotReader{err: fmt.Errorf("GetByID: %w", sdk.ErrNotFound)},
			resolver:          validResolver(),
			deleter:           &fakeConfigDeleter{},
			writer:            &fakeTeardownWriter{},
			wantStatus:        http.StatusNotFound,
			wantDeleterCalled: false,
			wantWriterCalled:  false,
		},
		{
			name:              "lookup error — 500, no teardown",
			reader:            &stubSnapshotReader{err: errors.New("db down")},
			resolver:          validResolver(),
			deleter:           &fakeConfigDeleter{},
			writer:            &fakeTeardownWriter{},
			wantStatus:        http.StatusInternalServerError,
			wantDeleterCalled: false,
			wantWriterCalled:  false,
		},
		{
			name:              "teardown error — 500",
			reader:            &stubSnapshotReader{row: ownedTeardownRow()},
			resolver:          validResolver(),
			deleter:           &fakeConfigDeleter{},
			writer:            &fakeTeardownWriter{err: errors.New("tx failed")},
			wantStatus:        http.StatusInternalServerError,
			wantDeleterCalled: true,
			wantWriterCalled:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTeardownHandler(tt.reader, tt.resolver, tt.deleter, tt.writer)
			rec := doTeardown(t, h, http.MethodDelete, teardownVehicleID, true)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.deleter.called != tt.wantDeleterCalled {
				t.Errorf("deleter called = %v, want %v", tt.deleter.called, tt.wantDeleterCalled)
			}
			if tt.writer.called != tt.wantWriterCalled {
				t.Errorf("writer called = %v, want %v", tt.writer.called, tt.wantWriterCalled)
			}

			// Ownership is enforced at the query layer too: whenever the writer
			// runs, it must be scoped to the authenticated caller.
			if tt.writer.called && tt.writer.gotUserID != teardownUserID {
				t.Errorf("writer userID = %q, want %q (owner scope)", tt.writer.gotUserID, teardownUserID)
			}

			if rec.Code != http.StatusOK {
				return
			}
			var body vehicleTeardownResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if !body.Removed {
				t.Error("removed = false, want true")
			}
			if body.StreamConfigDeleted != tt.wantStreamDeleted {
				t.Errorf("streamConfigDeleted = %v, want %v", body.StreamConfigDeleted, tt.wantStreamDeleted)
			}
			if body.WasLastVehicle != tt.wantWasLastVehicle {
				t.Errorf("wasLastVehicle = %v, want %v", body.WasLastVehicle, tt.wantWasLastVehicle)
			}
			if body.TeslaTokensCleared != tt.wantTokensCleared {
				t.Errorf("teslaTokensCleared = %v, want %v", body.TeslaTokensCleared, tt.wantTokensCleared)
			}
			// Owner-action items are always present and honest.
			if !body.VirtualKeyRemoval.Required || body.VirtualKeyRemoval.Automatable {
				t.Error("virtualKeyRemoval must be required and NOT automatable")
			}
			if len(body.VirtualKeyRemoval.Steps) == 0 {
				t.Error("virtualKeyRemoval.steps must be non-empty")
			}
		})
	}
}

// TestVehicleTeardownHandler_DeleteBeforeTeardown asserts the Tesla stream
// config delete happens BEFORE the local teardown (car-offboarding.md §5.1:
// the config delete needs a live token, so it must run before tokens clear).
func TestVehicleTeardownHandler_DeleteBeforeTeardown(t *testing.T) {
	order := 0
	deleter := &fakeConfigDeleter{order: &order}
	writer := &fakeTeardownWriter{result: VehicleTeardownResult{Removed: true, WasLastVehicle: true}, order: &order}
	h := newTeardownHandler(&stubSnapshotReader{row: ownedTeardownRow()}, validResolver(), deleter, writer)

	rec := doTeardown(t, h, http.MethodDelete, teardownVehicleID, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if deleter.seq == 0 || writer.seq == 0 {
		t.Fatal("both deleter and writer must be called")
	}
	if deleter.seq >= writer.seq {
		t.Errorf("config delete (seq %d) must precede teardown (seq %d)", deleter.seq, writer.seq)
	}
	if deleter.gotVIN != teardownVIN {
		t.Errorf("deleter VIN = %q, want %q", deleter.gotVIN, teardownVIN)
	}
}

// TestVehicleTeardownHandler_RevokeURL asserts the response carries the
// owner-confirmed consent-revoke deep link built from the configured client_id.
func TestVehicleTeardownHandler_RevokeURL(t *testing.T) {
	h := newTeardownHandler(
		&stubSnapshotReader{row: ownedTeardownRow()},
		validResolver(),
		&fakeConfigDeleter{},
		&fakeTeardownWriter{result: VehicleTeardownResult{Removed: true, WasLastVehicle: true, TeslaTokensCleared: true}},
	)
	rec := doTeardown(t, h, http.MethodDelete, teardownVehicleID, true)

	var body vehicleTeardownResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.RevokeURL == "" {
		t.Fatal("revokeUrl must be set when a client_id is configured")
	}
	for _, want := range []string{teslaConsentRevokeBase, "revoke_client_id=" + teardownClientID, "back_url="} {
		if !strings.Contains(body.RevokeURL, want) {
			t.Errorf("revokeUrl %q missing %q", body.RevokeURL, want)
		}
	}
}

func TestVehicleTeardownHandler_MethodAndAuth(t *testing.T) {
	newH := func() *VehicleTeardownHandler {
		return newTeardownHandler(
			&stubSnapshotReader{row: ownedTeardownRow()},
			validResolver(),
			&fakeConfigDeleter{},
			&fakeTeardownWriter{result: VehicleTeardownResult{Removed: true}},
		)
	}

	t.Run("GET not allowed", func(t *testing.T) {
		rec := doTeardown(t, newH(), http.MethodGet, teardownVehicleID, true)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rec.Code)
		}
	})

	t.Run("missing auth", func(t *testing.T) {
		rec := doTeardown(t, newH(), http.MethodDelete, teardownVehicleID, false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		h := NewVehicleTeardownHandler(
			&stubTokenValidator{err: errors.New("bad token")},
			&stubSnapshotReader{row: ownedTeardownRow()},
			validResolver(),
			&fakeConfigDeleter{},
			&fakeTeardownWriter{},
			VehicleTeardownConfig{RevokeClientID: teardownClientID},
			discardLogger(),
		)
		rec := doTeardown(t, h, http.MethodDelete, teardownVehicleID, true)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}
