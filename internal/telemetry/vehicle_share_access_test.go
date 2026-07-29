package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// The MYR-184 ACCESS-CONTROL MATRIX.
//
// One table, every per-vehicle endpoint, every tier. This is the file to read
// to answer "what can a `live` viewer do?", and the file that fails when
// somebody widens or narrows a gate without meaning to.
//
// Each endpoint is exercised end-to-end through its real handler and a real
// ServeMux, because the thing being tested is the composition — a gate that is
// correct in isolation but wired with a nil share reader, or mounted without
// its option, is exactly the bug that would ship otherwise.

const (
	shareOwnerUser  = "user-owner"
	shareViewerUser = "user-viewer"
	shareOtherUser  = "user-unrelated"
)

// stubShareReader resolves tiers from a fixed map keyed "userID|vehicleID".
// A missing key is "no grant", surfaced as sdk.ErrNotFound exactly as the
// DB-backed reader does.
type stubShareReader struct {
	tiers map[string]auth.SharePermission
	err   error
	calls int
}

func (s *stubShareReader) SharePermissionFor(_ context.Context, userID, vehicleID string) (auth.SharePermission, error) {
	s.calls++
	if s.err != nil {
		return auth.SharePermission(""), s.err
	}
	if tier, ok := s.tiers[userID+"|"+vehicleID]; ok {
		return tier, nil
	}
	return auth.SharePermission(""), fmt.Errorf("stub: %w", sdk.ErrNotFound)
}

// grantingReader builds a reader that grants one tier to shareViewerUser on the
// fixture vehicle and nothing to anybody else.
func grantingReader(tier auth.SharePermission) *stubShareReader {
	return &stubShareReader{
		tiers: map[string]auth.SharePermission{
			shareViewerUser + "|" + fixtureSnapshotRowID: tier,
		},
	}
}

// TestVehicleAccessFor is the unit-level truth table behind every gate below.
func TestVehicleAccessFor(t *testing.T) {
	const vehicleID = fixtureSnapshotRowID

	tests := []struct {
		name       string
		caller     string
		owner      string
		reader     VehicleShareReader
		minTier    auth.SharePermission
		wantRole   auth.Role
		wantTier   auth.SharePermission
		wantDenied bool
		wantErr    bool
	}{
		{
			name: "owner passes every tier gate without a share lookup",
			// The nil reader is the assertion: an owner must never need one.
			caller: shareOwnerUser, owner: shareOwnerUser, reader: nil,
			minTier: auth.PermissionRides, wantRole: auth.RoleOwner,
		},
		{
			name:   "live viewer passes a live gate",
			caller: shareViewerUser, owner: shareOwnerUser, reader: grantingReader(auth.PermissionLive),
			minTier: auth.PermissionLive, wantRole: auth.RoleViewer, wantTier: auth.PermissionLive,
		},
		{
			name:   "live viewer is refused a live_history gate",
			caller: shareViewerUser, owner: shareOwnerUser, reader: grantingReader(auth.PermissionLive),
			minTier: auth.PermissionLiveHistory, wantDenied: true,
		},
		{
			name:   "live_history viewer passes a live gate (cumulative)",
			caller: shareViewerUser, owner: shareOwnerUser, reader: grantingReader(auth.PermissionLiveHistory),
			minTier: auth.PermissionLive, wantRole: auth.RoleViewer, wantTier: auth.PermissionLiveHistory,
		},
		{
			name:   "live_history viewer is refused a rides gate",
			caller: shareViewerUser, owner: shareOwnerUser, reader: grantingReader(auth.PermissionLiveHistory),
			minTier: auth.PermissionRides, wantDenied: true,
		},
		{
			name:   "rides viewer passes every gate",
			caller: shareViewerUser, owner: shareOwnerUser, reader: grantingReader(auth.PermissionRides),
			minTier: auth.PermissionRides, wantRole: auth.RoleViewer, wantTier: auth.PermissionRides,
		},
		{
			name:   "an unrelated caller is denied even at the floor tier",
			caller: shareOtherUser, owner: shareOwnerUser, reader: grantingReader(auth.PermissionRides),
			minTier: auth.PermissionLive, wantDenied: true,
		},
		{
			name:   "a nil share reader denies every non-owner",
			caller: shareViewerUser, owner: shareOwnerUser, reader: nil,
			minTier: auth.PermissionLive, wantDenied: true,
		},
		{
			name:   "a lookup outage is an error, not a denial",
			caller: shareViewerUser, owner: shareOwnerUser,
			reader:  &stubShareReader{err: errors.New("connection refused")},
			minTier: auth.PermissionLive, wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			access, err := vehicleAccessFor(context.Background(), tt.reader, tt.caller, vehicleID, tt.owner, tt.minTier)

			switch {
			case tt.wantDenied:
				if !errors.Is(err, errNoVehicleAccess) {
					t.Fatalf("err = %v, want errNoVehicleAccess", err)
				}
				return
			case tt.wantErr:
				if err == nil {
					t.Fatal("expected an error")
				}
				if errors.Is(err, errNoVehicleAccess) {
					t.Fatal("an outage must not be reported as a denial")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if access.Role != tt.wantRole {
				t.Errorf("role = %q, want %q", access.Role, tt.wantRole)
			}
			if access.Tier != tt.wantTier {
				t.Errorf("tier = %q, want %q", access.Tier, tt.wantTier)
			}
		})
	}
}

// serveTiered runs one request against a handler built for the given tier and
// returns the status code.
type tieredCase struct {
	// endpoint names the surface under test.
	endpoint string
	// serve builds and runs the endpoint for one caller + reader.
	serve func(t *testing.T, caller string, reader VehicleShareReader) int
	// wantByTier is the status each tier must receive. "none" is a caller
	// with no grant at all.
	wantByTier map[string]int
}

// TestEndpointTierMatrix asserts the per-endpoint tier gates end to end.
//
// The `none` column is the regression guard that matters most: before MYR-184
// every one of these endpoints answered 403 to every non-owner, and the change
// that opened them must not have opened them to somebody with no grant.
func TestEndpointTierMatrix(t *testing.T) {
	ok, forbidden := http.StatusOK, http.StatusForbidden

	cases := []tieredCase{
		{
			endpoint: "GET /api/vehicles/{vehicleId}/snapshot",
			serve:    serveSnapshotFor,
			// The snapshot IS the live-location surface, so the floor tier
			// opens it. P1 location visible to a viewer is the product.
			wantByTier: map[string]int{
				"none": forbidden, "live": ok, "live_history": ok, "rides": ok,
			},
		},
		{
			endpoint: "GET /api/vehicles/{vehicleId}/drives",
			serve:    serveDrivesFor,
			wantByTier: map[string]int{
				"none": forbidden, "live": forbidden, "live_history": ok, "rides": ok,
			},
		},
		{
			endpoint: "POST /api/ride-requests",
			serve:    serveRideCreateFor,
			wantByTier: map[string]int{
				"none": forbidden, "live": forbidden, "live_history": forbidden, "rides": http.StatusCreated,
			},
		},
	}

	tiers := map[string]VehicleShareReader{
		"none":         &stubShareReader{},
		"live":         grantingReader(auth.PermissionLive),
		"live_history": grantingReader(auth.PermissionLiveHistory),
		"rides":        grantingReader(auth.PermissionRides),
	}

	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			// The owner passes every gate, on every endpoint.
			t.Run("owner", func(t *testing.T) {
				got := tc.serve(t, shareOwnerUser, &stubShareReader{})
				if got == forbidden {
					t.Fatalf("the OWNER was refused %s (403) — the gate is inverted", tc.endpoint)
				}
			})
			for tier, reader := range tiers {
				want := tc.wantByTier[tier]
				t.Run("viewer/"+tier, func(t *testing.T) {
					got := tc.serve(t, shareViewerUser, reader)
					if got != want {
						t.Errorf("%s with tier %q: status %d, want %d", tc.endpoint, tier, got, want)
					}
				})
			}
		})
	}
}

func serveSnapshotFor(t *testing.T, caller string, reader VehicleShareReader) int {
	t.Helper()
	h := NewVehicleSnapshotHandler(
		&stubTokenValidator{userID: caller},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)},
		discardLogger(),
		WithSnapshotShareReader(reader),
	)
	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicles/{vehicleId}/snapshot", h)
	return doTieredRequest(t, mux, http.MethodGet, "/api/vehicles/"+fixtureSnapshotRowID+"/snapshot", nil)
}

func serveDrivesFor(t *testing.T, caller string, reader VehicleShareReader) int {
	t.Helper()
	h := NewVehicleDrivesHandler(
		&stubTokenValidator{userID: caller},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)},
		&stubDriveLister{},
		discardLogger(),
		WithDrivesShareReader(reader),
	)
	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicles/{vehicleId}/drives", h)
	return doTieredRequest(t, mux, http.MethodGet, "/api/vehicles/"+fixtureSnapshotRowID+"/drives", nil)
}

func serveRideCreateFor(t *testing.T, caller string, reader VehicleShareReader) int {
	t.Helper()
	h := NewRideRequestHandler(
		&stubTokenValidator{userID: caller},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)},
		&fakeRideStore{},
		nil,
		discardLogger(),
		WithRideShareReader(reader),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/ride-requests", h.ServeCreate)

	return doTieredRequest(t, mux, http.MethodPost, "/api/ride-requests", validRideCreateBody())
}

// doTieredRequest issues one authenticated request and returns the status.
func doTieredRequest(t *testing.T, mux *http.ServeMux, method, path string, body []byte) int {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, reader)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

// validRideCreateBody is a minimal valid create body for the fixture vehicle.
// The tier matrix cares only about the gate, so the body just has to clear
// validation — it is the same shape ride_request_handler_test.go's validBody
// uses.
func validRideCreateBody() []byte {
	return []byte(`{"vehicleId":"` + fixtureSnapshotRowID +
		`","pickup":{"lat":37.79,"lng":-122.39,"label":"Home"},` +
		`"dropoff":{"lat":37.77,"lng":-122.39,"label":"Caltrain"}}`)
}
