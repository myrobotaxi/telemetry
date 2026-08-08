-- MYR-452: identity convergence must leave a trail.
--
-- store.OwnerProvisioner.rebindApple re-points go_identity_apple.user_id from
-- the caller's id onto the canonical Prisma "User" id when a Tesla link proves
-- two identities are the same human. Before this table that re-point SEVERED
-- the only link back: nothing referenced the caller's original id, while the
-- caller's JWT went on carrying it for the life of its refresh family.
--
-- Account deletion is keyed on that JWT subject, so for a converged owner every
-- step of the teardown targeted an id that owned nothing: the Apple binding
-- (now filed under the canonical id) survived, the "User" row and its cascades
-- survived, and the endpoint still answered 204. The next Sign in with Apple
-- found the surviving binding and signed the person straight back into the
-- account they had just deleted.
--
-- WHY A TABLE AND NOT A COLUMN ON go_users. The re-pointed id is whatever the
-- caller's JWT names, and that is NOT always a go_users id: identity.linkage
-- mints an Apple binding directly onto a Prisma "User" id on a verified-email
-- match, and onto a configured cuid on a bootstrap override, creating no
-- go_users row in either case. A go_users column would silently record nothing
-- for exactly those callers and leave them resurrectable — the same bug in a
-- different shape. This table is keyed on neither identity source.
CREATE TABLE IF NOT EXISTS go_identity_convergence (
    -- The abandoned id: what the caller's token still says.
    from_user_id TEXT PRIMARY KEY,
    -- The id the Apple binding was moved to.
    to_user_id   TEXT NOT NULL,
    converged_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- A self-edge would make the closure walk pointless and is always a bug.
    CONSTRAINT go_identity_convergence_no_self CHECK (from_user_id <> to_user_id)
);

-- The reverse lookup: which abandoned ids point at this canonical one. Used by
-- the deletion-scope walk, which traverses the graph in both directions.
CREATE INDEX IF NOT EXISTS idx_go_identity_convergence_to
    ON go_identity_convergence (to_user_id);

-- ONE-OFF REPAIR of the single convergence that predates this table, by exact
-- id (MYR-452).
--
-- There is deliberately NO heuristic backfill here. An earlier draft matched
-- orphaned rows to bindings on email equality; that was withdrawn because a row
-- in this table is not a hint — it is an unconditional grant of DELETE
-- authority over another user id, since the deletion scope now runs every
-- destructive step over the whole closure. An `UPDATE … FROM` with a
-- non-unique email join picks its partner arbitrarily and reports success, so
-- any duplicate or stale address could have pointed one person's Delete at
-- another person's account. Guessing is not an acceptable input to that.
--
-- This pair instead comes from direct observation of production: go_users
-- cd38cb3e… was created 134 ms before the binding for the same Apple-verified
-- address appeared under Prisma user cmnccy4tf…, which is the exact signature
-- of rebindApple firing. Both EXISTS guards make the statement inert in every
-- other environment (and in prod too, once that account is deleted), and the
-- ids are 32-hex/cuid values that cannot collide by accident.
-- The third guard is the important one: it re-checks, in SQL, the observation
-- the pairing rests on — that the abandoned row and the surviving binding carry
-- the SAME Apple-verified address. It is a guard, not a selector: it cannot
-- choose a partner, only refuse the hard-coded one. If the two rows have
-- drifted apart by deploy time, the statement does nothing and the account
-- stays as it is rather than being handed a delete grant over a stranger.
INSERT INTO go_identity_convergence (from_user_id, to_user_id)
SELECT 'cd38cb3eba91a3b9b69ff6eee5aedcc29', 'cmnccy4tf0000ky04coe4is96'
WHERE EXISTS (
    SELECT 1
    FROM go_users g
    JOIN go_identity_apple i ON lower(i.email) = lower(g.email)
    WHERE g.id = 'cd38cb3eba91a3b9b69ff6eee5aedcc29'
      AND i.user_id = 'cmnccy4tf0000ky04coe4is96'
      AND g.email IS NOT NULL
)
-- And the abandoned id must still hold no binding of its own; if it does, it
-- was never converged away and needs no edge.
AND NOT EXISTS (
    SELECT 1 FROM go_identity_apple
    WHERE user_id = 'cd38cb3eba91a3b9b69ff6eee5aedcc29'
)
ON CONFLICT (from_user_id) DO NOTHING;
