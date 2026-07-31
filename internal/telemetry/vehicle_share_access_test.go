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

// stubShareReader resolves GRANTS from a fixed map keyed "userID|vehicleID".
// A missing key is "no grant", surfaced as sdk.ErrNotFound exactly as the
// DB-backed reader does — which is also what it does for a SUSPENDED grant,
// since the statement excludes those. A stub that CAN return a suspended grant
// is therefore modelling a future implementation, and the second gate in
// vehicleAccessFor is what must catch it.
type stubShareReader struct {
	grants map[string]auth.ShareGrant
	err    error
	calls  int
}

func (s *stubShareReader) ShareGrantFor(_ context.Context, userID, vehicleID string) (auth.ShareGrant, error) {
	s.calls++
	if s.err != nil {
		return auth.ShareGrant{}, s.err
	}
	if g, ok := s.grants[userID+"|"+vehicleID]; ok {
		return g, nil
	}
	return auth.ShareGrant{}, fmt.Errorf("stub: %w", sdk.ErrNotFound)
}

// grantingReader builds a reader that grants one capability set to
// shareViewerUser on the fixture vehicle and nothing to anybody else.
func grantingReader(grant auth.ShareGrant) *stubShareReader {
	return &stubShareReader{
		grants: map[string]auth.ShareGrant{
			shareViewerUser + "|" + fixtureSnapshotRowID: grant,
		},
	}
}

// baseGrant / rideGrant name the two capability sets the product now has.
var (
	baseGrant = auth.ShareGrant{}
	rideGrant = auth.ShareGrant{AllowRides: true}
)

// TestVehicleAccessFor is the unit-level truth table behind every gate below.
func TestVehicleAccessFor(t *testing.T) {
	const vehicleID = fixtureSnapshotRowID

	tests := []struct {
		name       string
		caller     string
		owner      string
		reader     VehicleShareReader
		need       shareCapability
		wantRole   auth.Role
		wantGrant  auth.ShareGrant
		wantDenied bool
		wantErr    bool
	}{
		{
			name: "owner passes every gate without a share lookup",
			// The nil reader is the assertion: an owner must never need one.
			caller: shareOwnerUser, owner: shareOwnerUser, reader: nil,
			need: capRides, wantRole: auth.RoleOwner,
		},
		{
			name:   "base grant passes a base gate",
			caller: shareViewerUser, owner: shareOwnerUser, reader: grantingReader(baseGrant),
			need: capBase, wantRole: auth.RoleViewer, wantGrant: baseGrant,
		},
		{
			name:   "base grant is refused a rides gate",
			caller: shareViewerUser, owner: shareOwnerUser, reader: grantingReader(baseGrant),
			need: capRides, wantDenied: true,
		},
		{
			name:   "ride grant passes a base gate",
			caller: shareViewerUser, owner: shareOwnerUser, reader: grantingReader(rideGrant),
			need: capBase, wantRole: auth.RoleViewer, wantGrant: rideGrant,
		},
		{
			name:   "ride grant passes a rides gate",
			caller: shareViewerUser, owner: shareOwnerUser, reader: grantingReader(rideGrant),
			need: capRides, wantRole: auth.RoleViewer, wantGrant: rideGrant,
		},
		{
			// MYR-369 SUSPENSION INVARIANT at the gate. The DB statement
			// already excludes suspended rows, so a reader that returns
			// one models a stub or an edited WHERE clause — and it must
			// still be denied, at EVERY gate including the floor.
			name:   "a suspended grant is denied even at the base gate",
			caller: shareViewerUser, owner: shareOwnerUser,
			reader: grantingReader(auth.ShareGrant{Suspended: true}),
			need:   capBase, wantDenied: true,
		},
		{
			// The ride flag does not survive suspension — the whole point
			// of routing through GrantsRides rather than the bare field.
			name:   "a suspended RIDE grant is denied a rides gate",
			caller: shareViewerUser, owner: shareOwnerUser,
			reader: grantingReader(auth.ShareGrant{AllowRides: true, Suspended: true}),
			need:   capRides, wantDenied: true,
		},
		{
			name:   "an unrelated caller is denied even at the base gate",
			caller: shareOtherUser, owner: shareOwnerUser, reader: grantingReader(rideGrant),
			need: capBase, wantDenied: true,
		},
		{
			name:   "a nil share reader denies every non-owner",
			caller: shareViewerUser, owner: shareOwnerUser, reader: nil,
			need: capBase, wantDenied: true,
		},
		{
			name:   "a lookup outage is an error, not a denial",
			caller: shareViewerUser, owner: shareOwnerUser,
			reader: &stubShareReader{err: errors.New("connection refused")},
			need:   capBase, wantErr: true,
		},
		{
			// An unrecognized requirement satisfies nothing: a gate that
			// cannot be evaluated must not open.
			name:   "an unknown capability requirement denies",
			caller: shareViewerUser, owner: shareOwnerUser, reader: grantingReader(rideGrant),
			need: shareCapability(99), wantDenied: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			access, err := vehicleAccessFor(context.Background(), tt.reader, tt.caller, vehicleID, tt.owner, tt.need)

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
			if access.Grant != tt.wantGrant {
				t.Errorf("grant = %+v, want %+v", access.Grant, tt.wantGrant)
			}
		})
	}
}

// TestVehicleAccessForOwnerOnly pins the drives gate (MYR-369): no grant of any
// shape opens an owner-only surface, and the helper takes no reader at all so
// there is nothing for a future edit to widen.
func TestVehicleAccessForOwnerOnly(t *testing.T) {
	ctx := context.Background()

	access, err := vehicleAccessForOwnerOnly(ctx, shareOwnerUser, shareOwnerUser)
	if err != nil {
		t.Fatalf("the owner was refused their own vehicle: %v", err)
	}
	if access.Role != auth.RoleOwner {
		t.Errorf("role = %q, want owner", access.Role)
	}

	for _, caller := range []string{shareViewerUser, shareOtherUser, ""} {
		if _, err := vehicleAccessForOwnerOnly(ctx, caller, shareOwnerUser); !errors.Is(err, errNoVehicleAccess) {
			t.Errorf("non-owner %q: err = %v, want errNoVehicleAccess", caller, err)
		}
	}
}

// serveTiered runs one request against a handler built for the given tier and
// returns the status code.
type tieredCase struct {
	// endpoint names the surface under test.
	endpoint string
	// serve builds and runs the endpoint for one caller + reader.
	serve func(t *testing.T, caller string, reader VehicleShareReader) int
	// wantByGrant is the status each grant shape must receive. "none" is a
	// caller with no grant at all.
	wantByGrant map[string]int
}

// TestEndpointGrantMatrix asserts the per-endpoint capability gates end to end.
//
// The `none` column is the regression guard that matters most: before MYR-184
// every one of these endpoints answered 403 to every non-owner, and the change
// that opened them must not have opened them to somebody with no grant.
//
// The `suspended` columns are the MYR-369 guard: a suspended grant must be
// refused by EVERY endpoint, whatever capability it carries. In production it
// never reaches these gates at all (it is absent from the access set and the
// grant statement returns no row), so these rows exercise the second,
// independent gate — the one that still holds if a WHERE clause is edited.
func TestEndpointGrantMatrix(t *testing.T) {
	ok, forbidden := http.StatusOK, http.StatusForbidden

	cases := []tieredCase{
		{
			endpoint: "GET /api/vehicles/{vehicleId}/snapshot",
			serve:    serveSnapshotFor,
			// The snapshot IS the live-location surface, so the base grant
			// opens it. P1 location visible to a viewer is the product.
			wantByGrant: map[string]int{
				"none": forbidden, "base": ok, "rides": ok,
				"suspended-base": forbidden, "suspended-rides": forbidden,
			},
		},
		{
			endpoint: "GET /api/vehicles/{vehicleId}/drives",
			serve:    serveDrivesFor,
			// MYR-369: OWNER-ONLY AGAIN. No grant opens this, including a
			// legacy live_history one — the capability was removed from
			// the product, not merely renamed.
			wantByGrant: map[string]int{
				"none": forbidden, "base": forbidden, "rides": forbidden,
				"suspended-base": forbidden, "suspended-rides": forbidden,
			},
		},
		{
			endpoint: "POST /api/ride-requests",
			serve:    serveRideCreateFor,
			wantByGrant: map[string]int{
				"none": forbidden, "base": forbidden, "rides": http.StatusCreated,
				"suspended-base": forbidden, "suspended-rides": forbidden,
			},
		},
	}

	readers := map[string]VehicleShareReader{
		"none":            &stubShareReader{},
		"base":            grantingReader(baseGrant),
		"rides":           grantingReader(rideGrant),
		"suspended-base":  grantingReader(auth.ShareGrant{Suspended: true}),
		"suspended-rides": grantingReader(auth.ShareGrant{AllowRides: true, Suspended: true}),
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
			for shape, reader := range readers {
				want := tc.wantByGrant[shape]
				t.Run("viewer/"+shape, func(t *testing.T) {
					got := tc.serve(t, shareViewerUser, reader)
					if got != want {
						t.Errorf("%s with grant %q: status %d, want %d", tc.endpoint, shape, got, want)
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

// serveDrivesFor deliberately DROPS the reader (MYR-369): the drives handler no
// longer has a share-reader seam at all, which is the structural half of making
// the surface owner-only — there is no wiring line to re-open it with. The
// parameter stays so the matrix can drive every endpoint uniformly, and its
// being ignored is precisely what the `forbidden` column proves.
func serveDrivesFor(t *testing.T, caller string, _ VehicleShareReader) int {
	t.Helper()
	h := NewVehicleDrivesHandler(
		&stubTokenValidator{userID: caller},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)},
		&stubDriveLister{},
		discardLogger(),
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
