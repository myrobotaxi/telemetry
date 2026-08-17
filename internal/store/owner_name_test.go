package store

import "testing"

// The PURE half of MYR-581's owner-name resolution: the first-token reduction and
// the nullable collapse. No database — the ladder itself is SQL and is exercised
// against a real Postgres by TestVehicleRepo_GetDispatchState (the gate arm) and
// the catalog list tests.
//
// What matters here is the two-way correspondence with the SQL, because the whole
// design rests on it: `ownerNamedPredicate` says a car's owner is NAMED exactly
// when the ladder is NOT NULL, and this function must produce a non-nil first name
// under exactly the same condition. The SQL's `TRIM`/`NULLIF` is what makes that
// hold for whitespace-only names — hence the arms below that look redundant.

func TestOwnerFirstNameToken(t *testing.T) {
	tests := []struct {
		name string
		in   *string
		want *string
	}{
		{
			name: "a single-token name passes through",
			in:   ptr("Amruth"),
			want: ptr("Amruth"),
		},
		{
			name: "a full name is reduced to its FIRST token — the P1 counterparty policy",
			in:   ptr("Ada Lovelace"),
			want: ptr("Ada"),
		},
		{
			name: "surrounding and internal whitespace collapses, matching MYR-229's requesterName",
			in:   ptr("  Ada   Lovelace "),
			want: ptr("Ada"),
		},
		{
			name: "a three-part name still yields one token",
			in:   ptr("Mira Chen Rodriguez"),
			want: ptr("Mira"),
		},
		{
			name: "a non-Latin name is not special-cased — it is somebody's name",
			in:   ptr("Ольга Иванова"),
			want: ptr("Ольга"),
		},
		{
			name: "NULL from the ladder stays nil — the owner has no name on any rung",
			in:   nil,
			want: nil,
		},
		{
			name: "the empty string collapses to nil, so it can NEVER reach the wire as a name",
			in:   ptr(""),
			want: nil,
		},
		{
			// The SQL's TRIM should already have made this NULL. Asserting it here
			// too is the belt to that braces: if a rung ever loses its TRIM, the
			// gate would say "named" while this function produced "", and the wire
			// value and the gate would disagree — the one failure the shared ladder
			// exists to prevent. Collapsing to nil keeps the wire honest even then.
			name: "a whitespace-only name collapses to nil, agreeing with the SQL's TRIM",
			in:   ptr("   "),
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ownerFirstNameToken(tc.in)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %q, want nil", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("got nil, want %q", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("got %q, want %q", *got, *tc.want)
			}
		})
	}
}

// TestOwnerNameLadderSharesOneDefinition is a STRUCTURAL assertion, and it earns
// its place: the entire correctness argument for MYR-581 is that the catalog's
// emitted name and the ride-request gate are derived from ONE ladder expression.
// Two copies that happened to agree today would satisfy every behavioural test in
// the repository and drift on the next edit.
func TestOwnerNameLadderSharesOneDefinition(t *testing.T) {
	if !containsSub(catalogOwnerNameExpr, ownerNameLadderExpr) {
		t.Error("catalogOwnerNameExpr no longer derives from ownerNameLadderExpr — " +
			"the wire value and the offerability gate can now disagree")
	}
	if !containsSub(ownerNamedPredicate, ownerNameLadderExpr) {
		t.Error("ownerNamedPredicate no longer derives from ownerNameLadderExpr — " +
			"the offerability gate and the wire value can now disagree")
	}
	// The gate must be a boolean projection, never the name: the P1 value must not
	// be reachable from the enforcement path.
	if !containsSub(ownerNamedPredicate, "IS NOT NULL") {
		t.Error("ownerNamedPredicate must project a BOOLEAN — the enforcement path never handles the name")
	}
	// Every rung must TRIM. Without it a whitespace-only name reads as NULL to the
	// reducer above but NOT NULL to the gate.
	if got := countSub(ownerNameLadderExpr, "NULLIF(TRIM("); got != 3 {
		t.Errorf("ladder has %d TRIM-guarded rungs, want 3 — an untrimmed rung lets the gate and the wire disagree", got)
	}
}

func ptr(s string) *string { return &s }

// containsSub / countSub keep this file free of a strings import solely for two
// assertions, matching the no-frills style of the other pure store tests.
func containsSub(haystack, needle string) bool { return countSub(haystack, needle) > 0 }

func countSub(haystack, needle string) int {
	if needle == "" {
		return 0
	}
	n := 0
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			n++
		}
	}
	return n
}
