// SQL for the go_saved_places repository (MYR-321). See saved_places_repo.go
// for the encryption contract and saved_places_scan.go for the coordinate
// crypto helpers.

package store

// savedPlaceColumns is the projection every read and both writes return, in
// one place so the scanner's field order cannot drift from the statements'.
// kind comes first because the row is self-describing on the wire: a SavedPlace
// handed around alone must never lose which slot it is.
const savedPlaceColumns = `kind, lat_enc, lng_enc, label`

// querySavedPlacesForUser reads both slots for one account.
//
// ORDER BY kind is what puts home before work ('home' < 'work' lexically), and
// it is deliberately a STABLE order rather than an arbitrary one so a client
// diffing two responses sees no spurious movement. Consumers are still told not
// to depend on it (§7.20) — they look a kind up rather than index — but a
// server that shuffled rows between identical reads would be gratuitous.
//
// Returns 0, 1 or 2 rows. There is no filter to apply: a kind that was never
// set, or was deleted, has no row at all, so absence IS the "not set" state.
const querySavedPlacesForUser = `
SELECT ` + savedPlaceColumns + `
FROM go_saved_places
WHERE user_id = $1
ORDER BY kind`

// queryUpsertSavedPlace writes one whole place and returns it as stored.
//
// WHOLE-OBJECT, NOT PARTIAL — note the absence of the COALESCE($n, existing)
// pattern queryUpsertPushPrefs uses. That difference is the design, not an
// omission: §7.19's five switches are independent, so an omitted key must mean
// "leave alone"; §7.20's label and coordinates are ONE fact, so every write
// replaces the whole slot and a client cannot move a pin while keeping a label
// that no longer describes it.
//
// The conflict target is the PRIMARY KEY (user_id, kind), which is why setting
// a place is one statement whether or not a row already existed — and why a
// second 'home' for the same account cannot exist to be shadowed.
//
// created_at is NOT touched on conflict: it records when the person first saved
// this slot, and replacing the address does not make that a new fact.
const queryUpsertSavedPlace = `
INSERT INTO go_saved_places (user_id, kind, lat_enc, lng_enc, label, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
ON CONFLICT (user_id, kind) DO UPDATE
SET lat_enc    = EXCLUDED.lat_enc,
    lng_enc    = EXCLUDED.lng_enc,
    label      = EXCLUDED.label,
    updated_at = NOW()
RETURNING ` + savedPlaceColumns

// queryDeleteSavedPlace forgets one slot.
//
// A real DELETE, not a tombstone — the opposite of go_vehicle_shares, and for a
// clean reason: a revoked share is evidence in someone ELSE's audit trail (the
// car owner's record of who could see their vehicle), whereas a saved place is
// a person's own note to themselves. Nobody is owed a record that this account
// once knew where its owner lived, and keeping the ciphertext after they asked
// for it to be forgotten would be the wrong answer to the only question that
// matters here.
//
// Deleting zero rows is a clean no-op, which is what makes §7.20's DELETE
// idempotent (204 whether or not a row was there).
const queryDeleteSavedPlace = `
DELETE FROM go_saved_places WHERE user_id = $1 AND kind = $2`
