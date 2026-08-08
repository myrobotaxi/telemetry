package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-447, §2.3.1. The privacy page will say that an audit row surviving an
// account deletion carries "an orphaned opaque identifier, enum values,
// timestamps and integer counts — no name, no email, no address, no
// coordinate, no token". §4.4 asserts it and CG-DL-5 requires it, but until
// now nothing enforced it: the metadata is a free-form JSONB column and any
// future writer could widen it without a single test going red.
//
// This is that enforcement, for the rows that outlive the person. It is the
// deliberate alternative to anonymizing or expiring those rows — see the
// decision record in data-lifecycle.md §2.3.1 for why neither of those was
// adopted. If the record cannot be made anonymous, it must at least be
// provably minimal, and "provably" means a failing test when it stops being.

// deletionAuditPII is a fixture whose every field is the kind of value that
// must never reach an audit row. The strings are marker-prefixed on purpose:
// the assertions work by scanning the raw metadata JSON for them, and a
// realistic-but-bland value like "Alice" would hide in the noise of a cuid.
// The first two are PLANTED — they are the name and email seeded onto the
// user below, so those assertions are live: if the deletion tally ever grew
// a field carrying them, this fails.
//
// The rest are FORWARD GUARDS, and it is worth being explicit that they are
// not planted anywhere, because a reader could otherwise mistake them for
// live coverage. They cannot catch a leak from elsewhere in the system;
// what they catch is a future writer adding a label, an address or a
// coordinate to THIS row's metadata, which is the realistic way a P0 tally
// stops being one.
var deletionAuditPII = []string{
	"MYR447-NAME-Priya-Patel",
	"MYR447-EMAIL-priya@icloud.com",
	"MYR447-LABEL-Mira-Chen",
	"MYR447-ADDRESS-1600-Pennsylvania-Ave",
	"37.7749",
	"-122.4194",
}

// TestAccountDeletionAuditMetadataIsStructurallyP0 pins the SHAPE, not just the
// absence of today's known leaks. Every value in the surviving row's metadata
// must be a number or a boolean.
//
// Why shape rather than a denylist: a denylist only catches the PII somebody
// already thought of. The deletion tally is, by construction, counts and flags,
// so "no strings at all" is a property the correct implementation satisfies for
// free and an incorrect one cannot. A future field holding a name, an address,
// a label or a coordinate fails this test on the type alone, before anyone has
// to guess what it might be called.
func TestAccountDeletionAuditMetadataIsStructurallyP0(t *testing.T) {
	setupAccountDeletion(t)
	seedGoUser(t, delUserApple, strPtr(deletionAuditPII[0]), strPtr(deletionAuditPII[1]))

	counts := store.AccountDeletionCounts{
		VehicleCount: 1, DriveCount: 4, RidesCancelled: 2,
		SharesRevoked: 1, ShareLabelsScrubbed: 1, PushDevicesDeleted: 1,
		SavedPlacesDeleted: 2, RefreshTokensRevoked: 1,
	}
	res, err := newAccountDeleter(t).DeleteIdentity(context.Background(), soloScope(delUserApple), counts)
	if err != nil {
		t.Fatalf("DeleteIdentity: %v", err)
	}

	var raw []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT "metadata" FROM "AuditLog" WHERE "id" = $1`, res.AuditLogID).Scan(&raw); err != nil {
		t.Fatalf("read audit metadata: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("metadata is empty — the test would pass vacuously")
	}

	for k, v := range got {
		switch v.(type) {
		case float64, bool:
			// A count or a flag. This is the whole permitted vocabulary.
		default:
			t.Errorf("metadata key %q is %T (%v); CG-DL-5 permits only counts "+
				"and flags in a row that outlives the person it describes", k, v, v)
		}
	}
}

// TestAccountDeletionAuditRowCarriesNoPII is the complementary belt-and-braces
// pass: whatever the shape, none of the person's actual data may appear
// ANYWHERE in the row. Asserted over the concatenated columns rather than the
// parsed metadata, so a value nested inside an object or smuggled into
// targetId could not slip past the shape check above.
//
// The one identifier that IS expected to survive is the user's cuid — that is
// the deliberate design of §4.5, and the deletion proof is worthless without
// it. This test asserts it is the ONLY thing that survives.
func TestAccountDeletionAuditRowCarriesNoPII(t *testing.T) {
	setupAccountDeletion(t)
	seedGoUser(t, delUserApple, strPtr(deletionAuditPII[0]), strPtr(deletionAuditPII[1]))

	res, err := newAccountDeleter(t).DeleteIdentity(context.Background(), soloScope(delUserApple),
		store.AccountDeletionCounts{VehicleCount: 1, ShareLabelsScrubbed: 2})
	if err != nil {
		t.Fatalf("DeleteIdentity: %v", err)
	}

	var userID, action, targetType, targetID, initiator string
	var metadata []byte
	if err := testPool.QueryRow(context.Background(), `
		SELECT "userId", "action", "targetType", "targetId", "initiator", "metadata"
		FROM "AuditLog" WHERE "id" = $1`, res.AuditLogID).
		Scan(&userID, &action, &targetType, &targetID, &initiator, &metadata); err != nil {
		t.Fatalf("read audit row: %v", err)
	}

	whole := strings.Join([]string{userID, action, targetType, targetID, initiator, string(metadata)}, "|")
	for _, needle := range deletionAuditPII {
		if strings.Contains(whole, needle) {
			t.Errorf("the surviving audit row carries %q — the privacy page's "+
				"claim that a deleted account leaves only an opaque identifier "+
				"is false. Row: %s", needle, whole)
		}
	}

	// Non-vacuity: the row must actually be about this person, or the sweep
	// above proves nothing.
	if userID != delUserApple || targetID != delUserApple {
		t.Fatalf("audit row is not keyed to the deleted user: userId=%q targetId=%q", userID, targetID)
	}
}
