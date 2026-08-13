package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Integration tests for the MYR-355 account-deletion writer. Every one runs
// against real Postgres, because what is under test is a set of conditional
// statements, a transaction boundary and a cross-table identity probe — none
// of which a mock exercises.

const (
	delUserApple  = "cdel000000000000000000apl" // Apple-native: no "User" row
	delUserLegacy = "cdel000000000000000000leg" // legacy web: has a "User" row
	delUserOwner  = "cdel000000000000000000own"
	delUserOther  = "cdel00000000000000000othr"
)

func newAccountDeleter(t *testing.T) *store.AccountDeleter {
	t.Helper()
	return store.NewAccountDeleter(testPool, testLogger())
}

// soloScope is the un-converged deletion scope: the caller stands for itself.
// It is what ResolveDeletionScope returns for every account that never went
// through an identity convergence, which is the overwhelming majority — the
// converged shape gets its own tests in account_deletion_resurrection_test.go.
//
// The parameter is what documents which id each call site builds its scope from,
// so it stays even though every shape in this file reuses one fixture id.
//
//nolint:unparam // see above — the parameter is documentation, not dead weight.
func soloScope(userID string) store.DeletionScope {
	return store.DeletionScope{CallerID: userID, CanonicalID: userID, IDs: []string{userID}}
}

// setupAccountDeletion installs a clean slate for one test.
//
// The AuditLog table is created by audit_repo_test.go's `ensureAuditSchema`
// rather than by a fixture of our own — deliberately. That constant carries the
// append-only BEFORE UPDATE/DELETE triggers CG-DL-2 depends on, and a second,
// trigger-less `CREATE TABLE IF NOT EXISTS` here would win the race (this file
// sorts first) and silently disarm the guard for the whole package. Rows are
// cleared with the same TRUNCATE those tests use, because a plain DELETE is
// exactly what the trigger refuses.
func setupAccountDeletion(t *testing.T) {
	t.Helper()
	if !dockerAvailable {
		t.Skip("Docker not available -- skipping account-deletion integration test")
	}
	mustApplyGoMigrations(t)
	ensureAuditSchema(t)
	cleanAuditLog(t, testPool)
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM go_vehicle_shares`,
		`DELETE FROM go_push_devices`,
		`DELETE FROM go_refresh_tokens`,
		`DELETE FROM go_identity_apple`,
		`DELETE FROM go_users`,
		`DELETE FROM go_ride_requests`,
	} {
		if _, err := testPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("clean (%s): %v", stmt, err)
		}
	}
	cleanTables(t, testPool)
	if _, err := testPool.Exec(ctx, `DELETE FROM "User"`); err != nil {
		t.Fatalf("clean User: %v", err)
	}
}

// seedShareGrant inserts one go_vehicle_shares row in the given status.
// `status` is deliberately a parameter even though every caller currently
// passes "accepted": the column drives the access predicate every reader of
// go_vehicle_shares joins on, so a seed helper that could only produce accepted
// grants would quietly stop tests from covering the revoked and pending arms.
// Keeping it costs one argument at four call sites and makes the grant's
// lifecycle state explicit at each of them.
//
//nolint:unparam // status is intentionally variable; see above.
func seedShareGrant(t *testing.T, id, vehicleID, ownerID, acceptedBy, status string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO go_vehicle_shares
			(id, vehicle_id, owner_user_id, label, permission, code, status,
			 expires_at, accepted_at, accepted_by_user_id)
		VALUES ($1, $2, $3, 'Guest', 'rides', $4, $5, NOW() + INTERVAL '7 days', NOW(), $6)`,
		id, vehicleID, ownerID, "C"+id[len(id)-5:], status, acceptedBy); err != nil {
		t.Fatalf("seed share grant %s: %v", id, err)
	}
}

// seedPushDevice inserts one go_push_devices registration.
func seedPushDevice(t *testing.T, id, userID, deviceToken string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO go_push_devices (id, user_id, device_token) VALUES ($1, $2, $3)`,
		id, userID, deviceToken); err != nil {
		t.Fatalf("seed push device %s: %v", id, err)
	}
}

// seedRefreshToken inserts one live go_refresh_tokens row.
func seedRefreshToken(t *testing.T, hash, userID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO go_refresh_tokens (token_hash, family_id, user_id, expires_at)
		VALUES ($1, $2, $3, NOW() + INTERVAL '90 days')`,
		hash, "fam-"+hash, userID); err != nil {
		t.Fatalf("seed refresh token %s: %v", hash, err)
	}
}

func countQuery(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// --- identity transaction --------------------------------------------------

// TestAccountDeleter_DeleteIdentity_DualSource is the dual-source identity
// case stated plainly: an Apple-native account has NO Prisma "User" row and a
// legacy web account has no go_users row. Both must delete cleanly, and the
// audit row must record which one it was.
func TestAccountDeleter_DeleteIdentity_DualSource(t *testing.T) {
	tests := []struct {
		name          string
		seed          func(t *testing.T, userID string)
		wantHadPrisma bool
	}{
		{
			name: "apple-native user (go_users + apple binding, no User row)",
			seed: func(t *testing.T, userID string) {
				seedGoUser(t, userID, strPtr("Priya Patel"), strPtr("priya@icloud.com"))
				seedAppleIdentity(t, "sub-"+userID, userID, strPtr("Priya Patel"), strPtr("priya@icloud.com"))
			},
			wantHadPrisma: false,
		},
		{
			name: "legacy web user (User row + apple binding, no go_users row)",
			seed: func(t *testing.T, userID string) {
				seedUser(t, userID, strPtr("Ada Lovelace"), strPtr("ada@example.com"))
				seedAppleIdentity(t, "sub-"+userID, userID, strPtr("Ada Lovelace"), strPtr("ada@example.com"))
			},
			wantHadPrisma: true,
		},
		{
			name: "apple binding only (no user row anywhere)",
			seed: func(t *testing.T, userID string) {
				seedAppleIdentity(t, "sub-"+userID, userID, strPtr("Solo"), nil)
			},
			wantHadPrisma: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupAccountDeletion(t)
			userID := delUserApple
			tc.seed(t, userID)

			res, err := newAccountDeleter(t).DeleteIdentity(context.Background(), soloScope(userID),
				store.AccountDeletionCounts{VehicleCount: 1, RidesCancelled: 2})
			if err != nil {
				t.Fatalf("DeleteIdentity: %v", err)
			}
			if !res.Deleted || res.AlreadyGone {
				t.Fatalf("result = %+v, want Deleted", res)
			}
			if res.HadPrismaUser != tc.wantHadPrisma {
				t.Fatalf("HadPrismaUser = %v, want %v", res.HadPrismaUser, tc.wantHadPrisma)
			}

			if n := countQuery(t, `SELECT count(*) FROM go_identity_apple WHERE user_id = $1`, userID); n != 0 {
				t.Fatalf("apple bindings left = %d", n)
			}
			if n := countQuery(t, `SELECT count(*) FROM go_users WHERE id = $1`, userID); n != 0 {
				t.Fatalf("go_users rows left = %d", n)
			}
			if n := countQuery(t, `SELECT count(*) FROM "User" WHERE "id" = $1`, userID); n != 0 {
				t.Fatalf("User rows left = %d", n)
			}
		})
	}
}

// The audit row is written BEFORE the delete, in the same transaction
// (CG-DL-3), carries targetType='user' / initiator='user', and its metadata is
// P0 counts ONLY — never a name, an email or a coordinate (CG-DL-5).
func TestAccountDeleter_DeleteIdentity_WritesTheAuditRow(t *testing.T) {
	setupAccountDeletion(t)
	seedGoUser(t, delUserApple, strPtr("Priya Patel"), strPtr("priya@icloud.com"))

	counts := store.AccountDeletionCounts{
		VehicleCount: 2, DriveCount: 9, RidesCancelled: 1,
		SharesRevoked: 3, ShareLabelsScrubbed: 3, PushDevicesDeleted: 1,
		SavedPlacesDeleted: 2, RefreshTokensRevoked: 4,
	}
	res, err := newAccountDeleter(t).DeleteIdentity(context.Background(), soloScope(delUserApple), counts)
	if err != nil {
		t.Fatalf("DeleteIdentity: %v", err)
	}

	var action, targetType, targetID, initiator string
	var metadata []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT "action", "targetType", "targetId", "initiator", "metadata"
		 FROM "AuditLog" WHERE "id" = $1`, res.AuditLogID).
		Scan(&action, &targetType, &targetID, &initiator, &metadata); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if action != "account_deleted" || targetType != "user" || initiator != "user" {
		t.Fatalf("audit row = action %q targetType %q initiator %q", action, targetType, initiator)
	}
	if targetID != delUserApple {
		t.Fatalf("targetId = %q, want the caller's own cuid", targetID)
	}

	var got map[string]any
	if err := json.Unmarshal(metadata, &got); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	// Every key must be one of the declared P0 counts/flags. A new key here
	// is a CG-DL-5 decision, not an accident.
	allowed := map[string]bool{
		"vehicleCount": true, "driveCount": true, "ridesCancelled": true,
		"sharesRevoked": true, "pushDevicesDeleted": true,
		// MYR-321. A COUNT of the saved Home/Work rows removed — never the
		// places themselves. The coordinates are P1 and this row is P0-only,
		// so what the audit trail records is that two rows went, never where
		// they pointed.
		"savedPlacesDeleted": true,
		// MYR-447. A COUNT of the share rows whose owner-typed label — the
		// departing person's NAME, written by somebody else — was erased.
		// Never the labels: they are P1 and this row is P0-only.
		"shareLabelsScrubbed":  true,
		"refreshTokensRevoked": true, "hadPrismaUser": true,
		// MYR-540. A COUNT of the departing person's group-ride membership
		// rows removed (step 6b) — never the rides, never the other members.
		// P0-only, per CG-DL-5, exactly like every other count here.
		"rideMembershipsDeleted": true,
	}
	for k, v := range got {
		if !allowed[k] {
			t.Fatalf("audit metadata carries an undeclared key %q = %v", k, v)
		}
	}
	if got["vehicleCount"] != float64(2) || got["sharesRevoked"] != float64(3) ||
		got["savedPlacesDeleted"] != float64(2) {
		t.Fatalf("audit metadata counts = %v", got)
	}

	// The audit row must not have grown a coordinate or a label alongside the
	// count. Asserted on the RAW JSON so a nested object could not hide one.
	for _, leak := range []string{"latitude", "longitude", "lat", "lng", "label", "places\":["} {
		if strings.Contains(string(metadata), leak) {
			t.Errorf("audit metadata leaked %q (CG-DL-5, P0-only): %s", leak, metadata)
		}
	}
}

// A re-run after a completed deletion is a clean no-op that writes NO second
// audit row. This is the idempotency hinge for the whole endpoint: without it,
// the client's retry-on-error path would duplicate the FR-10.2 entry.
func TestAccountDeleter_DeleteIdentity_ReRunWritesNoSecondAuditRow(t *testing.T) {
	setupAccountDeletion(t)
	seedGoUser(t, delUserApple, strPtr("Priya Patel"), nil)
	deleter := newAccountDeleter(t)
	ctx := context.Background()

	if _, err := deleter.DeleteIdentity(ctx, soloScope(delUserApple), store.AccountDeletionCounts{}); err != nil {
		t.Fatalf("first DeleteIdentity: %v", err)
	}
	second, err := deleter.DeleteIdentity(ctx, soloScope(delUserApple), store.AccountDeletionCounts{})
	if err != nil {
		t.Fatalf("second DeleteIdentity: %v", err)
	}
	if !second.AlreadyGone || second.Deleted {
		t.Fatalf("second result = %+v, want AlreadyGone", second)
	}
	if second.AuditLogID != "" {
		t.Fatal("the no-op arm must not write an audit row")
	}
	if n := countQuery(t, `SELECT count(*) FROM "AuditLog" WHERE "userId" = $1`, delUserApple); n != 1 {
		t.Fatalf("audit rows = %d, want exactly 1 across both calls", n)
	}
}

// --- the idempotent SQL steps ----------------------------------------------

// Each step must affect the caller's rows, leave everyone else's alone, and
// report zero on a re-run.
func TestAccountDeleter_StepsAreScopedAndIdempotent(t *testing.T) {
	setupAccountDeletion(t)
	ctx := context.Background()
	deleter := newAccountDeleter(t)

	// Two grants REDEEMED by the deleted user + one redeemed by someone else.
	seedShareGrant(t, "cshare_a", "cveh_x", delUserOwner, delUserApple, "accepted")
	seedShareGrant(t, "cshare_b", "cveh_y", delUserOwner, delUserApple, "accepted")
	seedShareGrant(t, "cshare_c", "cveh_z", delUserOwner, delUserOther, "accepted")
	// Push devices + refresh tokens for both people.
	seedPushDevice(t, "cpush_a", delUserApple, "token-a")
	seedPushDevice(t, "cpush_b", delUserOther, "token-b")
	seedRefreshToken(t, "hash-a", delUserApple)
	seedRefreshToken(t, "hash-b", delUserOther)

	steps := []struct {
		name    string
		run     func() (int, error)
		want    int
		survive func(t *testing.T)
	}{
		{
			name: "revoke shares received",
			run:  func() (int, error) { return deleter.RevokeSharesReceived(ctx, delUserApple) },
			want: 2,
			survive: func(t *testing.T) {
				if n := countQuery(t,
					`SELECT count(*) FROM go_vehicle_shares WHERE accepted_by_user_id = $1 AND status = 'accepted'`,
					delUserOther); n != 1 {
					t.Fatalf("another viewer's grant was revoked (%d left)", n)
				}
				// Revocation is a TOMBSTONE, not a delete: the owner's audit
				// trail of who could see their car outlives the viewer.
				if n := countQuery(t,
					`SELECT count(*) FROM go_vehicle_shares WHERE accepted_by_user_id = $1 AND status = 'revoked'`,
					delUserApple); n != 2 {
					t.Fatalf("revoked tombstones = %d, want 2", n)
				}
			},
		},
		{
			name: "delete push devices",
			run:  func() (int, error) { return deleter.DeletePushDevices(ctx, delUserApple) },
			want: 1,
			survive: func(t *testing.T) {
				if n := countQuery(t, `SELECT count(*) FROM go_push_devices WHERE user_id = $1`, delUserOther); n != 1 {
					t.Fatal("another person's device registration was deleted")
				}
			},
		},
		{
			name: "revoke refresh tokens",
			run:  func() (int, error) { return deleter.RevokeRefreshTokens(ctx, delUserApple) },
			want: 1,
			survive: func(t *testing.T) {
				if n := countQuery(t,
					`SELECT count(*) FROM go_refresh_tokens WHERE user_id = $1 AND revoked = FALSE`,
					delUserOther); n != 1 {
					t.Fatal("another person's refresh token was revoked")
				}
				var reason string
				if err := testPool.QueryRow(ctx,
					`SELECT reason FROM go_refresh_tokens WHERE token_hash = 'hash-a'`).Scan(&reason); err != nil {
					t.Fatalf("read reason: %v", err)
				}
				if reason != "account_deleted" {
					t.Fatalf("revocation reason = %q, want account_deleted", reason)
				}
			},
		},
	}

	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			n, err := tc.run()
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if n != tc.want {
				t.Fatalf("%s affected %d rows, want %d", tc.name, n, tc.want)
			}
			tc.survive(t)

			// Idempotent: the re-run the endpoint depends on affects nothing.
			again, err := tc.run()
			if err != nil {
				t.Fatalf("%s re-run: %v", tc.name, err)
			}
			if again != 0 {
				t.Fatalf("%s re-run affected %d rows, want 0", tc.name, again)
			}
		})
	}
}

// --- retention -------------------------------------------------------------

// TestAccountDeletion_RideHistorySurvivesAsFormerRider is THE retention test.
// A completed ride the deleted user took in someone else's car is the OWNER's
// record and must survive the deletion whole — and its requesterName must go
// from a real first name to OMITTED, which is the signal the client renders as
// "Former rider". No column was added: this is the MYR-264 requester_exists
// probe doing exactly what it was built to do, pinned so a future refactor
// cannot quietly turn a deleted rider back into the "Rider" literal.
func TestAccountDeletion_RideHistorySurvivesAsFormerRider(t *testing.T) {
	setupAccountDeletion(t)
	ctx := context.Background()
	repo, _ := setupRideRequestRepo(t)

	// The rider exists on BOTH identity sources, the way a real Apple-native
	// account that also matched a legacy email would.
	seedGoUser(t, delUserApple, strPtr("Priya Patel"), strPtr("priya@icloud.com"))
	seedAppleIdentity(t, "sub-"+delUserApple, delUserApple, strPtr("Priya Patel"), strPtr("priya@icloud.com"))

	rec := scheduledRideRequest()
	rec.RiderID = delUserApple
	rec.OwnerID = delUserOwner
	created, err := repo.Create(ctx, rec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	completed, err := repo.UpdateStatusFrom(ctx, created.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusCompleted)
	if err != nil {
		t.Fatalf("complete the ride: %v", err)
	}
	if completed.RequesterName != "Priya" {
		t.Fatalf("before deletion RequesterName = %q, want %q", completed.RequesterName, "Priya")
	}

	if _, err := newAccountDeleter(t).DeleteIdentity(ctx, soloScope(delUserApple), store.AccountDeletionCounts{}); err != nil {
		t.Fatalf("DeleteIdentity: %v", err)
	}

	// The ROW is still there — it is the owner's history of their own car.
	after, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("the completed ride must survive the rider's deletion: %v", err)
	}
	if after.Status != store.RideRequestStatusCompleted {
		t.Fatalf("status = %q, want completed", after.Status)
	}
	if after.RiderID != delUserApple {
		t.Fatalf("rider_id = %q — the linkage is left in place, only the identity is gone", after.RiderID)
	}
	// …and the NAME is gone, omitted rather than degraded to the "Rider"
	// literal, because no identity row exists in ANY of the three sources.
	if after.RequesterName != "" {
		t.Fatalf("after deletion RequesterName = %q, want \"\" (omitted → the client renders \"Former rider\")",
			after.RequesterName)
	}

	// The owner's own list still shows the ride.
	page, err := repo.ListByOwnerPage(ctx, delUserOwner, nil, store.RideRequestListCursor{}, 50)
	if err != nil {
		t.Fatalf("ListByOwnerPage: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].RequesterName != "" {
		t.Fatalf("owner list = %+v, want one item with an omitted requester name", page.Items)
	}
}

// The open-ride sweep must see instant AND scheduled rides in every
// non-terminal state, and must NOT see terminal ones.
func TestRideRequestRepo_ListOpenByRider(t *testing.T) {
	setupAccountDeletion(t)
	ctx := context.Background()
	repo, _ := setupRideRequestRepo(t)

	openStates := []store.RideRequestStatus{
		store.RideRequestStatusRequested,
		store.RideRequestStatusAccepted,
		store.RideRequestStatusEnroute,
		store.RideRequestStatusArrived,
	}
	terminal := []store.RideRequestStatus{
		store.RideRequestStatusCompleted,
		store.RideRequestStatusCancelled,
		store.RideRequestStatusDeclined,
	}

	wantOpen := map[string]bool{}
	createdOrder := make([]string, 0, len(openStates)+len(terminal))
	for _, status := range append(append([]store.RideRequestStatus{}, openStates...), terminal...) {
		rec := scheduledRideRequest() // scheduled: exempt from the one-active-instant guard
		rec.RiderID = delUserApple
		rec.OwnerID = delUserOwner
		created, err := repo.Create(ctx, rec)
		if err != nil {
			t.Fatalf("Create(%s): %v", status, err)
		}
		if status != store.RideRequestStatusRequested {
			if _, err := repo.UpdateStatus(ctx, created.ID, status); err != nil {
				t.Fatalf("UpdateStatus(%s): %v", status, err)
			}
		}
		createdOrder = append(createdOrder, created.ID)
		for _, s := range openStates {
			if s == status {
				wantOpen[created.ID] = true
			}
		}
	}

	// A ride belonging to somebody else must never be swept.
	other := scheduledRideRequest()
	other.RiderID = delUserOther
	other.OwnerID = delUserOwner
	if _, err := repo.Create(ctx, other); err != nil {
		t.Fatalf("Create for the other rider: %v", err)
	}

	got, err := repo.ListOpenByRider(ctx, delUserApple)
	if err != nil {
		t.Fatalf("ListOpenByRider: %v", err)
	}
	if len(got) != len(wantOpen) {
		t.Fatalf("got %d open rides, want %d", len(got), len(wantOpen))
	}
	// Ordering is oldest-first — the order the owner saw the requests in. The
	// ids were minted in creation order, so the returned sequence must match
	// the order they were created in.
	seen := make([]string, 0, len(got))
	for _, ref := range got {
		if !wantOpen[ref.ID] {
			t.Fatalf("terminal or foreign ride %s (%s) came back", ref.ID, ref.Status)
		}
		seen = append(seen, ref.ID)
	}
	wantSeq := make([]string, 0, len(wantOpen))
	for _, id := range createdOrder {
		if wantOpen[id] {
			wantSeq = append(wantSeq, id)
		}
	}
	if strings.Join(seen, ",") != strings.Join(wantSeq, ",") {
		t.Fatalf("order = %v, want oldest-first %v", seen, wantSeq)
	}
}

// TestAccountDeleter_DeleteIdentity_ConcurrentCallsWriteOneAuditRow is the
// race the client's own retry affordance creates: a user who taps Delete twice,
// or retries while the first request is still in flight, fires two deletions at
// one account. Without the FOR UPDATE probes both transactions read "rows
// exist" and both write an `account_deleted` row — two FR-10.2 entries for one
// account, which is a false audit trail rather than a cosmetic duplicate.
//
// Only real Postgres can show this: the locking is the mechanism.
func TestAccountDeleter_DeleteIdentity_ConcurrentCallsWriteOneAuditRow(t *testing.T) {
	setupAccountDeletion(t)
	seedGoUser(t, delUserApple, strPtr("Priya Patel"), strPtr("priya@icloud.com"))
	seedAppleIdentity(t, "sub-"+delUserApple, delUserApple, strPtr("Priya Patel"), nil)
	seedUser(t, delUserApple, strPtr("Priya Patel"), strPtr("priya@example.com"))

	deleter := newAccountDeleter(t)
	const callers = 4
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		deleted int
		gone    int
	)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together, so the probes genuinely overlap
			res, err := deleter.DeleteIdentity(context.Background(), soloScope(delUserApple), store.AccountDeletionCounts{})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("DeleteIdentity: %v", err)
				return
			}
			switch {
			case res.Deleted:
				deleted++
			case res.AlreadyGone:
				gone++
			}
		}()
	}
	close(start)
	wg.Wait()

	if deleted != 1 || gone != callers-1 {
		t.Fatalf("outcomes = %d deleted / %d already-gone, want exactly 1 / %d", deleted, gone, callers-1)
	}
	if n := countQuery(t, `SELECT count(*) FROM "AuditLog" WHERE "userId" = $1`, delUserApple); n != 1 {
		t.Fatalf("audit rows = %d across %d concurrent deletions, want exactly 1", n, callers)
	}
}
