// The vehicle OWNER's display name, resolved inline on every vehicle read that
// needs it (MYR-581) — plus the boolean gate derived from the SAME expression.
//
// WHY BOTH LIVE HERE, AND WHY THEY ARE ONE FRAGMENT. Two surfaces consume this:
// the §7.0 catalog emits the owner's FIRST NAME on every row (a rider seeing
// "Amruth's Model X" instead of "Tesla"), and the ride-request gates refuse a car
// whose owner has NO resolvable name at all. Those two must never disagree — a
// row that shows a name while the gate calls the owner nameless, or the reverse,
// is the worst available outcome on this pair, because one of them is then
// lying about the very fact the other is enforcing. So the ladder is written
// ONCE, as `ownerNameLadderExpr`, and both the wire value and the predicate are
// spelled as derivations of it.
//
// THE THREE-SOURCE LADDER is the platform's, not a new one: Prisma "User" first
// (legacy/web owners), then the Apple first-consent name in go_identity_apple
// (the real name for an Apple-native owner), then go_users. It is
// character-for-character the precedence `requesterIdentitySelect` (MYR-229 /
// MYR-264) and `queryOwnerFirstNameSources` (MYR-368) already implement, and
// reusing it is the whole point: `User"."name` ALONE is not enough, because
// `queryProvisionUser`'s `ON CONFLICT DO NOTHING` leaves pre-existing rows with
// a NULL name that the Apple identity can still resolve — an owner who looks
// nameless to a one-table lookup and has a perfectly good name one rung down.
//
// CORRELATED SUBSELECTS RATHER THAN LEFT JOINS, and the semantics are the ones
// the fail-closed reading needs either way: a scalar subquery that matches no
// row evaluates to NULL, exactly as a LEFT JOIN's projected column would. A
// vanished owner therefore resolves to NULL → the gate refuses → fail closed.
// Subselects are used because that is the shape the two existing ladders use,
// and because three LEFT JOINs onto the catalog would multiply rows if any of
// the three ever gained a second row per user (go_identity_apple already can —
// its user_id index is not unique, which is why the middle rung carries
// ORDER BY last_login_at DESC LIMIT 1).
//
// TRIM IS LOAD-BEARING, not tidiness. `NULLIF(x, '')` alone lets a name of
// "   " through as non-NULL, which would make the gate say "named" while the
// first-token reduction below produced "" and the wire emitted null — the exact
// disagreement this file exists to prevent. With TRIM, whitespace-only is NULL
// at every rung, so `ownerNamed` is true if and only if `ownerFirstName`
// resolves to a non-empty token.
//
// P1. The resolved name is a person's name (data-classification.md §1.16). It
// is NEVER logged on any path — the gate logs the EVENT and the vehicle id, and
// the enforcement path reads the BOOLEAN only, never the value.

package store

// ownerNameLadderExpr is the ladder itself, keyed on the vehicle row's owner. It
// is a bare expression (no alias) so both consumers below can wrap it; it
// references `"Vehicle"."userId"` fully qualified, which is valid in every
// statement that selects it (the owner catalog, the shared catalog, the member
// catalog and the snapshot read all have `"Vehicle"` in scope).
const ownerNameLadderExpr = `COALESCE(
		NULLIF(TRIM((SELECT u."name" FROM "User" u WHERE u."id" = "Vehicle"."userId")), ''),
		NULLIF(TRIM((SELECT a."name" FROM go_identity_apple a WHERE a.user_id = "Vehicle"."userId" ORDER BY a.last_login_at DESC LIMIT 1)), ''),
		NULLIF(TRIM((SELECT g."name" FROM go_users g WHERE g.id = "Vehicle"."userId")), '')
	)`

// catalogOwnerNameExpr carries the owner's display name onto the three catalog
// reads. The FULL name, deliberately: the first-token reduction happens in Go
// (ownerFirstNameToken) so the MYR-229 reduction is reused rather than
// re-implemented in SQL, and so the two spellings cannot drift.
//
// Costs one probe per rung per row on a single-digit-row catalog: "User"."id"
// and go_users.id are primary keys and go_identity_apple.user_id is indexed, so
// each rung is an index probe bounded at one row. The same shape
// requesterIdentitySelect already pays per ride row.
const catalogOwnerNameExpr = ownerNameLadderExpr + ` AS "ownerName"`

// ownerNamedPredicate is the GATE, derived from the same ladder: does this car's
// owner have a name we could show a counterparty at all? INPUT ONLY — it is the
// one thing the ride-request enforcement path reads, so the P1 value itself
// never enters it (the MYR-578 raw-badge precedent: carry the input, not the
// display value).
const ownerNamedPredicate = `(` + ownerNameLadderExpr + `) IS NOT NULL AS "ownerNamed"`

// ownerFirstNameToken reduces a resolved owner name to the FIRST-NAMES-ONLY form
// the platform delivers to counterparties, as a nullable wire value.
//
// The reduction is `firstNameToken` (ride_request_requester.go, MYR-229) — the
// same function behind RideRequest.requesterName and the push copy — so "Ada
// Lovelace" and "  Ada  Lovelace " both reduce to "Ada" on every surface. The
// P1 policy is first names only to counterparties, the same policy as
// RedeemShareInviteResponse.ownerFirstName.
//
// Returns nil for a nil input and for anything that reduces to the empty
// string, so "no name" has exactly ONE spelling on the wire (null) and the
// empty string can never reach a consumer as a name.
func ownerFirstNameToken(name *string) *string {
	if name == nil {
		return nil
	}
	token := firstNameToken(*name)
	if token == "" {
		return nil
	}
	return &token
}
