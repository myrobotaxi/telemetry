package store

import (
	"strings"
	"testing"
)

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
	if !strings.Contains(catalogOwnerNameExpr, ownerNameLadderExpr) {
		t.Error("catalogOwnerNameExpr no longer derives from ownerNameLadderExpr — " +
			"the wire value and the offerability gate can now disagree")
	}
	if !strings.Contains(ownerNamedPredicate, ownerNameLadderExpr) {
		t.Error("ownerNamedPredicate no longer derives from ownerNameLadderExpr — " +
			"the offerability gate and the wire value can now disagree")
	}
	// The gate must be a boolean projection, never the name: the P1 value must not
	// be reachable from the enforcement path.
	if !strings.Contains(ownerNamedPredicate, "IS NOT NULL") {
		t.Error("ownerNamedPredicate must project a BOOLEAN — the enforcement path never handles the name")
	}
	// Every rung must TRIM. Without it a whitespace-only name reads as NULL to the
	// reducer above but NOT NULL to the gate.
	if got := strings.Count(ownerNameLadderExpr, "NULLIF(TRIM("); got != 3 {
		t.Errorf("ladder has %d TRIM-guarded rungs, want 3 — an untrimmed rung lets the gate and the wire disagree", got)
	}
}

// TestConfirmationGateIsSharedAndScoped is MYR-583's structural assertion, and it
// exists for the same reason the one above it does: the correctness argument is
// that the emit and the gate acquire the confirmation conjunction from ONE place,
// and two copies that agree today would pass every behavioural test and drift on
// the next edit.
//
// It also pins the two DELIBERATE EXEMPTIONS. Both are decisions with reasons
// (see ride_request_queries.go and vehicle_share_queries.go), and a decision with
// a reason is exactly the kind of thing a later "consistency" refactor removes
// without reading it. If either exemption is revisited, this test is where the
// revisit must be stated out loud.
func TestConfirmationGateIsSharedAndScoped(t *testing.T) {
	gated := map[string]struct{ sql, probe string }{
		"ownerNameLadderExpr": {ownerNameLadderExpr, ownerNameConfirmedExpr},
		"acceptedByNameExpr":  {acceptedByNameExpr, acceptedByNameConfirmedExpr},
	}
	for name, g := range gated {
		t.Run(name+" carries the confirmation probe", func(t *testing.T) {
			if !strings.Contains(g.sql, g.probe) {
				t.Errorf("%s no longer requires a confirmation — an unconfirmed name would be "+
					"published to a counterparty again (MYR-583)", name)
			}
		})
	}

	// The pair the wire and the gate share: both must reach the confirmation
	// through the ladder, not around it.
	for name, sql := range map[string]string{
		"catalogOwnerNameExpr": catalogOwnerNameExpr,
		"ownerNamedPredicate":  ownerNamedPredicate,
	} {
		t.Run(name+" inherits the gate from the shared ladder", func(t *testing.T) {
			if !strings.Contains(sql, ownerNameConfirmedExpr) {
				t.Errorf("%s no longer carries the confirmation conjunction — the emit and the "+
					"offerability gate can now disagree about whether an owner is named", name)
			}
		})
	}

	// The UNGATED ladders, asserted as such. `requesterName` keeps naming a rider
	// to the owner deciding whether to admit them (removing it would reintroduce
	// "Tesla wants a ride" on the very card the report came from), and the redeem
	// ladder falls through to an EMAIL LOCAL-PART, so gating it would disclose
	// MORE about the owner rather than less.
	for name, sql := range map[string]string{
		"requesterIdentitySelect":    requesterIdentitySelect,
		"queryOwnerFirstNameSources": queryOwnerFirstNameSources,
	} {
		t.Run(name+" is deliberately NOT gated", func(t *testing.T) {
			if strings.Contains(sql, "go_profile_name_confirmations") {
				t.Errorf("%s acquired a confirmation gate. That may be right, but it is a "+
					"CONTRACT CHANGE with a documented reason against it — update "+
					"rest-api.md and this test together, or revert", name)
			}
		})
	}
}

// TestEveryNameLadderTrimsEveryRung is the CROSS-LADDER invariant, and it is the
// one assertion in this file that spans files.
//
// The platform resolves "what is this person called?" through four sibling SQL
// ladders — this package's `ownerNameLadderExpr` (the vehicle catalog + the
// offerability gate), `acceptedByNameExpr` (the share listing),
// `requesterIdentitySelect` (the ride surfaces) and `queryOwnerFirstNameSources`
// (the redeem screen and the push copy). They cannot yet be ONE constant: each
// keys its subselects on a different column, and the statements that embed them
// are `const`, so a key-parameterized helper would force them all to `var`.
//
// What they CAN be is identical in spelling, and this test is what holds that.
// Until MYR-581 the two older ladders omitted `TRIM`, which is not cosmetic: a top
// rung holding "   " won its COALESCE outright, so the rungs below were never
// consulted and a real name one rung down was skipped in favour of an email
// local-part. A new ladder added without `TRIM`, or an old one that loses it,
// reintroduces exactly that.
func TestEveryNameLadderTrimsEveryRung(t *testing.T) {
	ladders := map[string]string{
		"ownerNameLadderExpr":        ownerNameLadderExpr,
		"acceptedByNameExpr":         acceptedByNameExpr,
		"requesterIdentitySelect":    requesterIdentitySelect,
		"queryOwnerFirstNameSources": queryOwnerFirstNameSources,
	}
	for name, sql := range ladders {
		t.Run(name, func(t *testing.T) {
			// Every rung that reads a name or an email must be TRIM-guarded, so the
			// count of guarded rungs must equal the count of NULLIFs outright — a
			// bare `NULLIF(` anywhere is an untrimmed rung.
			guarded := strings.Count(sql, "NULLIF(TRIM(")
			total := strings.Count(sql, "NULLIF(")
			if guarded != total {
				t.Errorf("%s has %d NULLIF rungs but only %d are TRIM-guarded — "+
					"an untrimmed rung lets a whitespace-only name win the COALESCE and "+
					"hide a real name below it", name, total, guarded)
			}
			if total == 0 {
				t.Errorf("%s has no NULLIF rungs at all — this test is no longer looking at a ladder", name)
			}
		})
	}
}

func ptr(s string) *string { return &s }
