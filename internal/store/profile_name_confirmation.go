// The CONFIRMATION half of the display-name story (MYR-583): the one place that
// spells "has this person confirmed the name we show other people?" in SQL.
//
// ── THE RULING ───────────────────────────────────────────────────────────────
//
// Client, 2026-08-17, verbatim: "Any names not entered/confirmed should be in
// Setting Up. So please ensure even cars with names but not confirmed show that
// label." A NAME ONLY COUNTS ONCE ITS OWNER HAS CONFIRMED IT through the MYR-581
// prompt. A ladder-resolvable name nobody confirmed must read as ABSENT — both on
// the wire and to the offerability gate.
//
// ── WHY A NAME CAN EXIST WITHOUT CONSENT ─────────────────────────────────────
//
// Two of the ladder's three rungs are filled by machinery the person never saw:
// `go_identity_apple.name` comes from Apple's FIRST-consent payload (sent once,
// forwarded by the client, never shown to anybody for approval), and Prisma
// `"User".name` may hold whatever the legacy web onboarding wrote — including the
// literal placeholder 'Tesla User' for 2026-03 web-era accounts. Before MYR-583
// the ladder could not tell either of those from a name somebody deliberately
// typed. A RESOLVABLE NAME IS NOT EVIDENCE OF CONSENT.
//
// ── WHY THE FRAGMENTS LIVE HERE AND NOT WITH THEIR CONSUMERS ─────────────────
//
// Three surfaces gate on this fact — the catalog's `ownerFirstName`, the
// offerability predicate `ownerNamed`, and the share listing's `acceptedByName` —
// and they must gate on it IDENTICALLY. owner_name.go's whole argument is that
// the wire value and the gate are derived from ONE ladder so they cannot
// disagree; that argument survives MYR-583 only if the confirmation conjunction
// is applied INSIDE that one ladder, from one written-once fragment. So this file
// owns the SQL and the consumers wrap it.
//
// TWO FRAGMENTS, NOT ONE, and for exactly the reason acceptedByNameExpr already
// duplicates the ladder itself (see vehicle_share_accepted_name.go): the SUBJECT
// differs. One keys on `"Vehicle"."userId"`, the other on `accepted_by_user_id`,
// and the statements that embed them are `const` — a subject-parameterized helper
// would force every embedding query in the package to `var`. They are written
// adjacently, from the same template, so a reader can see at a glance that they
// say the same thing about different columns.
//
// ── WHAT IS DELIBERATELY *NOT* GATED ─────────────────────────────────────────
//
// `requesterIdentitySelect` (RideRequest.requesterName) and
// `queryOwnerFirstNameSources` (the redeem screen + push copy) keep resolving
// unconfirmed names. Both exemptions are argued at their own definitions; the
// short of it is that gating them would REMOVE information a party already
// relies on, or replace a real name with something worse (an email local-part).
//
// P0. Nothing in this file touches a name. The table holds an opaque cuid and a
// timestamp, which is what lets the offerability gate consult consent without the
// P1 value entering the enforcement path.

package store

// queryUpsertProfileNameConfirmation records that the caller has confirmed their
// display name. Run INSIDE the MYR-581 name-write transaction
// (ProfileNameRepo.UpdateUserName), never on its own — a confirmation that
// committed while the name write rolled back would mark a stale name as
// approved, which is the one outcome worse than no confirmation at all.
//
// UPSERT rather than INSERT ... DO NOTHING: `confirmed_at` tracks the MOST RECENT
// confirmation. Nothing reads it as a deadline (every consumer asks only whether
// the row exists), so the choice is purely about which timestamp is more useful
// to an operator, and "when did they last accept this name" answers more
// questions than "when did they first".
//
// Keyed on the caller's authenticated subject — the SAME key the three name
// UPDATEs beside it use. That matters for the converged-identity case (MYR-452):
// if a caller's token names an abandoned alias, their rename lands on the alias's
// rows and their confirmation lands under the alias too, so the pair stays
// consistent. It is ONE pre-existing limitation of the rename endpoint, not a new
// second one introduced here.
const queryUpsertProfileNameConfirmation = `
INSERT INTO go_profile_name_confirmations (user_id, confirmed_at)
VALUES ($1, NOW())
ON CONFLICT (user_id) DO UPDATE SET confirmed_at = NOW()`

// ownerNameConfirmedExpr is the confirmation probe keyed on the VEHICLE ROW'S
// OWNER. Valid in every statement that has `"Vehicle"` in scope (the owner
// catalog, the shared catalog, the member catalog, the snapshot read) — the same
// scope requirement `ownerNameLadderExpr` already carries.
//
// EXISTS, not a join and not a COUNT: the question is binary, the primary key
// answers it with one index probe, and a scalar subquery against a missing row
// would need its own NULL handling. A vanished owner has no row, so EXISTS is
// FALSE and the gate fails CLOSED — the same direction the ladder's own NULL
// resolves in.
const ownerNameConfirmedExpr = `EXISTS (
			SELECT 1 FROM go_profile_name_confirmations pnc
			WHERE pnc.user_id = "Vehicle"."userId"
		)`

// acceptedByNameConfirmedExpr is the SAME probe keyed on the accepting account of
// a share grant. Written out rather than shared with the constant above because
// the subject column differs and the embedding statements are `const`.
//
// `accepted_by_user_id` is NULLABLE, and that is free correctness rather than a
// gap: on a PENDING row the comparison is against NULL, no row matches, EXISTS is
// FALSE, and the name resolves absent — which is exactly what a pending row
// should render, with no `CASE WHEN status = 'accepted'` guard needed.
const acceptedByNameConfirmedExpr = `EXISTS (
			SELECT 1 FROM go_profile_name_confirmations pnc
			WHERE pnc.user_id = accepted_by_user_id
		)`

// queryDeleteProfileNameConfirmationForUser removes the person's confirmation
// row. The ONLY delete this feature has, and it belongs to account deletion
// (step 8b of the rest-api.md §7.6 sequence) — confirmation is otherwise
// monotonic. Idempotent: a re-run affects zero rows.
const queryDeleteProfileNameConfirmationForUser = `
DELETE FROM go_profile_name_confirmations WHERE user_id = $1`
