package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- fakes -----------------------------------------------------------------

type fakeTombstoneClearer struct {
	cleared      bool
	err          error
	called       bool
	gotUserID    string
	gotVehicleID string
}

func (f *fakeTombstoneClearer) ClearTombstone(_ context.Context, userID, teslaVehicleID string) (bool, error) {
	f.called = true
	f.gotUserID = userID
	f.gotVehicleID = teslaVehicleID
	if f.err != nil {
		return false, f.err
	}
	return f.cleared, nil
}

type fakeReaddProvisioner struct {
	provisioned  bool
	err          error
	called       bool
	gotUserID    string
	gotVehicleID string
}

func (f *fakeReaddProvisioner) ProvisionReaddedVehicle(_ context.Context, userID, teslaVehicleID string) (bool, error) {
	f.called = true
	f.gotUserID = userID
	f.gotVehicleID = teslaVehicleID
	return f.provisioned, f.err
}

// readdAuth is a minimal tokenValidator fake for the re-add handler tests.
type readdAuth struct {
	userID string
	err    error
}

func (a readdAuth) ValidateToken(context.Context, string) (string, error) {
	return a.userID, a.err
}

// newReaddRequest builds a POST re-add request with the teslaVehicleId path
// value already resolved (the handler reads r.PathValue).
func newReaddRequest(teslaVehicleID, bearer string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/tesla/vehicles/"+teslaVehicleID+"/re-add", nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	r.SetPathValue("teslaVehicleId", teslaVehicleID)
	return r
}

func decodeReaddResponse(t *testing.T, body []byte) vehicleReaddResponse {
	t.Helper()
	var resp vehicleReaddResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	return resp
}

func TestVehicleReaddHandler(t *testing.T) {
	const uid, tvid = "cowner001", "vid-123"

	t.Run("deliberate re-add clears tombstone then provisions", func(t *testing.T) {
		clearer := &fakeTombstoneClearer{cleared: true}
		prov := &fakeReaddProvisioner{provisioned: true}
		h := NewVehicleReaddHandler(readdAuth{userID: uid}, clearer, prov, discardLogger())

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newReaddRequest(tvid, "tok"))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		resp := decodeReaddResponse(t, rec.Body.Bytes())
		if !resp.Readded || !resp.WasTombstoned || !resp.Provisioned {
			t.Errorf("response = %+v, want all true", resp)
		}
		// The tombstone clear MUST happen with the caller's own id (fail-closed
		// ownership) and BEFORE provisioning.
		if !clearer.called || clearer.gotUserID != uid || clearer.gotVehicleID != tvid {
			t.Errorf("clearer called=%v user=%q vid=%q, want (true,%q,%q)",
				clearer.called, clearer.gotUserID, clearer.gotVehicleID, uid, tvid)
		}
		if !prov.called || prov.gotUserID != uid || prov.gotVehicleID != tvid {
			t.Errorf("provisioner called=%v user=%q vid=%q, want (true,%q,%q)",
				prov.called, prov.gotUserID, prov.gotVehicleID, uid, tvid)
		}
	})

	t.Run("idempotent: no tombstone present is a clean 200 no-op", func(t *testing.T) {
		clearer := &fakeTombstoneClearer{cleared: false}
		prov := &fakeReaddProvisioner{provisioned: false}
		h := NewVehicleReaddHandler(readdAuth{userID: uid}, clearer, prov, discardLogger())

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newReaddRequest(tvid, "tok"))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		resp := decodeReaddResponse(t, rec.Body.Bytes())
		if !resp.Readded {
			t.Errorf("readded = false, want true (post-state is un-trapped either way)")
		}
		if resp.WasTombstoned {
			t.Errorf("wasTombstoned = true, want false (nothing was cleared)")
		}
	})

	t.Run("provision failure never fails the re-add (tombstone still cleared)", func(t *testing.T) {
		clearer := &fakeTombstoneClearer{cleared: true}
		prov := &fakeReaddProvisioner{err: errors.New("fleet list unavailable")}
		h := NewVehicleReaddHandler(readdAuth{userID: uid}, clearer, prov, discardLogger())

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newReaddRequest(tvid, "tok"))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (un-trap is authoritative)", rec.Code)
		}
		resp := decodeReaddResponse(t, rec.Body.Bytes())
		if !resp.Readded || !resp.WasTombstoned || resp.Provisioned {
			t.Errorf("response = %+v, want readded+wasTombstoned true, provisioned false", resp)
		}
	})

	t.Run("nil provisioner: tombstone cleared, provisioned=false", func(t *testing.T) {
		clearer := &fakeTombstoneClearer{cleared: true}
		h := NewVehicleReaddHandler(readdAuth{userID: uid}, clearer, nil, discardLogger())

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newReaddRequest(tvid, "tok"))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		resp := decodeReaddResponse(t, rec.Body.Bytes())
		if !resp.Readded || resp.Provisioned {
			t.Errorf("response = %+v, want readded true, provisioned false", resp)
		}
	})

	t.Run("missing bearer is 401 and never touches the registry", func(t *testing.T) {
		clearer := &fakeTombstoneClearer{}
		h := NewVehicleReaddHandler(readdAuth{userID: uid}, clearer, nil, discardLogger())

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newReaddRequest(tvid, ""))

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if clearer.called {
			t.Errorf("ClearTombstone called on an unauthenticated request (must fail closed)")
		}
	})

	t.Run("invalid token is 401 and never touches the registry", func(t *testing.T) {
		clearer := &fakeTombstoneClearer{}
		h := NewVehicleReaddHandler(readdAuth{err: errors.New("bad token")}, clearer, nil, discardLogger())

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newReaddRequest(tvid, "tok"))

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if clearer.called {
			t.Errorf("ClearTombstone called with an invalid token (must fail closed)")
		}
	})

	t.Run("missing teslaVehicleId is 400", func(t *testing.T) {
		clearer := &fakeTombstoneClearer{}
		h := NewVehicleReaddHandler(readdAuth{userID: uid}, clearer, nil, discardLogger())

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newReaddRequest("", "tok"))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if clearer.called {
			t.Errorf("ClearTombstone called with an empty teslaVehicleId")
		}
	})

	t.Run("clear error surfaces as 500 (atomic: nothing provisioned)", func(t *testing.T) {
		clearer := &fakeTombstoneClearer{err: errors.New("db down")}
		prov := &fakeReaddProvisioner{}
		h := NewVehicleReaddHandler(readdAuth{userID: uid}, clearer, prov, discardLogger())

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newReaddRequest(tvid, "tok"))

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if prov.called {
			t.Errorf("provisioner called after a clear failure (must not provision on error)")
		}
	})

	t.Run("non-POST method is 405", func(t *testing.T) {
		h := NewVehicleReaddHandler(readdAuth{userID: uid}, &fakeTombstoneClearer{}, nil, discardLogger())
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/tesla/vehicles/"+tvid+"/re-add", nil)
		r.SetPathValue("teslaVehicleId", tvid)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})
}

// TestVehicleReaddHandler_ErrorEnvelope pins the typed error envelope shape so a
// 401 carries the auth_failed code (rest-api.md §4.1).
func TestVehicleReaddHandler_ErrorEnvelope(t *testing.T) {
	h := NewVehicleReaddHandler(readdAuth{err: errors.New("x")}, &fakeTombstoneClearer{}, nil, discardLogger())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newReaddRequest("vid-9", "tok"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if !strings.Contains(rec.Body.String(), "auth_failed") {
		t.Errorf("body = %s, want auth_failed code", rec.Body.String())
	}
}
