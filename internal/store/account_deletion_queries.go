// SQL for the account-deletion writer (MYR-355): the PER-STEP, user-scoped
// statements — steps 1 and 4-9 of data-lifecycle.md §3.1, each run on its own
// over one id and outside any transaction. See account_deletion.go for the
// methods that run them.
//
// The identity transaction's statements (step 10) and the MYR-452 convergence
// walk live in account_deletion_identity_queries.go, split off when MYR-596's
// step 8e took this file past the 300-line cap. The seam is the one
// account_deletion.go / account_deletion_identity.go already draw.

package store

// queryRevokeSharesReceived tombstones every live grant the deleted user
// REDEEMED (the mirror image of queryRevokeSharesForVehicle, which tombstones
// every grant ON a car). Revocation is a tombstone, not a delete (migration
// 0020): the owner's audit trail of who could see their car outlives the
// viewer's account. `status <> 'revoked'` makes a re-run affect zero rows
// instead of re-stamping revoked_at, so the step is idempotent.
const queryRevokeSharesReceived = `
UPDATE go_vehicle_shares
SET status = 'revoked', revoked_at = NOW()
WHERE accepted_by_user_id = $1 AND status <> 'revoked'`

// queryScrubSharesReceivedLabel erases the owner-typed label from every share
// row the deleted user had REDEEMED (MYR-447).
//
// WHAT THE LABEL IS, AND WHY IT OUTLIVING THE PERSON IS THE BUG. `label` is a
// free-text name the CAR OWNER typed for their own list — "Mira Chen", "Mom",
// "Roommate" (data-classification.md §1.15). It is P1, it is a third party's
// name, and it is the deleted user's name. Revocation alone (the step above)
// leaves it in place forever: `queryRevokeSharesReceived` moves `status` and
// stamps `revoked_at` and touches nothing else, so before MYR-447 a person
// could delete their account and have their name persist indefinitely in a
// stranger's row, keyed by a cuid that resolves to nothing.
//
// WHY THIS IS A SEPARATE STATEMENT rather than another SET clause on the
// revoke. The revoke is guarded by `status <> 'revoked'` for idempotency, which
// means it deliberately skips rows that were ALREADY revoked — by the owner, or
// by an earlier partial run of this very sequence. Those rows carry the same
// name and are exactly as stale, so folding the scrub into the revoke would
// leave the oldest and most-forgotten labels untouched. This statement is
// therefore keyed only on the person, not on the status.
//
// SCRUB VALUE. `label` is TEXT NOT NULL (migration 0020) so the scrub writes
// the empty string rather than NULL — chosen over widening the column because
// `store.CreateInvite` rejects a blank label at the door, which makes the empty
// string an unambiguous scrubbed-here sentinel that no live row can hold. The
// `label <> ”` predicate then makes a re-run affect zero rows, so the step is
// idempotent on the same terms as every other step in the sequence.
//
// NO WIRE EFFECT. A revoked grant is never serialized to any client (`status`
// has no `revoked` wire member), and the label was owner-facing only — it is
// never delivered to the invited party. So the owner loses a name on a row they
// can no longer see, and nobody's UI changes.
const queryScrubSharesReceivedLabel = `
UPDATE go_vehicle_shares
SET label = ''
WHERE accepted_by_user_id = $1 AND label <> ''`

// queryDeletePushDevicesForUser drops the whole APNs address book for one
// person. Unlike the sign-out unregister (queryDeletePushDevice) this is not
// scoped to one token: the account is going away, so every installation it
// still owns must stop receiving its ride notifications. Deleting zero rows on
// a re-run is a clean no-op.
//
// This is deliberately a DELETE rather than a tombstone: go_push_devices is an
// address book, not a ledger — a stale row is a live capability (anyone who
// resolves this cuid would push to that phone), and Apple's own 410 self-heal
// path already deletes rather than marks.
const queryDeletePushDevicesForUser = `
DELETE FROM go_push_devices WHERE user_id = $1`

// queryDeleteSavedPlacesForUser drops both saved-place slots for one person
// (MYR-321). Not scoped to a kind, unlike the §7.20 per-slot delete: the
// account is going away, so every place it saved goes with it.
//
// A DELETE rather than a tombstone, and this is the one step in the sequence
// where that choice is not merely conventional. The rows hold AES-256-GCM
// ciphertext of where this person LIVES (data-classification.md §1.17) — a
// durable home coordinate, not a one-off trip. A tombstone would keep exactly
// that ciphertext around after somebody exercised their right to erasure, and
// no counterparty is owed a record that this account once knew its owner's
// address: unlike a revoked share, which is evidence in the CAR OWNER's audit
// trail, a saved place is a person's own note to themselves. Nothing outlives
// them here.
//
// Deleting zero rows on a re-run is a clean no-op.
const queryDeleteSavedPlacesForUser = `
DELETE FROM go_saved_places WHERE user_id = $1`

// queryDeleteUserActivityForUser drops the person's last-seen row (MYR-592,
// migration 0043).
//
// A DELETE rather than a tombstone, and the reason is the value's TIER rather
// than its shape. The row is a cuid and one timestamp, but the timestamp is a
// BEHAVIOURAL observation about a person — when they were last using the
// product — which is why it is classified P1 (data-classification.md §1.21)
// while its structurally identical neighbour go_profile_name_confirmations is
// P0. Keeping a behavioural record after somebody has exercised their right to
// erasure is exactly the thing erasure is for, and no counterparty is owed it:
// unlike a revoked share, nobody else's audit trail contains this fact.
//
// It also has to go for a plainer reason. The inactivity sweeper reads this
// table; a surviving row for a deleted account is a person the sweeper still
// believes in. Today nothing follows from that (the candidate query joins from
// a live vehicle, and the account's vehicles went in step 3), but the row would
// be waiting for the first query that forgets to.
//
// Deleting zero rows on a re-run is a clean no-op.
const queryDeleteUserActivityForUser = `
DELETE FROM go_user_activity WHERE user_id = $1`

// queryDeleteTeslaTokenKeepaliveForUser drops the person's keepalive
// bookkeeping (MYR-594, migration 0045).
//
// HYGIENE RATHER THAN AN ERASURE OBLIGATION, which is the difference from its
// neighbour above and worth stating so a later reader does not assume the
// stronger duty applies here too. Every column is P0
// (data-classification.md §1.23): an opaque cuid, three timestamps about a
// PLATFORM ACTION, and a copy of an expiry integer. Nothing in it is a
// behavioural observation about the person, and nothing in it is a credential —
// the grant itself lives encrypted on the "Account" row that step 10 removes.
//
// It goes anyway, for the reason the last-seen row's comment gives second: a
// surviving row is an account the keepalive arm still holds a cooldown for,
// keyed by a cuid nothing resolves. Nothing follows from that today — the
// candidate query joins from a live suspended vehicle (gone in step 3) and a
// live Tesla OAuth row (gone in step 10), so the arm cannot reach it — but a
// row waiting for the first query that forgets to join is exactly the orphan
// class this sequence exists to prevent.
//
// Deleting zero rows on a re-run is a clean no-op, and zero is the ordinary
// result: a row exists only for an owner the keepalive arm has actually tried.
//
// #nosec G101 -- a table name containing "token", not a credential. The table
// holds no token material at all; that is the point of it.
const queryDeleteTeslaTokenKeepaliveForUser = `
DELETE FROM go_tesla_token_keepalive WHERE user_id = $1`

// queryDeleteRemovedVehiclesForUser drops the person's removed-vehicle
// tombstones (MYR-596; the table is MYR-261, migration 0006).
//
// THE TOMBSTONE PROTECTS A LIVE ACCOUNT AND NOTHING ELSE. Its whole job is to
// stop the best-effort post-link sync (OwnerProvisioner.UpsertOwnedVehicle)
// re-inserting a VIN the owner deliberately removed. That sync runs only for an
// account with a Tesla link, and after this sequence there is no account, no
// link and no sync — so a surviving row defends against a code path that can no
// longer execute. What is left is a (user_id, VIN) pair filed against a person
// who no longer exists, which is the orphan class §1.4.2 and §1.4.3 are already
// written against. The client ruled it goes ("remove everything", 2026-08-20).
//
// ORDERING IS NORMATIVE AND THIS IS THE ONE STEP IN THE 8-FAMILY THAT HAS ONE.
// Step 3 — the per-vehicle teardown — WRITES a tombstone for every car it
// removes, in the same transaction as the Vehicle delete (§1.4.1). So this
// DELETE must run AFTER step 3, or the teardown simply re-creates every row it
// just removed and the account exits the sequence with a full set of fresh
// tombstones. Step 8e satisfies that; nothing after step 3 writes one.
//
// P0 throughout (data-classification.md §1.3): an opaque cuid, an opaque Tesla
// vehicle id, a redactable VIN and a timestamp. Removing it is hygiene rather
// than an erasure obligation, like 8d and unlike 8c.
//
// Deleting zero rows on a re-run is a clean no-op, and zero is the ordinary
// result — most accounts never removed a car.
const queryDeleteRemovedVehiclesForUser = `
DELETE FROM go_removed_vehicles WHERE user_id = $1`

// queryRevokeRefreshTokensForUser revokes every unrevoked refresh token in the
// deleted user's name. Revoke rather than delete, matching the reuse-detection
// model in migration 0003: the rotation lineage is evidence and stays. reason
// is the opaque P0 enum the column already carries.
//
// The ACCESS token cannot be revoked (it is a stateless ES256 JWT with a ~1h
// TTL) — the user-existence check in auth.JWTAuthenticator is what closes that
// window, which is why the handler invalidates the existence cache after the
// identity rows go.
// #nosec G101 -- column/predicate SQL over a hash-only table, not a credential
// (gosec greps the literal 'refresh_tokens' + 'revoked' and misflags it).
const queryRevokeRefreshTokensForUser = `
UPDATE go_refresh_tokens
SET revoked = TRUE, revoked_at = NOW(), reason = 'account_deleted'
WHERE user_id = $1 AND revoked = FALSE`
