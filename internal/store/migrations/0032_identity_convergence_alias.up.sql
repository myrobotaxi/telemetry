-- MYR-452: identity convergence must leave a trail.
--
-- store.OwnerProvisioner.rebindApple re-points go_identity_apple.user_id from
-- the caller's go_users id onto the canonical Prisma "User" id when a Tesla
-- link converges two identities onto one human. Before this migration that
-- re-point SEVERED the only link back: the caller's go_users row was left with
-- no binding and nothing referencing it, while the caller's JWT went on
-- carrying the OLD subject for the life of its refresh family (90 days).
--
-- Account deletion is keyed on that JWT subject, so for a converged owner every
-- step of the teardown targeted an id that owned nothing: the Apple binding
-- (now filed under the canonical id) survived, the "User" row and its cascades
-- survived, and the endpoint still answered 204. The next Sign in with Apple
-- found the surviving binding and signed the person straight back into the
-- account they had just deleted.
--
-- converged_to is that missing link. It makes the pre-convergence id resolvable
-- to the canonical one, which is what lets the deletion scope cover both.
ALTER TABLE go_users ADD COLUMN IF NOT EXISTS converged_to TEXT;

-- Partial: only converged rows carry a value, and the reverse lookup
-- (which aliases point at this canonical id?) is the only read.
CREATE INDEX IF NOT EXISTS idx_go_users_converged_to
    ON go_users (converged_to) WHERE converged_to IS NOT NULL;

-- Backfill the residue of every convergence that happened BEFORE this column
-- existed. Best-effort and deliberately narrow: it only claims a go_users row
-- that (a) holds no binding of its own — the signature of having been re-pointed
-- away — and (b) shares an email with a binding filed under a different id.
--
-- The email equality is safe as an identity join here because the address is one
-- WE verified through Apple (identity.SignInWithApple stores only the verified
-- claim, and a hidden-relay address is per-app stable), and because the
-- no-binding-of-its-own predicate already restricts the candidate set to
-- orphans. A row that matches nothing is simply left alone: it stays exactly as
-- broken as it is today, and no live row can be captured by this statement.
UPDATE go_users g
SET converged_to = i.user_id
FROM go_identity_apple i
WHERE g.converged_to IS NULL
  AND g.email IS NOT NULL
  AND i.email = g.email
  AND i.user_id <> g.id
  AND NOT EXISTS (SELECT 1 FROM go_identity_apple b WHERE b.user_id = g.id);
