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
// A THIRD LEG LANDED IN MYR-540: the vehicle of a LIVE GROUP RIDE the caller
// has JOINED. A member is riding in the car, so they see it exactly as the
// requester does — and, like the requester, only for the ride's lifetime.
//
// It is the narrowest of the three legs by construction and each narrowing is
// deliberate:
//
//   - RIDE-SCOPED, not vehicle-scoped. The membership row is the whole grant;
//     there is no standing entry on the car, so there is nothing to revoke
//     afterwards and no residue for an owner to discover months later.
//   - LIVE ONLY. The terminal statuses are excluded here, in the statement, so
//     access ENDS when the ride does with no sweep, no expiry job and no
//     revocation call. This is the same shape as `suspended_at IS NULL` above:
//     an access-control predicate placed in the one query every surface
//     resolves through, rather than five independent checks.
//   - VIEWER TIER. Membership only puts the vehicle IN the set. What the member
//     may then see is decided by ResolveVehicleAccess, which resolves them to
//     RoleViewer with the zero-value (most restrictive) capability set — so a
//     member cannot request rides in the car on the strength of riding in it.
//
// Note the failure direction if this leg were ever dropped: a member's app goes
// dark mid-ride. If it were ever WIDENED (say, by losing the status filter), a
// stranger keeps live GPS on somebody's car forever. The status predicate is the
// one that carries the risk, and it has a test of its own.
const queryUserVehicleIDs = `
SELECT "id" FROM "Vehicle" WHERE "userId" = $1
UNION
SELECT vehicle_id FROM go_vehicle_shares
WHERE accepted_by_user_id = $1 AND status = 'accepted' AND suspended_at IS NULL
UNION
SELECT r.vehicle_id FROM go_ride_members m
JOIN go_ride_requests r ON r.id = m.ride_id
WHERE m.user_id = $1 AND r.status NOT IN ('completed', 'declined', 'cancelled')`

// queryRideMembershipOnVehicle answers the per-vehicle form of the same
// question: does this caller hold a LIVE group-ride membership on a ride being
// served by this car? It is the role-resolution counterpart to the third UNION
// leg above, and the two carry the SAME status predicate — a set that admits a
// vehicle while the role resolution denies it would produce a client that can
// subscribe and then receives a deny-all projection, which is the worst of both
// answers.
//
// READ-ONLY, both relations Go-owned. Served by idx_go_ride_members_user.
const queryRideMembershipOnVehicle = `
SELECT EXISTS (
	SELECT 1 FROM go_ride_members m
	JOIN go_ride_requests r ON r.id = m.ride_id
	WHERE m.user_id = $1 AND r.vehicle_id = $2
	  AND r.status NOT IN ('completed', 'declined', 'cancelled')
)`

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
