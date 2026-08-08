package store_test

import (
	"context"
	"testing"
)

// MYR-447: when the person a share was granted TO deletes their account, the
// owner-typed label on that grant is their NAME, written by somebody else, and
// before this change it survived the deletion indefinitely.
//
// These run against real Postgres because what is under test is the row set a
// predicate selects — precisely the thing a mock cannot get wrong on your
// behalf.

// seedShareGrantLabelled is seedShareGrant with the label under the test's
// control. The shared helper hardcodes 'Guest', which is useless here: the
// whole point is which label survives and which does not.
func seedShareGrantLabelled(t *testing.T, id, vehicleID, ownerID, acceptedBy, status, label string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO go_vehicle_shares
			(id, vehicle_id, owner_user_id, label, permission, code, status,
			 expires_at, accepted_at, accepted_by_user_id)
		VALUES ($1, $2, $3, $4, 'rides', $5, $6, NOW() + INTERVAL '7 days', NOW(), $7)`,
		id, vehicleID, ownerID, label, "C"+id[len(id)-5:], status, acceptedBy); err != nil {
		t.Fatalf("seed labelled share grant %s: %v", id, err)
	}
}

func shareLabelOf(t *testing.T, id string) string {
	t.Helper()
	var label string
	if err := testPool.QueryRow(context.Background(),
		`SELECT label FROM go_vehicle_shares WHERE id = $1`, id).Scan(&label); err != nil {
		t.Fatalf("read label %s: %v", id, err)
	}
	return label
}

// TestScrubSharesReceivedLabel_ErasesEveryGrantTheUserRedeemed is the core
// claim. Note the ALREADY-REVOKED row: it is the reason this is a separate step
// rather than another SET clause on the revocation, which is guarded by
// `status <> 'revoked'` and would skip exactly that row.
func TestScrubSharesReceivedLabel_ErasesEveryGrantTheUserRedeemed(t *testing.T) {
	setupAccountDeletion(t)
	deleter := newAccountDeleter(t)

	// go_vehicle_shares carries no foreign keys (CG-DL-9 forbids them across
	// the Go/Prisma boundary), so the grants stand alone — no User or Vehicle
	// row is needed to make this predicate meaningful.
	//
	// Three grants redeemed by the departing user, in three different states.
	seedShareGrantLabelled(t, "cshr447accepted", "cveh447a", delUserOwner, delUserApple, "accepted", "Mira Chen")
	seedShareGrantLabelled(t, "cshr447revoked", "cveh447b", delUserOwner, delUserApple, "revoked", "Mira Chen (old phone)")
	// A grant redeemed by SOMEBODY ELSE, on the same owner's car. Must survive
	// untouched: it is a different person's name in a record that is not going
	// anywhere.
	seedShareGrantLabelled(t, "cshr447bystander", "cveh447a", delUserOwner, delUserOther, "accepted", "Roommate")

	n, err := deleter.ScrubSharesReceivedLabel(context.Background(), delUserApple)
	if err != nil {
		t.Fatalf("ScrubSharesReceivedLabel: %v", err)
	}
	if n != 2 {
		t.Fatalf("scrubbed %d rows, want 2 (the accepted grant AND the already-revoked one)", n)
	}

	for _, id := range []string{"cshr447accepted", "cshr447revoked"} {
		if got := shareLabelOf(t, id); got != "" {
			t.Errorf("share %s still carries the deleted person's name %q", id, got)
		}
	}
	if got := shareLabelOf(t, "cshr447bystander"); got != "Roommate" {
		t.Errorf("a bystander's label was scrubbed: got %q, want %q", got, "Roommate")
	}
}

// TestScrubSharesReceivedLabel_IsIdempotent pins the property the whole
// deletion sequence depends on: any step may be re-run after a mid-sequence
// failure, and the re-run must affect zero rows rather than report work.
func TestScrubSharesReceivedLabel_IsIdempotent(t *testing.T) {
	setupAccountDeletion(t)
	deleter := newAccountDeleter(t)
	ctx := context.Background()

	seedShareGrantLabelled(t, "cshr447idem", "cveh447c", delUserOwner, delUserApple, "accepted", "Mira Chen")

	first, err := deleter.ScrubSharesReceivedLabel(ctx, delUserApple)
	if err != nil {
		t.Fatalf("first scrub: %v", err)
	}
	if first != 1 {
		t.Fatalf("first scrub affected %d rows, want 1", first)
	}

	second, err := deleter.ScrubSharesReceivedLabel(ctx, delUserApple)
	if err != nil {
		t.Fatalf("second scrub: %v", err)
	}
	if second != 0 {
		t.Fatalf("re-run affected %d rows, want 0 — the step is not idempotent", second)
	}
}

// TestScrubSharesReceivedLabel_RejectsEmptyUserID guards the predicate against
// the one input that would make it match a row set nobody intended.
func TestScrubSharesReceivedLabel_RejectsEmptyUserID(t *testing.T) {
	setupAccountDeletion(t)
	deleter := newAccountDeleter(t)

	if _, err := deleter.ScrubSharesReceivedLabel(context.Background(), "  "); err == nil {
		t.Fatal("expected an error for a blank user id, got nil")
	}
}
