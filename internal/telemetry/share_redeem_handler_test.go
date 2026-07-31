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

	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// --- Test doubles ---

// fakeShareRedeemStore scripts the redemption outcome and records the code the
// handler submitted (so normalization can be asserted).
type fakeShareRedeemStore struct {
	grants     []ShareGrantRow
	redeemErr  error
	gotCode    string
	gotUser    string
	ownerName  string
	ownerErr   error
	redeemCall int
}

func (f *fakeShareRedeemStore) RedeemCode(_ context.Context, code, redeemerID string) ([]ShareGrantRow, error) {
	f.redeemCall++
	f.gotCode, f.gotUser = code, redeemerID
	return f.grants, f.redeemErr
}

func (f *fakeShareRedeemStore) OwnerFirstName(_ context.Context, _ string) (string, error) {
	if f.ownerErr != nil {
		return "", f.ownerErr
	}
	if f.ownerName == "" {
		return "Alex", nil
	}
	return f.ownerName, nil
}

// fakeSharedLister returns catalog rows for whatever ids it is asked about.
type fakeSharedLister struct {
	rows   []SharedVehicleRow
	err    error
	gotIDs []string
}

func (f *fakeSharedLister) ListSharedByUser(_ context.Context, _ string) ([]SharedVehicleRow, error) {
	return f.rows, f.err
}

func (f *fakeSharedLister) ListSharedByIDs(_ context.Context, _ string, ids []string) ([]SharedVehicleRow, error) {
	f.gotIDs = ids
	return f.rows, f.err
}

// sharedCatalogRow is a viewer-shaped catalog row for the fixture vehicle,
// carrying the grant's ride capability (MYR-369 — the row no longer carries a
// tier, and the wire `sharePermission` is derived from this bool).
func sharedCatalogRow(allowRides bool) SharedVehicleRow {
	return SharedVehicleRow{
		VehicleCatalogRow: VehicleCatalogRow{
			ID:             fixtureSnapshotRowID,
			VIN:            "5YJ3E1EA1PF000001",
			Name:           "Alex's Model 3",
			Model:          "Model 3",
			Year:           2024,
			Color:          "Pearl White",
			LicensePlate:   "8ABC123",
			Status:         "parked",
			ChargeLevel:    72,
			EstimatedRange: 210,
			LastUpdated:    time.Date(2026, 7, 29, 15, 4, 5, 0, time.UTC),
		},
		AllowRides: allowRides,
	}
}

// newRedeemMux mounts the redeem route.
func newRedeemMux(caller string, redeem ShareRedeemStore, lister SharedVehicleLister, inv AccessCacheInvalidator) *http.ServeMux { //nolint:unparam // the redeeming identity is named at each call site on purpose — these are authorization tests
	h := NewShareRedeemHandler(&stubTokenValidator{userID: caller}, redeem, lister, inv, discardLogger())
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/invites/redeem", h.ServeHTTP)
	return mux
}

const redeemPath = "/api/invites/redeem"

func TestShareRedeemHandler_Success(t *testing.T) {
	redeem := &fakeShareRedeemStore{
		grants:    []ShareGrantRow{{VehicleID: fixtureSnapshotRowID, OwnerUserID: shareOwnerUser, AllowRides: true}},
		ownerName: "Alex",
	}
	lister := &fakeSharedLister{rows: []SharedVehicleRow{sharedCatalogRow(true)}}
	invalidator := &fakeAccessInvalidator{}
	mux := newRedeemMux(shareViewerUser, redeem, lister, invalidator)

	rec := doShareRequest(t, mux, http.MethodPost, redeemPath, `{"code":"RBO246"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		OwnerFirstName string           `json:"ownerFirstName"`
		Vehicles       []map[string]any `json:"vehicles"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.OwnerFirstName != "Alex" {
		t.Errorf("ownerFirstName = %q, want %q", body.OwnerFirstName, "Alex")
	}
	if len(body.Vehicles) != 1 {
		t.Fatalf("got %d vehicles, want 1", len(body.Vehicles))
	}

	row := body.Vehicles[0]
	if row["role"] != "viewer" {
		t.Errorf("role = %v, want viewer", row["role"])
	}
	if row["sharePermission"] != "rides" {
		t.Errorf("sharePermission = %v, want rides", row["sharePermission"])
	}

	// The redeemer's join screen renders "{Owner}'s {Vehicle}", so the vehicle
	// nickname comes through (MYR-184) — and must, since `name` is `required`
	// in vehicle-summary.schema.json and this row is a VehicleSummary.
	if row["name"] != "Alex's Model 3" {
		t.Errorf("name = %v, want the vehicle nickname on the join screen", row["name"])
	}
	// What the viewer mask DOES withhold is the owner-facing invite shape —
	// no label, no code, no invite id — asserted below.
	for _, withheld := range []string{"label", "code", "inviteId", "vin"} {
		if _, leaked := row[withheld]; leaked {
			t.Errorf("the redeemer's row carried %q — the viewer projection leaked owner-facing state", withheld)
		}
	}
	// ...and the fields a rider genuinely needs are present.
	for _, want := range []string{"vehicleId", "model", "color", "licensePlate", "vinLast4", "chargeLevel"} {
		if _, ok := row[want]; !ok {
			t.Errorf("the viewer row is missing %q", want)
		}
	}

	// The redeemer's cached access set must be busted so the car shows up on
	// their very next GET /api/vehicles.
	if len(invalidator.busted) != 1 || invalidator.busted[0] != shareViewerUser {
		t.Errorf("busted = %v, want [%s]", invalidator.busted, shareViewerUser)
	}
	// The catalog read is narrowed to what THIS redemption granted.
	if len(lister.gotIDs) != 1 || lister.gotIDs[0] != fixtureSnapshotRowID {
		t.Errorf("catalog read asked for %v, want the granted set", lister.gotIDs)
	}
	// The redeemer is the JWT subject, never client-supplied.
	if redeem.gotUser != shareViewerUser {
		t.Errorf("redeemed as %q, want the token subject", redeem.gotUser)
	}
}

func TestShareRedeemHandler_CodeNormalization(t *testing.T) {
	tests := []struct {
		name       string
		submitted  string
		wantCode   string
		wantStatus int
	}{
		{"already normalized", "RBO246", "RBO246", http.StatusOK},
		{"lower case is upper-cased", "rbo246", "RBO246", http.StatusOK},
		{"separators are stripped", "rb o-24_6", "RBO246", http.StatusOK},
		// Still malformed after normalization → 400, and the store is never
		// asked. 400 is deliberately NOT 404: "you sent nonsense" and "that
		// code grants you nothing" are different answers.
		{"too short", "ABC", "", http.StatusBadRequest},
		{"too long", "ABCDEFG", "", http.StatusBadRequest},
		{"empty", "", "", http.StatusBadRequest},
		{"all separators", "------", "", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redeem := &fakeShareRedeemStore{
				grants: []ShareGrantRow{{VehicleID: fixtureSnapshotRowID, OwnerUserID: shareOwnerUser, AllowRides: false}},
			}
			lister := &fakeSharedLister{rows: []SharedVehicleRow{sharedCatalogRow(false)}}
			mux := newRedeemMux(shareViewerUser, redeem, lister, nil)

			body, err := json.Marshal(map[string]string{"code": tt.submitted})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			rec := doShareRequest(t, mux, http.MethodPost, redeemPath, string(body))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d. Body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if redeem.gotCode != tt.wantCode {
				t.Errorf("store received %q, want %q", redeem.gotCode, tt.wantCode)
			}
			if tt.wantStatus == http.StatusBadRequest && redeem.redeemCall != 0 {
				t.Error("a malformed code reached the store")
			}
		})
	}
}

func TestShareRedeemHandler_Failures(t *testing.T) {
	tests := []struct {
		name       string
		redeemErr  error
		wantStatus int
		wantCode   string
	}{
		{
			// The three dead-code cases all arrive here as sdk.ErrNotFound
			// and must be answered identically.
			name:       "unknown / expired / consumed",
			redeemErr:  fmt.Errorf("stub: %w", sdk.ErrNotFound),
			wantStatus: http.StatusNotFound, wantCode: "not_found",
		},
		{
			name:       "owner redeeming their own code",
			redeemErr:  ErrShareSelfRedeem,
			wantStatus: http.StatusConflict, wantCode: "conflict",
		},
		{
			name:       "already granted through another invite",
			redeemErr:  ErrShareAlreadyGranted,
			wantStatus: http.StatusConflict, wantCode: "conflict",
		},
		{
			name:       "store outage",
			redeemErr:  errors.New("connection refused"),
			wantStatus: http.StatusInternalServerError, wantCode: "internal_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redeem := &fakeShareRedeemStore{redeemErr: tt.redeemErr}
			mux := newRedeemMux(shareViewerUser, redeem, &fakeSharedLister{}, nil)

			rec := doShareRequest(t, mux, http.MethodPost, redeemPath, `{"code":"RBO246"}`)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d. Body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			body := decodeShareBody(t, rec)
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("no error envelope in %s", rec.Body.String())
			}
			if errObj["code"] != tt.wantCode {
				t.Errorf("error code = %v, want %q", errObj["code"], tt.wantCode)
			}

			// The submitted code is a live bearer credential and must never
			// be echoed back — an error message repeating it is a
			// confirmation oracle for an enumerating caller.
			if strings.Contains(rec.Body.String(), "RBO246") {
				t.Errorf("the submitted code was echoed into the error body: %s", rec.Body.String())
			}
		})
	}
}

// TestShareRedeemHandler_NotFoundBodiesAreIdentical is the anti-enumeration
// assertion stated as a property rather than a per-case expectation: an
// attacker must not be able to tell an unknown code from an expired one or
// from one somebody else already used.
func TestShareRedeemHandler_NotFoundBodiesAreIdentical(t *testing.T) {
	// All three cases reach the handler as the SAME error — that is the
	// store's contract — so the handler cannot distinguish them even if it
	// wanted to. This test pins the property at the HTTP boundary.
	var bodies []string
	for i := 0; i < 3; i++ {
		redeem := &fakeShareRedeemStore{redeemErr: fmt.Errorf("case %d: %w", i, sdk.ErrNotFound)}
		mux := newRedeemMux(shareViewerUser, redeem, &fakeSharedLister{}, nil)
		rec := doShareRequest(t, mux, http.MethodPost, redeemPath, `{"code":"RBO246"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("case %d: status %d, want 404", i, rec.Code)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Errorf("404 bodies differ:\n %s\n %s", bodies[0], bodies[i])
		}
	}
}

// TestShareRedeemHandler_RateLimited proves the endpoint stops answering after
// the per-user budget. Without this the 36^6 code space is brute-forceable.
func TestShareRedeemHandler_RateLimited(t *testing.T) {
	redeem := &fakeShareRedeemStore{redeemErr: fmt.Errorf("stub: %w", sdk.ErrNotFound)}
	mux := newRedeemMux(shareViewerUser, redeem, &fakeSharedLister{}, nil)

	// Exhaust the budget with guesses.
	for i := 0; i < redeemRateLimit; i++ {
		rec := doShareRequest(t, mux, http.MethodPost, redeemPath, `{"code":"AAAAAA"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("attempt %d: status %d, want 404 (still under the cap)", i+1, rec.Code)
		}
	}

	rec := doShareRequest(t, mux, http.MethodPost, redeemPath, `{"code":"AAAAAA"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d: status %d, want 429", redeemRateLimit+1, rec.Code)
	}
	body := decodeShareBody(t, rec)
	if errObj, ok := body["error"].(map[string]any); ok && errObj["code"] != "rate_limited" {
		t.Errorf("error code = %v, want rate_limited", errObj["code"])
	}
	if redeem.redeemCall != redeemRateLimit {
		t.Errorf("the store was called %d times, want %d — the limiter must short-circuit BEFORE the lookup",
			redeem.redeemCall, redeemRateLimit)
	}
}

func TestShareRedeemHandler_AuthRequired(t *testing.T) {
	mux := newRedeemMux(shareViewerUser, &fakeShareRedeemStore{}, &fakeSharedLister{}, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, redeemPath,
		strings.NewReader(`{"code":"RBO246"}`))
	// Deliberately no Authorization header.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401 for a request with no bearer token", rec.Code)
	}
}
