package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// Integration tests for the MYR-184 sharing repository. Every one of them runs
// against real Postgres, because what is being tested here is not Go logic — it
// is a set of conditional UPDATE predicates and a partial-unique index, none of
// which a mock would exercise.

const (
	shareOwnerA  = "cshown0000000000000000000a"
	shareOwnerB  = "cshown0000000000000000000b"
	shareViewer1 = "cshvw00000000000000000001"
	shareViewer2 = "cshvw00000000000000000002"
)

// newShareRepo builds the repository over the shared test pool.
func newShareRepo(t *testing.T) *store.VehicleShareRepo {
	t.Helper()
	return store.NewVehicleShareRepo(testPool, testLogger())
}

// seedShareFixtures installs a clean slate: two owners, two viewers, and two
// vehicles owned by owner A plus one owned by owner B.
func seedShareFixtures(t *testing.T) (vehA1, vehA2, vehB string) {
	t.Helper()
	mustApplyGoMigrations(t)
	cleanVehicleShares(t)
	cleanTables(t, testPool)
	if _, err := testPool.Exec(context.Background(), `DELETE FROM "User"`); err != nil {
		t.Fatalf("clean User: %v", err)
	}

	// "User"."email" is UNIQUE, so each fixture needs its own address.
	// Every one carries the same two-token display name so the
	// first-name-only assertion has something to trim.
	for _, id := range []string{shareOwnerA, shareOwnerB, shareViewer1, shareViewer2} {
		name := "Alex Owner"
		email := id + "@example.com"
		seedUser(t, id, &name, &email)
	}

	vehA1, vehA2, vehB = "cshveh0000000000000000a1", "cshveh0000000000000000a2", "cshveh00000000000000000b"
	seedVehicleSummaryRow(t, vehA1, shareOwnerA, vinForIndex(701), "Car A1", "Model 3", 2024, "white", store.VehicleStatusParked, 80, 240)
	seedVehicleSummaryRow(t, vehA2, shareOwnerA, vinForIndex(702), "Car A2", "Model Y", 2023, "blue", store.VehicleStatusParked, 60, 200)
	seedVehicleSummaryRow(t, vehB, shareOwnerB, vinForIndex(703), "Car B", "Model S", 2022, "red", store.VehicleStatusParked, 50, 300)
	return vehA1, vehA2, vehB
}

// mustCreateInvite mints an invite and fails the test if it does not.
func mustCreateInvite(t *testing.T, repo *store.VehicleShareRepo, owner, pathVehicle string, ids []string, permission string) store.VehicleShare {
	t.Helper()
	row, err := repo.CreateInvite(context.Background(), store.CreateShareInviteInput{
		OwnerUserID:   owner,
		PathVehicleID: pathVehicle,
		VehicleIDs:    ids,
		Label:         "Mira Chen",
		Permission:    permission,
	})
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	return row
}

func TestVehicleShareRepo_CreateInvite(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, vehB := seedShareFixtures(t)
	repo := newShareRepo(t)

	t.Run("single vehicle mints a pending row with a well-formed code", func(t *testing.T) {
		row := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)

		if row.VehicleID != vehA1 {
			t.Errorf("returned row is for %s, want the path vehicle %s", row.VehicleID, vehA1)
		}
		if row.Status != store.ShareStatusPending {
			t.Errorf("status = %q, want pending", row.Status)
		}
		if !store.ValidShareCodeFormat(row.Code) {
			// The code itself is P1 — report only that it is malformed.
			t.Errorf("minted code does not match ^[A-Z0-9]{6}$ (length %d)", len(row.Code))
		}
		// 7-day TTL, checked loosely so a slow container does not flake.
		ttl := row.ExpiresAt.Sub(row.CreatedAt)
		if ttl < 6*24*time.Hour || ttl > 8*24*time.Hour {
			t.Errorf("expiry TTL = %v, want ~7 days", ttl)
		}
	})

	t.Run("multi vehicle mints N rows sharing ONE code", func(t *testing.T) {
		cleanVehicleShares(t)
		row := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1, vehA2}, store.SharePermissionRides)

		for _, vehicleID := range []string{vehA1, vehA2} {
			rows, err := repo.ListInvitesForVehicle(ctx, vehicleID, shareOwnerA)
			if err != nil {
				t.Fatalf("list %s: %v", vehicleID, err)
			}
			if len(rows) != 1 {
				t.Fatalf("vehicle %s has %d invites, want 1", vehicleID, len(rows))
			}
			if rows[0].Code != row.Code {
				t.Errorf("vehicle %s carries a different code than the returned row — the whole point of a multi-vehicle invite is one code", vehicleID)
			}
			if rows[0].ID == row.ID && vehicleID != vehA1 {
				t.Error("sibling rows must have distinct invite ids")
			}
		}
	})

	t.Run("a set containing an unowned vehicle mints NOTHING", func(t *testing.T) {
		cleanVehicleShares(t)
		_, err := repo.CreateInvite(ctx, store.CreateShareInviteInput{
			OwnerUserID:   shareOwnerA,
			PathVehicleID: vehA1,
			VehicleIDs:    []string{vehA1, vehB},
			Label:         "Mira Chen",
			Permission:    store.SharePermissionLive,
		})
		if !errors.Is(err, store.ErrShareVehicleNotOwned) {
			t.Fatalf("err = %v, want ErrShareVehicleNotOwned", err)
		}
		// The transaction must have rolled back — a partial mint would
		// leave a live code granting only some of what was asked for.
		rows, listErr := repo.ListInvitesForVehicle(ctx, vehA1, shareOwnerA)
		if listErr != nil {
			t.Fatalf("list: %v", listErr)
		}
		if len(rows) != 0 {
			t.Errorf("a rejected create left %d row(s) behind — it must be all-or-nothing", len(rows))
		}
	})

	t.Run("rejects a malformed input before touching the database", func(t *testing.T) {
		cases := map[string]store.CreateShareInviteInput{
			"empty label":       {OwnerUserID: shareOwnerA, PathVehicleID: vehA1, VehicleIDs: []string{vehA1}, Permission: store.SharePermissionLive},
			"bad permission":    {OwnerUserID: shareOwnerA, PathVehicleID: vehA1, VehicleIDs: []string{vehA1}, Label: "L", Permission: "admin"},
			"empty vehicle set": {OwnerUserID: shareOwnerA, PathVehicleID: vehA1, Label: "L", Permission: store.SharePermissionLive},
			"omits path":        {OwnerUserID: shareOwnerA, PathVehicleID: vehA1, VehicleIDs: []string{vehA2}, Label: "L", Permission: store.SharePermissionLive},
		}
		for name, in := range cases {
			t.Run(name, func(t *testing.T) {
				if _, err := repo.CreateInvite(ctx, in); err == nil {
					t.Fatal("expected an error, got nil")
				}
			})
		}
	})
}

func TestVehicleShareRepo_ListRevokeResend(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, _, _ := seedShareFixtures(t)
	repo := newShareRepo(t)

	t.Run("list is scoped to the owner", func(t *testing.T) {
		cleanVehicleShares(t)
		mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)

		mine, err := repo.ListInvitesForVehicle(ctx, vehA1, shareOwnerA)
		if err != nil || len(mine) != 1 {
			t.Fatalf("owner list: rows=%d err=%v", len(mine), err)
		}
		theirs, err := repo.ListInvitesForVehicle(ctx, vehA1, shareOwnerB)
		if err != nil {
			t.Fatalf("other-owner list: %v", err)
		}
		if len(theirs) != 0 {
			t.Errorf("another owner saw %d of this vehicle's invites, want 0", len(theirs))
		}
	})

	t.Run("revoke tombstones, is idempotent, and hides the row", func(t *testing.T) {
		cleanVehicleShares(t)
		row := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)

		viewerID, err := repo.RevokeInvite(ctx, row.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("first revoke: %v", err)
		}
		if viewerID != "" {
			t.Errorf("revoking a PENDING invite reported viewer %q, want empty — nobody held access", viewerID)
		}

		// Idempotent: a retried DELETE must not 404.
		if _, err := repo.RevokeInvite(ctx, row.ID, shareOwnerA); err != nil {
			t.Errorf("second revoke: %v, want nil (idempotent)", err)
		}

		rows, err := repo.ListInvitesForVehicle(ctx, vehA1, shareOwnerA)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("a revoked row is still listed (%d rows) — tombstones are never serialized", len(rows))
		}

		// But the tombstone is still THERE, which is the whole point of not
		// hard-deleting: the audit trail survives.
		var status string
		if err := testPool.QueryRow(ctx,
			`SELECT status FROM go_vehicle_shares WHERE id = $1`, row.ID).Scan(&status); err != nil {
			t.Fatalf("tombstone lookup: %v", err)
		}
		if status != store.ShareStatusRevoked {
			t.Errorf("row status = %q, want revoked — revocation must not delete", status)
		}
	})

	t.Run("revoking another owner's invite is not-found, not forbidden", func(t *testing.T) {
		cleanVehicleShares(t)
		row := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)

		if _, err := repo.RevokeInvite(ctx, row.ID, shareOwnerB); !errors.Is(err, sdk.ErrNotFound) {
			t.Fatalf("err = %v, want a not-found (indistinguishable from a missing row)", err)
		}
	})

	t.Run("resend re-mints the code and pushes the expiry on the SAME row", func(t *testing.T) {
		cleanVehicleShares(t)
		original := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)

		updated, err := repo.ResendInvite(ctx, original.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("resend: %v", err)
		}
		if updated.ID != original.ID {
			t.Errorf("resend changed the invite id (%s → %s) — a client holding the id must keep working", original.ID, updated.ID)
		}
		if updated.Code == original.Code {
			t.Error("resend reused the previous code; the old one must be invalidated")
		}
		if !updated.ExpiresAt.After(original.ExpiresAt) {
			t.Errorf("resend did not push the expiry out (%v → %v)", original.ExpiresAt, updated.ExpiresAt)
		}
		if !updated.CreatedAt.Equal(original.CreatedAt) {
			t.Error("resend moved createdAt; the owner's 'sent {ago}' line refers to the original send")
		}
	})

	t.Run("resending an accepted grant is a conflict", func(t *testing.T) {
		cleanVehicleShares(t)
		row := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)
		if _, err := repo.RedeemCode(ctx, row.Code, shareViewer1); err != nil {
			t.Fatalf("redeem: %v", err)
		}
		_, err := repo.ResendInvite(ctx, row.ID, shareOwnerA)
		if !errors.Is(err, store.ErrShareNotPending) {
			t.Fatalf("err = %v, want ErrShareNotPending", err)
		}
	})

	t.Run("an accepted row never carries its code out of the store", func(t *testing.T) {
		cleanVehicleShares(t)
		row := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)
		if _, err := repo.RedeemCode(ctx, row.Code, shareViewer1); err != nil {
			t.Fatalf("redeem: %v", err)
		}
		rows, err := repo.ListInvitesForVehicle(ctx, vehA1, shareOwnerA)
		if err != nil || len(rows) != 1 {
			t.Fatalf("list: rows=%d err=%v", len(rows), err)
		}
		if rows[0].Status != store.ShareStatusAccepted {
			t.Fatalf("status = %q, want accepted", rows[0].Status)
		}
		if rows[0].Code != "" {
			t.Error("an accepted grant returned a code — it is a live credential and is not re-readable")
		}
		if rows[0].AcceptedAt == nil {
			t.Error("accepted row has no acceptedAt")
		}
	})
}

func TestVehicleShareRepo_RedeemCode(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	repo := newShareRepo(t)

	t.Run("redeems every row of a multi-vehicle code at once", func(t *testing.T) {
		cleanVehicleShares(t)
		row := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1, vehA2}, store.SharePermissionRides)

		grants, err := repo.RedeemCode(ctx, row.Code, shareViewer1)
		if err != nil {
			t.Fatalf("redeem: %v", err)
		}
		if len(grants) != 2 {
			t.Fatalf("granted %d vehicles, want 2 — one code, one atomic redemption", len(grants))
		}
		for _, g := range grants {
			if g.Permission != store.SharePermissionRides {
				t.Errorf("vehicle %s granted %q, want rides", g.VehicleID, g.Permission)
			}
			if g.OwnerUserID != shareOwnerA {
				t.Errorf("vehicle %s reports owner %q", g.VehicleID, g.OwnerUserID)
			}
		}
	})

	t.Run("is idempotent for the same redeemer", func(t *testing.T) {
		cleanVehicleShares(t)
		row := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)

		first, err := repo.RedeemCode(ctx, row.Code, shareViewer1)
		if err != nil {
			t.Fatalf("first redeem: %v", err)
		}
		second, err := repo.RedeemCode(ctx, row.Code, shareViewer1)
		if err != nil {
			t.Fatalf("re-redeem by the same account: %v, want the same grants back", err)
		}
		if len(second) != len(first) {
			t.Errorf("re-redeem returned %d grants, want %d", len(second), len(first))
		}

		var count int
		if err := testPool.QueryRow(ctx,
			`SELECT COUNT(*) FROM go_vehicle_shares WHERE vehicle_id = $1 AND status = 'accepted'`,
			vehA1).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 1 {
			t.Errorf("%d accepted rows after a double redemption, want 1", count)
		}
	})

	t.Run("invalid, expired, and consumed codes are indistinguishable", func(t *testing.T) {
		cleanVehicleShares(t)

		// Consumed by somebody else.
		consumed := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)
		if _, err := repo.RedeemCode(ctx, consumed.Code, shareViewer1); err != nil {
			t.Fatalf("seed consumption: %v", err)
		}

		// Expired: written directly, because the repository will not mint one.
		past := time.Now().Add(-time.Hour)
		if err := insertShare(t, "cshexp0001", vehA2, shareOwnerA, "EXPIRE", "pending", past, nil); err != nil {
			t.Fatalf("seed expired invite: %v", err)
		}

		for name, code := range map[string]string{
			"unknown":  "ZZZZZZ",
			"expired":  "EXPIRE",
			"consumed": consumed.Code,
		} {
			t.Run(name, func(t *testing.T) {
				_, err := repo.RedeemCode(ctx, code, shareViewer2)
				if !errors.Is(err, sdk.ErrNotFound) {
					t.Fatalf("err = %v, want a not-found — all three cases must answer identically", err)
				}
			})
		}
	})

	t.Run("the owner cannot redeem their own code, and nothing is accepted", func(t *testing.T) {
		cleanVehicleShares(t)
		row := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1, vehA2}, store.SharePermissionLive)

		_, err := repo.RedeemCode(ctx, row.Code, shareOwnerA)
		if !errors.Is(err, store.ErrShareSelfRedeem) {
			t.Fatalf("err = %v, want ErrShareSelfRedeem", err)
		}

		var accepted int
		if err := testPool.QueryRow(ctx,
			`SELECT COUNT(*) FROM go_vehicle_shares WHERE status = 'accepted'`).Scan(&accepted); err != nil {
			t.Fatalf("count: %v", err)
		}
		if accepted != 0 {
			t.Errorf("%d rows were accepted despite the refusal — the refusal must roll back the whole redemption", accepted)
		}
	})

	t.Run("a malformed code never reaches the database", func(t *testing.T) {
		for _, bad := range []string{"", "ABC", "abcdef", "ABCDE!", "ABCDEFG"} {
			if _, err := repo.RedeemCode(ctx, bad, shareViewer1); !errors.Is(err, sdk.ErrNotFound) {
				t.Errorf("code of length %d: err = %v, want a not-found", len(bad), err)
			}
		}
	})

	t.Run("a revoked invite stops redeeming", func(t *testing.T) {
		cleanVehicleShares(t)
		row := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)
		if _, err := repo.RevokeInvite(ctx, row.ID, shareOwnerA); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if _, err := repo.RedeemCode(ctx, row.Code, shareViewer1); !errors.Is(err, sdk.ErrNotFound) {
			t.Fatalf("a revoked code still redeemed: err = %v", err)
		}
	})
}

// TestVehicleShareRepo_ConcurrentRedeem is the race the whole redeem design
// exists for: two DIFFERENT accounts submitting one code at the same instant.
// Exactly one must win, and the loser must see a plain not-found — never a
// partial grant, and never a second accepted row.
func TestVehicleShareRepo_ConcurrentRedeem(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	repo := newShareRepo(t)
	cleanVehicleShares(t)

	row := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1, vehA2}, store.SharePermissionLive)

	type result struct {
		grants []store.ShareGrant
		err    error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, redeemer := range []string{shareViewer1, shareViewer2} {
		wg.Add(1)
		go func(idx int, user string) {
			defer wg.Done()
			<-start
			g, err := repo.RedeemCode(ctx, row.Code, user)
			results[idx] = result{grants: g, err: err}
		}(i, redeemer)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, r := range results {
		switch {
		case r.err == nil:
			winners++
			if len(r.grants) != 2 {
				t.Errorf("winner %d got %d grants, want both vehicles — a partial grant is the failure this test exists to catch", i, len(r.grants))
			}
		case errors.Is(r.err, sdk.ErrNotFound):
			// The correct loser outcome.
		default:
			t.Errorf("loser %d got an unexpected error: %v", i, r.err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d redemptions succeeded, want exactly 1", winners)
	}

	var accepted int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM go_vehicle_shares WHERE status = 'accepted'`).Scan(&accepted); err != nil {
		t.Fatalf("count: %v", err)
	}
	if accepted != 2 {
		t.Errorf("%d accepted rows, want 2 (both vehicles, one redeemer)", accepted)
	}
}

func TestVehicleShareRepo_AccessLookups(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	repo := newShareRepo(t)
	cleanVehicleShares(t)

	row := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLiveHistory)
	if _, err := repo.RedeemCode(ctx, row.Code, shareViewer1); err != nil {
		t.Fatalf("redeem: %v", err)
	}

	t.Run("shared vehicle ids", func(t *testing.T) {
		ids, err := repo.SharedVehicleIDs(ctx, shareViewer1)
		if err != nil {
			t.Fatalf("SharedVehicleIDs: %v", err)
		}
		if len(ids) != 1 || ids[0] != vehA1 {
			t.Fatalf("got %v, want [%s]", ids, vehA1)
		}
		// Somebody with no grants sees nothing, not an error.
		none, err := repo.SharedVehicleIDs(ctx, shareViewer2)
		if err != nil {
			t.Fatalf("SharedVehicleIDs(no grants): %v", err)
		}
		if len(none) != 0 {
			t.Errorf("an ungranted user saw %v", none)
		}
	})

	t.Run("permission lookup", func(t *testing.T) {
		tier, err := repo.SharePermissionFor(ctx, shareViewer1, vehA1)
		if err != nil {
			t.Fatalf("SharePermissionFor: %v", err)
		}
		if tier != store.SharePermissionLiveHistory {
			t.Errorf("tier = %q, want live_history", tier)
		}
		// No grant on a different car, and no grant for a different person.
		if _, err := repo.SharePermissionFor(ctx, shareViewer1, vehA2); !errors.Is(err, sdk.ErrNotFound) {
			t.Errorf("ungranted vehicle: err = %v, want not-found", err)
		}
		if _, err := repo.SharePermissionFor(ctx, shareViewer2, vehA1); !errors.Is(err, sdk.ErrNotFound) {
			t.Errorf("ungranted user: err = %v, want not-found", err)
		}
	})

	t.Run("revoking removes the access immediately", func(t *testing.T) {
		viewerID, err := repo.RevokeInvite(ctx, row.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if viewerID != shareViewer1 {
			t.Errorf("revoke reported viewer %q, want %q — the caller needs it to bust that person's cache", viewerID, shareViewer1)
		}
		if _, err := repo.SharePermissionFor(ctx, shareViewer1, vehA1); !errors.Is(err, sdk.ErrNotFound) {
			t.Errorf("a revoked viewer still resolves a tier: err = %v", err)
		}
		ids, err := repo.SharedVehicleIDs(ctx, shareViewer1)
		if err != nil {
			t.Fatalf("SharedVehicleIDs: %v", err)
		}
		if len(ids) != 0 {
			t.Errorf("a revoked viewer still has %v in their access set", ids)
		}
	})
}

func TestVehicleShareRepo_OwnerFirstName(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	seedShareFixtures(t)
	repo := newShareRepo(t)

	t.Run("first token of the display name", func(t *testing.T) {
		got, err := repo.OwnerFirstName(ctx, shareOwnerA)
		if err != nil {
			t.Fatalf("OwnerFirstName: %v", err)
		}
		if got != "Alex" {
			t.Errorf("got %q, want %q — first names only (P1 policy)", got, "Alex")
		}
		if strings.Contains(got, "Owner") {
			t.Error("the surname leaked into the redeemer-facing name")
		}
	})

	t.Run("falls back to the email local-part, then a literal", func(t *testing.T) {
		emailOnly := "cshemail000000000000000001"
		email := "grace.hopper@navy.mil"
		seedUser(t, emailOnly, nil, &email)
		got, err := repo.OwnerFirstName(ctx, emailOnly)
		if err != nil {
			t.Fatalf("OwnerFirstName: %v", err)
		}
		if got != "grace.hopper" {
			t.Errorf("got %q, want the email local-part", got)
		}

		bare := "cshbare0000000000000000001"
		seedUser(t, bare, nil, nil)
		got, err = repo.OwnerFirstName(ctx, bare)
		if err != nil {
			t.Fatalf("OwnerFirstName: %v", err)
		}
		if got == "" {
			t.Error("the fallback returned an empty string; the contract guarantees a non-empty name")
		}
	})
}

// TestNewShareCode_Distribution is a coarse sanity check on the code minter: no
// duplicates in a large batch, every code well-formed, and every alphabet
// symbol reachable. The last one is what catches a rejection-sampling bug that
// silently narrows the space.
func TestNewShareCode_Distribution(t *testing.T) {
	const draws = 4000
	seen := make(map[string]struct{}, draws)
	symbols := make(map[rune]struct{}, 36)

	for i := 0; i < draws; i++ {
		code, err := store.NewShareCodeForTest()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if !store.ValidShareCodeFormat(code) {
			t.Fatalf("draw %d is malformed (length %d)", i, len(code))
		}
		if _, dup := seen[code]; dup {
			t.Fatalf("duplicate code in %d draws — the entropy source is not random", draws)
		}
		seen[code] = struct{}{}
		for _, r := range code {
			symbols[r] = struct{}{}
		}
	}
	if len(symbols) != 36 {
		t.Errorf("only %d of 36 alphabet symbols appeared in %d draws (%d characters); the sampler is biased or the alphabet is truncated",
			len(symbols), draws, draws*6)
	}
}
