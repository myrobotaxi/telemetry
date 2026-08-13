package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// --- fakes -----------------------------------------------------------------

type fakeOwnedVehicleLister struct {
	ids   []string
	err   error
	calls int
}

func (f *fakeOwnedVehicleLister) ListOwnedVehicleIDs(_ context.Context, _ string) ([]string, error) {
	f.calls++
	return f.ids, f.err
}

// fakeAccountTeardown records every vehicle handed to it and can fail on a
// nominated id, which is how the mid-failure re-run cases are built.
type fakeAccountTeardown struct {
	removed  map[string]bool // ids already torn down (survives across runs)
	failOn   string
	seen     []string
	order    *[]string
	failures int
}

func newFakeAccountTeardown() *fakeAccountTeardown {
	return &fakeAccountTeardown{removed: map[string]bool{}}
}

func (f *fakeAccountTeardown) RemoveVehicle(_ context.Context, _, vehicleID string) (VehicleTeardownResult, error) {
	f.seen = append(f.seen, vehicleID)
	if f.order != nil {
		*f.order = append(*f.order, "teardown:"+vehicleID)
	}
	if f.failOn == vehicleID {
		f.failures++
		return VehicleTeardownResult{}, errors.New("boom")
	}
	if f.removed[vehicleID] {
		// Idempotent re-run: already gone is a clean no-op success.
		return VehicleTeardownResult{AlreadyGone: true}, nil
	}
	f.removed[vehicleID] = true
	return VehicleTeardownResult{Removed: true, DriveCount: 2}, nil
}

type fakeRideCanceller struct {
	open       []OpenRideRef
	listErr    error
	updateErr  map[string]error
	updated    []string
	order      *[]string
	fromPassed [][]string
}

func (f *fakeRideCanceller) ListOpenRidesByRider(_ context.Context, _ string) ([]OpenRideRef, error) {
	return f.open, f.listErr
}

func (f *fakeRideCanceller) UpdateStatusFrom(_ context.Context, id string, from []string, to string) (RideRequestData, error) {
	f.fromPassed = append(f.fromPassed, from)
	if f.order != nil {
		*f.order = append(*f.order, "cancel:"+id)
	}
	if err, ok := f.updateErr[id]; ok {
		return RideRequestData{}, err
	}
	f.updated = append(f.updated, id)
	// The guarded UPDATE's own RETURNING is what carries the full record —
	// which is exactly why the LIST does not have to.
	return RideRequestData{
		ID:        id,
		RiderID:   deletionUserID,
		OwnerID:   "cowner_1",
		VehicleID: "cveh_1",
		Status:    to,
	}, nil
}

// fakeAccountData is the store seam. Each counter both records the call and
// models idempotency: a second run reports zero rows affected, exactly as the
// real `status <> 'revoked'` / `WHERE user_id` statements do.
type fakeAccountData struct {
	driveCount    int
	driveCountErr error

	sharesRevoked int
	sharesErr     error

	shareLabelsScrubbed int
	shareLabelsErr      error

	devicesDeleted int
	devicesErr     error

	placesDeleted int
	placesErr     error

	tokensRevoked int
	tokensErr     error

	identityGone   bool
	identityErr    error
	identityCalls  int
	identityCounts AccountDeletionCounts
	identityScope  AccountDeletionScope

	// scopeExtraIDs are the abandoned ids an identity convergence left behind
	// (MYR-452). Empty — the ordinary case — resolves the caller to itself.
	scopeExtraIDs []string
	scopeErr      error
	// seenIDs records every id each step was actually invoked with, which is
	// what proves the sequence covered the whole closure rather than just the
	// subject the caller's token happened to carry.
	seenIDs []string

	order *[]string
	calls []string
}

func (f *fakeAccountData) note(step string) {
	f.calls = append(f.calls, step)
	if f.order != nil {
		*f.order = append(*f.order, step)
	}
}

func (f *fakeAccountData) ResolveDeletionScope(_ context.Context, callerID string) (AccountDeletionScope, error) {
	f.note("resolve_identity_scope")
	if f.scopeErr != nil {
		return AccountDeletionScope{}, f.scopeErr
	}
	scope := AccountDeletionScope{CallerID: callerID, CanonicalID: callerID, IDs: []string{callerID}}
	scope.IDs = append(scope.IDs, f.scopeExtraIDs...)
	return scope, nil
}

func (f *fakeAccountData) CountUserDrives(_ context.Context, id string) (int, error) {
	f.note("count_drives")
	f.seenIDs = append(f.seenIDs, id)
	return f.driveCount, f.driveCountErr
}

func (f *fakeAccountData) RevokeSharesReceived(_ context.Context, id string) (int, error) {
	f.note("revoke_shares")
	f.seenIDs = append(f.seenIDs, id)
	if f.sharesErr != nil {
		return 0, f.sharesErr
	}
	n := f.sharesRevoked
	f.sharesRevoked = 0 // idempotent: a re-run matches nothing
	return n, nil
}

func (f *fakeAccountData) ScrubSharesReceivedLabel(_ context.Context, id string) (int, error) {
	f.note("scrub_share_labels")
	f.seenIDs = append(f.seenIDs, id)
	if f.shareLabelsErr != nil {
		return 0, f.shareLabelsErr
	}
	n := f.shareLabelsScrubbed
	f.shareLabelsScrubbed = 0 // idempotent: a re-run matches nothing
	return n, nil
}

func (f *fakeAccountData) DeletePushDevices(_ context.Context, id string) (int, error) {
	f.note("delete_devices")
	f.seenIDs = append(f.seenIDs, id)
	if f.devicesErr != nil {
		return 0, f.devicesErr
	}
	n := f.devicesDeleted
	f.devicesDeleted = 0
	return n, nil
}

func (f *fakeAccountData) DeleteSavedPlaces(_ context.Context, id string) (int, error) {
	f.note("delete_saved_places")
	f.seenIDs = append(f.seenIDs, id)
	if f.placesErr != nil {
		return 0, f.placesErr
	}
	n := f.placesDeleted
	f.placesDeleted = 0 // idempotent: a re-run deletes nothing
	return n, nil
}

func (f *fakeAccountData) RevokeRefreshTokens(_ context.Context, id string) (int, error) {
	f.note("revoke_tokens")
	f.seenIDs = append(f.seenIDs, id)
	if f.tokensErr != nil {
		return 0, f.tokensErr
	}
	n := f.tokensRevoked
	f.tokensRevoked = 0
	return n, nil
}

func (f *fakeAccountData) DeleteIdentity(_ context.Context, scope AccountDeletionScope, counts AccountDeletionCounts) (AccountIdentityOutcome, error) {
	f.note("delete_identity")
	f.identityCalls++
	f.identityCounts = counts
	f.identityScope = scope
	if f.identityErr != nil {
		return AccountIdentityOutcome{}, f.identityErr
	}
	if f.identityGone {
		return AccountIdentityOutcome{AlreadyGone: true}, nil
	}
	f.identityGone = true // second run finds nothing left → no second audit row
	return AccountIdentityOutcome{Deleted: true, AuditLogID: "caudit1"}, nil
}

type fakeSessionInvalidator struct {
	users    []string
	vehicles []string
}

func (f *fakeSessionInvalidator) InvalidateUser(userID string) { f.users = append(f.users, userID) }
func (f *fakeSessionInvalidator) InvalidateVehicles(userID string) {
	f.vehicles = append(f.vehicles, userID)
}

// --- helpers ---------------------------------------------------------------

const deletionUserID = "cuser_355"

func newDeletionHandler(t *testing.T, deps AccountDeletionDeps) *AccountDeletionHandler {
	t.Helper()
	return NewAccountDeletionHandler(&stubTokenValidator{userID: deletionUserID}, deps, discardLogger())
}

func deleteMeRequest() *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/api/users/me", nil)
	r.Header.Set("Authorization", "Bearer token")
	return r
}

// call runs one DELETE and returns the recorder.
func callDelete(h *AccountDeletionHandler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, deleteMeRequest())
	return w
}

func openRide(id, status string) OpenRideRef {
	return OpenRideRef{ID: id, Status: status}
}

// --- tests -----------------------------------------------------------------

func TestAccountDeletion_MethodAndAuth(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		authHeader string
		validator  *stubTokenValidator
		wantStatus int
	}{
		{
			name:       "GET is rejected",
			method:     http.MethodGet,
			authHeader: "Bearer token",
			validator:  &stubTokenValidator{userID: deletionUserID},
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "missing Authorization is 401",
			method:     http.MethodDelete,
			validator:  &stubTokenValidator{userID: deletionUserID},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "invalid token is 401",
			method:     http.MethodDelete,
			authHeader: "Bearer nope",
			validator:  &stubTokenValidator{err: errors.New("expired")},
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := &fakeAccountData{}
			h := NewAccountDeletionHandler(tc.validator, AccountDeletionDeps{
				Vehicles: &fakeOwnedVehicleLister{},
				Teardown: newFakeAccountTeardown(),
				Rides:    &fakeRideCanceller{},
				Data:     data,
			}, discardLogger())

			r := httptest.NewRequestWithContext(context.Background(), tc.method, "/api/users/me", nil)
			if tc.authHeader != "" {
				r.Header.Set("Authorization", tc.authHeader)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if len(data.calls) != 0 {
				t.Fatalf("deletion ran on a rejected request: %v", data.calls)
			}
		})
	}
}

// TestAccountDeletion_OwnerWithSharesRunsEveryStepInOrder is the owner case:
// two cars, grants received, devices and tokens — and the ORDER matters,
// because the identity rows the caller authenticates with must go last.
func TestAccountDeletion_OwnerWithSharesRunsEveryStepInOrder(t *testing.T) {
	var order []string
	teardown := newFakeAccountTeardown()
	teardown.order = &order
	data := &fakeAccountData{
		driveCount:          7,
		sharesRevoked:       2,
		shareLabelsScrubbed: 2,
		devicesDeleted:      1,
		placesDeleted:       2,
		tokensRevoked:       3,
		order:               &order,
	}
	sessions := &fakeSessionInvalidator{}

	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles: &fakeOwnedVehicleLister{ids: []string{"cveh_a", "cveh_b"}},
		Teardown: teardown,
		Rides:    &fakeRideCanceller{},
		Data:     data,
		Sessions: sessions,
	})

	if got := callDelete(h).Code; got != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}

	want := []string{
		// MYR-452. FIRST, and load-bearing: the caller's JWT subject is not
		// reliably the id their account is filed under, so every step below
		// keys off the resolved closure rather than the raw subject.
		"resolve_identity_scope",
		"count_drives",
		"teardown:cveh_a",
		"teardown:cveh_b",
		"revoke_shares",
		// MYR-447. Immediately after the revocation it completes: step 4
		// tombstones the ACCESS, this erases the NAME the owner typed for a
		// person who has just deleted their account.
		"scrub_share_labels",
		"delete_devices",
		// MYR-321. Position is load-bearing in one direction only: it MUST
		// precede delete_identity, because a saved place that outlived its
		// owner would be encrypted GPS of where a deleted person lives, keyed
		// by a cuid nothing can resolve.
		"delete_saved_places",
		"revoke_tokens",
		"delete_identity",
	}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("sequence = %v, want %v", order, want)
	}

	// The audit metadata carries the P0 tally accumulated along the way.
	got := data.identityCounts
	if got.VehicleCount != 2 || got.DriveCount != 7 || got.SharesRevoked != 2 ||
		got.ShareLabelsScrubbed != 2 ||
		got.PushDevicesDeleted != 1 || got.SavedPlacesDeleted != 2 ||
		got.RefreshTokensRevoked != 3 {
		t.Fatalf("audit counts = %+v", got)
	}

	if len(sessions.users) != 1 || len(sessions.vehicles) != 1 {
		t.Fatalf("both auth caches must be invalidated, got users=%v vehicles=%v",
			sessions.users, sessions.vehicles)
	}
}

// TestAccountDeletion_AppleNativeOnlyUserDeletesCleanly covers the dual-source
// identity case: an Apple-native rider owns no vehicle and has no Prisma
// "User" row. Nothing about the sequence may depend on either existing.
func TestAccountDeletion_AppleNativeOnlyUserDeletesCleanly(t *testing.T) {
	teardown := newFakeAccountTeardown()
	data := &fakeAccountData{devicesDeleted: 1, tokensRevoked: 1}

	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles: &fakeOwnedVehicleLister{ids: nil},
		Teardown: teardown,
		Rides:    &fakeRideCanceller{},
		Data:     data,
	})

	if got := callDelete(h).Code; got != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}
	if len(teardown.seen) != 0 {
		t.Fatalf("no vehicles owned, but teardown ran for %v", teardown.seen)
	}
	if data.identityCalls != 1 {
		t.Fatalf("identity delete calls = %d, want 1", data.identityCalls)
	}
	if data.identityCounts.VehicleCount != 0 {
		t.Fatalf("vehicle count = %d, want 0", data.identityCounts.VehicleCount)
	}
}

// TestAccountDeletion_RiderWithOpenRideCancelsThroughTheGuardAndNotifies is
// the rider case. Cancellation must go through the SAME guarded from-set the
// rider-facing cancel endpoint uses, and must publish the standard lifecycle
// event so the owner is told.
func TestAccountDeletion_RiderWithOpenRideCancelsThroughTheGuardAndNotifies(t *testing.T) {
	rides := &fakeRideCanceller{
		open: []OpenRideRef{
			openRide("cride_1", rideStatusRequested),
			openRide("cride_2", rideStatusAccepted),
		},
	}
	publisher := &fakeRidePublisher{}
	data := &fakeAccountData{}

	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles: &fakeOwnedVehicleLister{},
		Teardown: newFakeAccountTeardown(),
		Rides:    rides,
		Events:   publisher,
		Data:     data,
	})

	if got := callDelete(h).Code; got != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}
	if len(rides.updated) != 2 {
		t.Fatalf("cancelled %v, want both rides", rides.updated)
	}
	if data.identityCounts.RidesCancelled != 2 {
		t.Fatalf("audit RidesCancelled = %d, want 2", data.identityCounts.RidesCancelled)
	}
	for _, from := range rides.fromPassed {
		if fmt.Sprint(from) != fmt.Sprint(accountDeletionCancellableFrom) {
			t.Fatalf("guarded from-set = %v, want the deletion teardown's own set %v", from, accountDeletionCancellableFrom)
		}
	}
	if len(publisher.events) != 2 {
		t.Fatalf("published %d lifecycle events, want 2", len(publisher.events))
	}
	for _, ev := range publisher.events {
		payload, ok := ev.Payload.(events.RideStatusChangedEvent)
		if !ok {
			t.Fatalf("payload type = %T, want RideStatusChangedEvent", ev.Payload)
		}
		if payload.Status != rideStatusCancelled {
			t.Fatalf("published status = %q, want cancelled", payload.Status)
		}
		if payload.OwnerID == "" {
			t.Fatal("owner id missing — the owner is who this event has to reach")
		}
	}
}

// A ride physically in progress is left alone: cancelling a car mid-trip from
// under its owner is worse than letting it terminate on its own.
func TestAccountDeletion_RideInProgressIsLeftToFinish(t *testing.T) {
	rides := &fakeRideCanceller{
		open: []OpenRideRef{
			openRide("cride_enroute", rideStatusEnroute),
			openRide("cride_arrived", rideStatusArrived),
			openRide("cride_open", rideStatusRequested),
		},
	}
	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles: &fakeOwnedVehicleLister{},
		Teardown: newFakeAccountTeardown(),
		Rides:    rides,
		Data:     &fakeAccountData{},
	})

	if got := callDelete(h).Code; got != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}
	if fmt.Sprint(rides.updated) != fmt.Sprint([]string{"cride_open"}) {
		t.Fatalf("cancelled %v, want only the requested ride", rides.updated)
	}
}

// Losing the transition race is not a failure: the owner closed the ride
// first, which is the outcome this step wanted anyway.
func TestAccountDeletion_RideClosedByTheOwnerFirstIsNotAnError(t *testing.T) {
	rides := &fakeRideCanceller{
		open: []OpenRideRef{
			openRide("cride_raced", rideStatusRequested),
			openRide("cride_gone", rideStatusRequested),
			openRide("cride_ok", rideStatusRequested),
		},
		updateErr: map[string]error{
			"cride_raced": ErrRideStatusConflict,
			"cride_gone":  fmt.Errorf("wrapped: %w", sdk.ErrNotFound),
		},
	}
	data := &fakeAccountData{}
	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles: &fakeOwnedVehicleLister{},
		Teardown: newFakeAccountTeardown(),
		Rides:    rides,
		Data:     data,
	})

	if got := callDelete(h).Code; got != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}
	if data.identityCalls != 1 {
		t.Fatal("the deletion must complete despite the lost races")
	}
	if data.identityCounts.RidesCancelled != 1 {
		t.Fatalf("RidesCancelled = %d, want 1 (only the uncontested ride)", data.identityCounts.RidesCancelled)
	}
}

// TestAccountDeletion_MidFailureIsReRunnable is the load-bearing property:
// every step is idempotent and the identity rows go LAST, so a failure leaves
// an account that can still authenticate and finish its own deletion.
func TestAccountDeletion_MidFailureIsReRunnable(t *testing.T) {
	tests := []struct {
		name       string
		breakIt    func(*fakeAccountTeardown, *fakeAccountData)
		healIt     func(*fakeAccountTeardown, *fakeAccountData)
		wantAudits int
	}{
		{
			name: "the second vehicle teardown fails",
			breakIt: func(td *fakeAccountTeardown, _ *fakeAccountData) {
				td.failOn = "cveh_b"
			},
			healIt: func(td *fakeAccountTeardown, _ *fakeAccountData) {
				td.failOn = ""
			},
			wantAudits: 1,
		},
		{
			name: "the share revocation fails",
			breakIt: func(_ *fakeAccountTeardown, d *fakeAccountData) {
				d.sharesErr = errors.New("db down")
			},
			healIt: func(_ *fakeAccountTeardown, d *fakeAccountData) {
				d.sharesErr = nil
			},
			wantAudits: 1,
		},
		{
			// MYR-321. A saved-place failure must abort like any other step
			// rather than being swallowed: leaving the rows behind would leave
			// encrypted GPS of a deleted person's home in the table.
			name: "the saved-place deletion fails",
			breakIt: func(_ *fakeAccountTeardown, d *fakeAccountData) {
				d.placesErr = errors.New("db down")
			},
			healIt: func(_ *fakeAccountTeardown, d *fakeAccountData) {
				d.placesErr = nil
			},
			wantAudits: 1,
		},
		{
			name: "the identity transaction itself fails",
			breakIt: func(_ *fakeAccountTeardown, d *fakeAccountData) {
				d.identityErr = errors.New("rollback")
			},
			healIt: func(_ *fakeAccountTeardown, d *fakeAccountData) {
				d.identityErr = nil
			},
			wantAudits: 2, // one failed attempt + one that commits
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			teardown := newFakeAccountTeardown()
			data := &fakeAccountData{sharesRevoked: 1, devicesDeleted: 1, placesDeleted: 2, tokensRevoked: 1}
			h := newDeletionHandler(t, AccountDeletionDeps{
				Vehicles: &fakeOwnedVehicleLister{ids: []string{"cveh_a", "cveh_b"}},
				Teardown: teardown,
				Rides:    &fakeRideCanceller{},
				Data:     data,
			})

			tc.breakIt(teardown, data)
			if got := callDelete(h).Code; got != http.StatusInternalServerError {
				t.Fatalf("first attempt status = %d, want 500", got)
			}

			tc.healIt(teardown, data)
			if got := callDelete(h).Code; got != http.StatusNoContent {
				t.Fatalf("retry status = %d, want 204", got)
			}

			if data.identityCalls != tc.wantAudits {
				t.Fatalf("identity transaction attempts = %d, want %d", data.identityCalls, tc.wantAudits)
			}
			// A car torn down on the first attempt is not torn down twice:
			// the second call sees AlreadyGone and counts it as done.
			if teardown.removed["cveh_a"] != true {
				t.Fatal("cveh_a should be gone after the retry")
			}
		})
	}
}

// TestAccountDeletion_SavedPlacesGoBeforeTheIdentity pins the MYR-321 step's
// two obligations: it must run, and it must run BEFORE the identity rows go.
//
// The ordering is not cosmetic. go_saved_places has no foreign key (CG-DL-9),
// so nothing cascades: a row left behind after the identity delete is
// AES-256-GCM ciphertext of where a deleted person LIVES, keyed by a cuid that
// no longer resolves to anybody and reachable by nothing but a table scan. It
// would never be read, never be reported, and never go away.
func TestAccountDeletion_SavedPlacesGoBeforeTheIdentity(t *testing.T) {
	var order []string
	data := &fakeAccountData{placesDeleted: 2, order: &order}
	teardown := newFakeAccountTeardown()
	teardown.order = &order

	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles: &fakeOwnedVehicleLister{},
		Teardown: teardown,
		Rides:    &fakeRideCanceller{},
		Data:     data,
	})

	if got := callDelete(h).Code; got != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}

	placesAt := slices.Index(order, "delete_saved_places")
	identityAt := slices.Index(order, "delete_identity")
	if placesAt < 0 {
		t.Fatalf("delete_saved_places never ran; sequence = %v", order)
	}
	if placesAt > identityAt {
		t.Fatalf("saved places must be deleted BEFORE the identity rows; sequence = %v", order)
	}
	if data.identityCounts.SavedPlacesDeleted != 2 {
		t.Fatalf("audit tally savedPlacesDeleted = %d, want 2", data.identityCounts.SavedPlacesDeleted)
	}

	// A re-run deletes nothing and is still a 204 — the step is idempotent,
	// which is what makes the whole sequence re-runnable.
	if got := callDelete(h).Code; got != http.StatusNoContent {
		t.Fatalf("re-run status = %d, want 204", got)
	}
	if data.identityCounts.SavedPlacesDeleted != 0 {
		t.Fatalf("re-run tally savedPlacesDeleted = %d, want 0", data.identityCounts.SavedPlacesDeleted)
	}
}

// A SECOND successful call is still a 204 and writes NO second audit row —
// which is what makes the client's retry-on-error path safe to offer.
func TestAccountDeletion_SecondCallAfterSuccessIsStill204AndWritesNoSecondAudit(t *testing.T) {
	data := &fakeAccountData{sharesRevoked: 1}
	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles: &fakeOwnedVehicleLister{ids: []string{"cveh_a"}},
		Teardown: newFakeAccountTeardown(),
		Rides:    &fakeRideCanceller{},
		Data:     data,
	})

	if got := callDelete(h).Code; got != http.StatusNoContent {
		t.Fatalf("first status = %d, want 204", got)
	}
	if got := callDelete(h).Code; got != http.StatusNoContent {
		t.Fatalf("second status = %d, want 204", got)
	}
	// Both calls entered the transaction; only the first found rows to
	// delete, so only the first wrote an audit row (the AlreadyGone arm).
	if data.identityCalls != 2 {
		t.Fatalf("identity calls = %d, want 2", data.identityCalls)
	}
	if !data.identityGone {
		t.Fatal("the second call must observe an already-deleted identity")
	}
}

// A failed drive count must not block the deletion: a missing statistic is
// never a reason to refuse someone erasure of their own account.
func TestAccountDeletion_DriveCountFailureIsNotFatal(t *testing.T) {
	data := &fakeAccountData{driveCount: 9, driveCountErr: errors.New("stat query down")}
	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles: &fakeOwnedVehicleLister{},
		Teardown: newFakeAccountTeardown(),
		Rides:    &fakeRideCanceller{},
		Data:     data,
	})

	if got := callDelete(h).Code; got != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}
	if data.identityCounts.DriveCount != 0 {
		t.Fatalf("DriveCount = %d, want 0 when the count failed", data.identityCounts.DriveCount)
	}
}

// A publish failure must not strand the account either: the ride IS cancelled
// and the owner's next read shows it.
func TestAccountDeletion_PublishFailureDoesNotFailTheDeletion(t *testing.T) {
	data := &fakeAccountData{}
	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles: &fakeOwnedVehicleLister{},
		Teardown: newFakeAccountTeardown(),
		Rides:    &fakeRideCanceller{open: []OpenRideRef{openRide("cride_1", rideStatusRequested)}},
		Events:   &fakeRidePublisher{err: errors.New("bus down")},
		Data:     data,
	})

	if got := callDelete(h).Code; got != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}
	if data.identityCounts.RidesCancelled != 1 {
		t.Fatal("the ride was cancelled; only its notification failed")
	}
}

// The 204 carries no body — there is no account left to describe.
func TestAccountDeletion_SuccessResponseHasNoBody(t *testing.T) {
	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles: &fakeOwnedVehicleLister{},
		Teardown: newFakeAccountTeardown(),
		Rides:    &fakeRideCanceller{},
		Data:     &fakeAccountData{},
	})
	w := callDelete(h)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if body := w.Body.String(); body != "" {
		t.Fatalf("body = %q, want empty", body)
	}
}

// --- MYR-452: the sequence must cover the whole identity closure ------------

// TestAccountDeletion_ConvergedScopeRunsEveryStepOverEveryID is the handler-side
// half of MYR-452.
//
// When a Tesla link converges two identities, the caller's token keeps naming
// the pre-convergence id while the account's rows sit under another. Every step
// therefore has to run over the resolved closure, not the raw subject — a step
// that quietly keeps keying off the JWT subject deletes nothing and still
// reports success, which is exactly how a "deleted" account came back.
func TestAccountDeletion_ConvergedScopeRunsEveryStepOverEveryID(t *testing.T) {
	const canonical = "ccanonical_452"

	data := &fakeAccountData{scopeExtraIDs: []string{canonical}}
	sessions := &fakeSessionInvalidator{}
	vehicles := &fakeOwnedVehicleLister{ids: []string{"cveh_a"}}

	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles: vehicles,
		Teardown: newFakeAccountTeardown(),
		Rides:    &fakeRideCanceller{},
		Data:     data,
		Sessions: sessions,
	})

	if got := callDelete(h).Code; got != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}

	// Every SQL step saw both ids. Counting per step rather than checking the
	// set as a whole is what catches one straggler still keyed on the subject.
	const sqlSteps = 6 // drives, shares, labels, devices, places, tokens
	if len(data.seenIDs) != sqlSteps*2 {
		t.Fatalf("steps ran on %d ids, want %d (every step over both)", len(data.seenIDs), sqlSteps*2)
	}
	for _, want := range []string{deletionUserID, canonical} {
		if !slices.Contains(data.seenIDs, want) {
			t.Errorf("no step ran against %q; seen=%v", want, data.seenIDs)
		}
	}

	// The identity transaction receives the whole closure, not one id.
	if !slices.Contains(data.identityScope.IDs, canonical) ||
		!slices.Contains(data.identityScope.IDs, deletionUserID) {
		t.Errorf("DeleteIdentity scope = %v, want both ids", data.identityScope.IDs)
	}

	// The vehicle teardown is per-id too: a converged owner's cars are filed
	// under the canonical id, which the token never names, so listing by the
	// subject alone finds an empty garage.
	if vehicles.calls != 2 {
		t.Errorf("owned-vehicle lister called %d times, want once per id", vehicles.calls)
	}

	// Both auth caches are dropped for BOTH ids — the caller's own subject is
	// the one their still-unexpired access token actually presents.
	if len(sessions.users) != 2 || len(sessions.vehicles) != 2 {
		t.Errorf("auth caches invalidated for users=%v vehicles=%v, want both ids in each",
			sessions.users, sessions.vehicles)
	}
}

// A scope that cannot be resolved must FAIL the deletion. Guessing at the
// closure is how a teardown silently misses its target and reports 204 over a
// live account, so this step is deliberately fatal where the drive count is not.
func TestAccountDeletion_ScopeResolutionFailureIs500AndDeletesNothing(t *testing.T) {
	data := &fakeAccountData{scopeErr: errors.New("convergence graph unreadable")}
	sessions := &fakeSessionInvalidator{}

	h := newDeletionHandler(t, AccountDeletionDeps{
		Vehicles: &fakeOwnedVehicleLister{},
		Teardown: newFakeAccountTeardown(),
		Rides:    &fakeRideCanceller{},
		Data:     data,
		Sessions: sessions,
	})

	if got := callDelete(h).Code; got != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", got)
	}
	if data.identityCalls != 0 {
		t.Error("identity transaction ran on an unresolved scope")
	}
	if len(data.seenIDs) != 0 {
		t.Errorf("destructive steps ran on an unresolved scope: %v", data.seenIDs)
	}
	if len(sessions.users) != 0 {
		t.Error("sessions were invalidated for a deletion that never ran")
	}
	if got := data.calls; len(got) != 1 || got[0] != "resolve_identity_scope" {
		t.Errorf("calls = %v, want the sequence to stop at scope resolution", got)
	}
}
