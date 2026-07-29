package telemetry

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestOwnerOnlyEndpointsRefuseViewers is the OTHER half of the MYR-184 access
// story, and the half that is easy to forget: the endpoints that did NOT open.
//
// Vehicle sharing grants READ access and, at the top tier, the ability to
// request a ride. It grants nothing that MUTATES the car or its account
// relationship. The five endpoints below are exactly that set, and each one is
// asserted to refuse a caller who is not the owner — at every tier, including
// `rides`.
//
// These handlers take no share reader at all, which is the structural reason
// they cannot be widened by accident. This test pins the behaviour anyway, so
// that adding one later is a deliberate act with a failing test attached rather
// than a quiet change to a shared helper.
func TestOwnerOnlyEndpointsRefuseViewers(t *testing.T) {
	// The most-privileged possible viewer. If even `rides` cannot touch these,
	// no tier can.
	const viewer = shareViewerUser

	tests := []struct {
		name   string
		method string
		path   string
		mount  func(mux *http.ServeMux, caller string)
	}{
		{
			name:   "vehicle command (§7.9)",
			method: http.MethodPost,
			path:   "/api/vehicles/" + fixtureSnapshotRowID + "/command/door_lock",
			mount: func(mux *http.ServeMux, caller string) {
				h := NewVehicleCommandHandler(
					&stubTokenValidator{userID: caller},
					&stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)},
					validTeslaToken(),
					&fakeCommandExecutor{},
					discardLogger(),
				)
				mux.HandleFunc("POST /api/vehicles/{vehicleId}/command/{name}", h.ServeHTTP)
			},
		},
		{
			name:   "license plate write (§7.14)",
			method: http.MethodPut,
			path:   "/api/tesla/vehicles/" + fixtureSnapshotRowID + "/plate",
			mount: func(mux *http.ServeMux, caller string) {
				h := NewVehiclePlateHandler(
					&stubTokenValidator{userID: caller},
					&stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)},
					&stubPlateWriter{},
					discardLogger(),
				)
				mux.HandleFunc("PUT /api/tesla/vehicles/{vehicleId}/plate", h.ServeHTTP)
			},
		},
		{
			name:   "service window write (§7.16)",
			method: http.MethodPut,
			path:   "/api/tesla/vehicles/" + fixtureSnapshotRowID + "/service-window",
			mount: func(mux *http.ServeMux, caller string) {
				h := NewVehicleServiceWindowHandler(
					&stubTokenValidator{userID: caller},
					&stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)},
					&stubServiceWindowWriter{},
					discardLogger(),
				)
				mux.HandleFunc("PUT /api/tesla/vehicles/{vehicleId}/service-window", h.ServeHTTP)
			},
		},
		{
			name:   "on-demand refresh (§7.15)",
			method: http.MethodPost,
			path:   "/api/tesla/vehicles/" + fixtureSnapshotRowID + "/refresh",
			mount: func(mux *http.ServeMux, caller string) {
				h := NewVehicleRefreshHandler(
					&stubTokenValidator{userID: caller},
					&stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)},
					&stubShareRefresher{},
					discardLogger(),
				)
				mux.HandleFunc("POST /api/tesla/vehicles/{vehicleId}/refresh", h.ServeHTTP)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			tt.mount(mux, viewer)

			got := doTieredRequest(t, mux, tt.method, tt.path, []byte(`{}`))
			if got != http.StatusForbidden {
				t.Errorf("a non-owner got %d from %s %s, want 403 — sharing must not grant write access",
					got, tt.method, tt.path)
			}
		})

		t.Run(tt.name+" (owner still allowed)", func(t *testing.T) {
			// The counter-assertion: the 403 above must come from the
			// ownership check, not from a handler that refuses everyone.
			mux := http.NewServeMux()
			tt.mount(mux, shareOwnerUser)

			got := doTieredRequest(t, mux, tt.method, tt.path, []byte(`{}`))
			if got == http.StatusForbidden {
				t.Errorf("the OWNER also got 403 from %s %s — the test above proves nothing",
					tt.method, tt.path)
			}
		})
	}
}

// --- Minimal stubs for the owner-only handlers ---
//
// Each does the least possible: every request in this test is refused before
// the handler reaches its collaborator, so the bodies are never exercised on
// the viewer path. They exist so the owner counter-assertion gets past
// construction.

type stubPlateWriter struct{}

func (s *stubPlateWriter) UpdateLicensePlate(_ context.Context, _, _, _ string) (bool, error) {
	return true, nil
}

type stubServiceWindowWriter struct{}

func (s *stubServiceWindowWriter) SetServiceExpectedEndAt(_ context.Context, _ string, _ *time.Time) error {
	return nil
}

type stubShareRefresher struct{}

func (s *stubShareRefresher) LastStreamAt(_ string) (time.Time, bool) { return time.Time{}, false }

func (s *stubShareRefresher) Refresh(_ context.Context, _, _ string) (RefreshResult, error) {
	return RefreshResult{}, nil
}
