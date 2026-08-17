// The ACCEPTING account's display name on the vehicle-sharing grant row
// (MYR-581). Split out of vehicle_share_queries.go under the 300-line rule: it is
// one expression, but the reason it exists is an argument about a DIFFERENT column
// on the same row, and that argument does not belong in the middle of an
// authorization-relevant statement set.

package store

// acceptedByNameExpr resolves the display name of the ACCEPTING account
// (MYR-581) — the person who redeemed this invite — inline, in the same statement
// as the grant row.
//
// WHY IT IS NEEDED AT ALL. An accepted row carries only `label`, which is the
// OWNER'S OWN MEMO ("Mom", "Mira Chen") typed before anybody redeemed anything.
// It is not the redeemer's name, it is never resolved against an account, and a
// code forwarded to somebody else makes it flatly wrong. So until now the owner's
// Share tab could not show who actually holds a grant, only who the owner
// intended to invite — which is what the client asked for: the name the shared
// user entered at signup.
//
// SAME LADDER, SAME REDUCTION as the vehicle catalog's ownerFirstName
// (owner_name.go) — Prisma "User" first, then the Apple first-consent name, then
// go_users; the FULL name is selected here and reduced to its first token in Go
// by the shared `ownerFirstNameToken`. First names only, the same P1 counterparty
// policy: this is a grantee's name delivered to the owner, so it gets exactly the
// treatment the owner's name gets when delivered to the grantee.
//
// A SEPARATE EXPRESSION rather than a reuse of `ownerNameLadderExpr`, and it has
// to be: that constant keys its subselects on `"Vehicle"."userId"`, and no
// statement here joins the vehicle relation. This one keys on
// `accepted_by_user_id` — nullable, and that is exactly what makes a PENDING row
// resolve to NULL for free: a scalar subselect compared against NULL matches no
// row, so no `CASE WHEN status = 'accepted'` guard is needed and none is written.
//
// It rides every statement that projects shareColumns, including the resend
// RETURNING — where the rows are pending by definition and it resolves NULL,
// which is correct and costs a comparison against NULL.
const acceptedByNameExpr = `COALESCE(
		NULLIF(TRIM((SELECT u."name" FROM "User" u WHERE u."id" = accepted_by_user_id)), ''),
		NULLIF(TRIM((SELECT a."name" FROM go_identity_apple a WHERE a.user_id = accepted_by_user_id ORDER BY a.last_login_at DESC LIMIT 1)), ''),
		NULLIF(TRIM((SELECT g."name" FROM go_users g WHERE g.id = accepted_by_user_id)), '')
	) AS accepted_by_name`
