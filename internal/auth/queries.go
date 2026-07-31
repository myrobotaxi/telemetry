package auth

// Every SQL statement the authenticator issues, in one file so the whole
// authorization-relevant statement set can be read at once. Each WHERE clause
// here is an access-control decision, and two of them changed in MYR-369 — see
// the per-statement notes.

// queryUserVehicleIDs fetches the caller's full VEHICLE ACCESS SET — the
// vehicles they own, UNIONed with the vehicles somebody has shared with them
// (MYR-91 viewer merge / MYR-184 sharing).
//
// Before MYR-184 the access set was ownership alone, which is why the `viewer`
// role existed in the mask matrix without a single row that could produce it.
// The second leg is what makes it real: an accepted go_vehicle_shares grant
// (Go-owned, migration 0020) puts the vehicle in the grantee's set, so it flows
// through EVERY consumer of this query at once — the WebSocket subscribed set,
// GET /api/vehicles, and every per-vehicle handler that asks "can this caller
// see this car".
//
// Revoked and pending rows are excluded by the `status = 'accepted'` predicate:
// an invite that was never redeemed grants nothing, and revocation is a
// tombstone flip, so a revoked grant drops out of the set on the next lookup.
// UNION (not UNION ALL) de-duplicates the case where an owner also holds a
// stale grant row for their own car.
//
// SUSPENDED GRANTS ARE EXCLUDED TOO (MYR-369, `suspended_at IS NULL`), and THIS
// IS THE PREDICATE THAT MAKES SUSPENSION MEAN ANYTHING. Suspension is specified
// as gating EVERYTHING — catalog row, REST snapshot, WebSocket subscription,
// drives, rides — and rather than adding five independent checks it is enforced
// once, here, in the one query every one of those surfaces resolves through.
// A suspended viewer's answer to "can this caller see this car" becomes plain
// no, identical to a stranger's, which is exactly the intended semantics: the
// grant row survives so the owner can lift the suspension, and conveys nothing
// while it stands.
//
// Note this is a NARROWING of the access set, so the failure direction if the
// predicate were ever dropped is over-exposure, not breakage — which is why it
// carries a test of its own rather than relying on a surface-level assertion
// noticing.
const queryUserVehicleIDs = `
SELECT "id" FROM "Vehicle" WHERE "userId" = $1
UNION
SELECT vehicle_id FROM go_vehicle_shares
WHERE accepted_by_user_id = $1 AND status = 'accepted' AND suspended_at IS NULL`

// queryUserExists is a slim row-existence probe used by the FR-10.1
// fail-closed JWT existence check (data-lifecycle.md §3.5, MYR-73). A user
// is "live" if a row exists in EITHER the Prisma-owned "User" table (web /
// Google users) OR the Go-owned go_users table (Apple-native users minted by
// the identity module, MYR-193 — they have no legacy Prisma row). Both EXISTS
// sub-probes hit a primary-key index, so this stays a microsecond lookup.
// The query returns one row when the user exists and zero rows otherwise, so
// UserExists's pgx.ErrNoRows handling is unchanged.
const queryUserExists = `SELECT 1 WHERE EXISTS (SELECT 1 FROM "User" WHERE "id" = $1) OR EXISTS (SELECT 1 FROM go_users WHERE "id" = $1)`

// queryVehicleOwnerByID fetches the owning user ID for a vehicle. Used
// by ResolveRole to determine whether the caller is the owner of the
// vehicle or a viewer (post-MYR-Invite, after the FR-5.4 invite path
// lands; today the only path to the viewer branch is a stale cache).
const queryVehicleOwnerByID = `SELECT "userId" FROM "Vehicle" WHERE "id" = $1`
