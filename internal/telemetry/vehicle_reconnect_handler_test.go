package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// MYR-592 — POST /api/tesla/vehicles/{vehicleId}/reconnect (rest-api.md §7.28).

const (
	reconnectUserID    = "user_owner"
	reconnectVehicleID = "veh_reconnect"
	reconnectVIN       = "5YJ3E1EA1PF000042"
)

// fakeConfigPusher records the re-install without ever reaching Tesla.
type fakeConfigPusher struct {
	err      error
	called   bool
	gotVIN   string
	gotToken string
	order    *int
	seq      int
}

func (f *fakeConfigPusher) PushForVIN(_ context.Context, token, vin string) error {
	f.called = true
	f.gotToken = token
	f.gotVIN = vin
	if f.order != nil {
		*f.order++
		f.seq = *f.order
	}
	return f.err
}

// fakeSuspensionClearer records the episode clear.
type fakeSuspensionClearer struct {
	err          error
	called       bool
	gotVehicleID string
	order        *int
	seq          int
}

func (f *fakeSuspensionClearer) ClearForVehicle(_ context.Context, vehicleID string) error {
	f.called = true
	f.gotVehicleID = vehicleID
	if f.order != nil {
		*f.order++
		f.seq = *f.order
	}
	return f.err
}

func ownedReconnectRow() VehicleSnapshotRow {
	return VehicleSnapshotRow{ID: reconnectVehicleID, UserID: reconnectUserID, VIN: reconnectVIN}
}

func newReconnectHandler(
	reader VehicleSnapshotReader,
	resolver teslaTokenResolver,
	pusher FleetConfigPusher,
	clearer SuspensionClearer,
) *VehicleReconnectHandler {
	return NewVehicleReconnectHandler(
		&stubTokenValidator{userID: reconnectUserID},
		reader,
		resolver,
		pusher,
		clearer,
		discardLogger(),
	)
}

func doReconnect(t *testing.T, h *VehicleReconnectHandler, method string, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/api/tesla/vehicles/{vehicleId}/reconnect", h)
	req := httptest.NewRequestWithContext(context.Background(), method,
		"/api/tesla/vehicles/"+reconnectVehicleID+"/reconnect", nil)
	if withAuth {
		req.Header.Set("Authorization", "Bearer jwt")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestReconnect_RecreatesTheConfigAndClearsTheEpisode is the happy path, and the
// ORDER assertion is the load-bearing part: clearing before the push would leave
// a catalog saying "connected" for a car with no config, an owner with no notice,
// and nothing left to press.
func TestReconnect_RecreatesTheConfigAndClearsTheEpisode(t *testing.T) {
	order := 0
	pusher := &fakeConfigPusher{order: &order}
	clearer := &fakeSuspensionClearer{order: &order}
	h := newReconnectHandler(
		&stubVehicleSnapshotReader{row: ownedReconnectRow()},
		&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok_owner"}},
		pusher, clearer,
	)

	rec := doReconnect(t, h, http.MethodPost, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !pusher.called || pusher.gotVIN != reconnectVIN {
		t.Errorf("push = (called %v, vin %q), want the owned VIN", pusher.called, pusher.gotVIN)
	}
	if pusher.gotToken != "tok_owner" {
		t.Errorf("token presented to Tesla = %q, want the owner's resolved token", pusher.gotToken)
	}
	if !clearer.called || clearer.gotVehicleID != reconnectVehicleID {
		t.Errorf("clear = (called %v, vehicle %q)", clearer.called, clearer.gotVehicleID)
	}
	if pusher.seq >= clearer.seq {
		t.Errorf("clear ran at %d and push at %d — the config must be back BEFORE the stamp is dropped, "+
			"or the catalog reads connected for a car with no config", clearer.seq, pusher.seq)
	}

	var body struct {
		VehicleID            string  `json:"vehicleId"`
		TelemetrySuspendedAt *string `json:"telemetrySuspendedAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.VehicleID != reconnectVehicleID {
		t.Errorf("vehicleId = %q, want %q", body.VehicleID, reconnectVehicleID)
	}
	if body.TelemetrySuspendedAt != nil {
		t.Errorf("telemetrySuspendedAt = %v, want an explicit null so the client can drop the notice "+
			"through the same code path the catalog uses", *body.TelemetrySuspendedAt)
	}
}

// TestReconnect_IsIdempotent: a car that was never suspended answers the same
// 200. An owner who taps twice must not be told their car is in a state it is not.
func TestReconnect_IsIdempotent(t *testing.T) {
	pusher := &fakeConfigPusher{}
	clearer := &fakeSuspensionClearer{}
	h := newReconnectHandler(
		&stubVehicleSnapshotReader{row: ownedReconnectRow()},
		&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
		pusher, clearer,
	)

	for i := range 2 {
		if rec := doReconnect(t, h, http.MethodPost, true); rec.Code != http.StatusOK {
			t.Fatalf("pass %d status = %d, want 200", i, rec.Code)
		}
	}
}

func TestReconnect_Refusals(t *testing.T) {
	tests := []struct {
		name       string
		reader     VehicleSnapshotReader
		resolver   teslaTokenResolver
		pusher     FleetConfigPusher
		clearer    SuspensionClearer
		method     string
		auth       bool
		wantStatus int
		wantPushed bool
		reason     string
	}{
		{
			name:       "a non-POST method",
			reader:     &stubVehicleSnapshotReader{row: ownedReconnectRow()},
			resolver:   &fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
			method:     http.MethodGet,
			auth:       true,
			wantStatus: http.StatusMethodNotAllowed,
			reason:     "there is nothing to GET here",
		},
		{
			name:       "no bearer",
			reader:     &stubVehicleSnapshotReader{row: ownedReconnectRow()},
			resolver:   &fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
			method:     http.MethodPost,
			auth:       false,
			wantStatus: http.StatusUnauthorized,
			reason:     "unauthenticated callers never reach a lookup",
		},
		{
			name:       "an unknown vehicle is a 404, never an existence oracle",
			reader:     &stubVehicleSnapshotReader{err: sdk.ErrNotFound},
			resolver:   &fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
			method:     http.MethodPost,
			auth:       true,
			wantStatus: http.StatusNotFound,
			reason:     "indistinguishable from ownership-filtered, matching §7.12/§7.14/§7.16/§7.18",
		},
		{
			name: "a non-owner — including a rides-tier viewer — is refused",
			reader: &stubVehicleSnapshotReader{row: VehicleSnapshotRow{
				ID: reconnectVehicleID, UserID: "somebody_else", VIN: reconnectVIN,
			}},
			resolver:   &fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
			method:     http.MethodPost,
			auth:       true,
			wantStatus: http.StatusForbidden,
			reason: "the suspension followed the OWNER's inactivity and the bill is theirs — " +
				"a rider restarting it would be spending somebody else's money",
		},
		{
			name: "a vehicle with no VIN never streamed",
			reader: &stubVehicleSnapshotReader{row: VehicleSnapshotRow{
				ID: reconnectVehicleID, UserID: reconnectUserID, VIN: "",
			}},
			resolver:   &fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
			method:     http.MethodPost,
			auth:       true,
			wantStatus: http.StatusConflict,
			reason:     "nothing failed; the request simply does not apply to this car",
		},
		{
			name:       "an unlinked Tesla account is a 401, not a 502",
			reader:     &stubVehicleSnapshotReader{row: ownedReconnectRow()},
			resolver:   &fakeTokenResolver{err: sdk.ErrNotFound},
			method:     http.MethodPost,
			auth:       true,
			wantStatus: http.StatusUnauthorized,
			reason:     "the honest next step is re-linking, not retrying this call",
		},
		{
			name:       "an unwired fleet path answers 502",
			reader:     &stubVehicleSnapshotReader{row: ownedReconnectRow()},
			resolver:   &fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
			pusher:     nil,
			method:     http.MethodPost,
			auth:       true,
			wantStatus: http.StatusBadGateway,
			reason:     "there is no way to install a config; the route stays mounted so the answer is honest",
		},
		{
			name:     "Tesla SKIPPING the car is a 409, not a retryable 502",
			reader:   &stubVehicleSnapshotReader{row: ownedReconnectRow()},
			resolver: &fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
			pusher: &fakeConfigPusher{err: &SkippedVehicleError{
				RedactedVIN: "***0042", Reason: "missing_key",
			}},
			method:     http.MethodPost,
			auth:       true,
			wantStatus: http.StatusConflict,
			wantPushed: true,
			reason:     "the owner has to re-pair the virtual key; tapping again cannot fix it",
		},
		{
			name:       "a Tesla failure is a 502",
			reader:     &stubVehicleSnapshotReader{row: ownedReconnectRow()},
			resolver:   &fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
			pusher:     &fakeConfigPusher{err: &FleetAPIError{StatusCode: 503, Body: "upstream"}},
			method:     http.MethodPost,
			auth:       true,
			wantStatus: http.StatusBadGateway,
			wantPushed: true,
			reason:     "the same 502 the §7.10 fleet-config push and §7.23 complete-setup already use",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pusher := tc.pusher
			var recorded *fakeConfigPusher
			if pusher == nil && tc.wantStatus != http.StatusBadGateway {
				recorded = &fakeConfigPusher{}
				pusher = recorded
			} else if fp, ok := pusher.(*fakeConfigPusher); ok {
				recorded = fp
			}
			clearer := &fakeSuspensionClearer{}

			h := newReconnectHandler(tc.reader, tc.resolver, pusher, clearer)
			rec := doReconnect(t, h, tc.method, tc.auth)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (%s); body %s",
					rec.Code, tc.wantStatus, tc.reason, rec.Body.String())
			}
			if recorded != nil && recorded.called != tc.wantPushed {
				t.Errorf("pushed = %v, want %v — a refusal must not reach Tesla",
					recorded.called, tc.wantPushed)
			}
			if clearer.called {
				t.Error("a refused reconnect cleared the suspension — the owner would lose the " +
					"disconnect notice while the car stayed silent")
			}
		})
	}
}

// TestReconnect_ClearFailureIsA500 — the config IS back, so the retry is
// idempotent and cheap; the honest answer is still an error.
func TestReconnect_ClearFailureIsA500(t *testing.T) {
	pusher := &fakeConfigPusher{}
	clearer := &fakeSuspensionClearer{err: errors.New("boom")}
	h := newReconnectHandler(
		&stubVehicleSnapshotReader{row: ownedReconnectRow()},
		&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
		pusher, clearer,
	)

	rec := doReconnect(t, h, http.MethodPost, true)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !pusher.called {
		t.Error("the push should still have run — the clear is the step that failed")
	}
}
