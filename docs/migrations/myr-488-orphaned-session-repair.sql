-- MYR-488 — one-off repair for the refresh-token families that were orphaned
-- by convergences that ran BEFORE the fix shipped.
--
-- ============================================================
-- THIS FILE IS AN OPERATOR ARTIFACT, NOT A GO MIGRATION.
-- ============================================================
--
-- It is deliberately NOT under internal/store/migrations/. A numbered migration
-- runs automatically on every deploy, against every environment, with nobody
-- watching — and what this statement changes is which human a live session
-- belongs to. That is the one class of change that should be run by a person who
-- has just read the SELECT below and agrees with what it says. Run it by hand,
-- against production, once.
--
-- WHAT WENT WRONG. store.OwnerProvisioner.rebindApple re-pointed a person's
-- Apple binding onto the canonical user and recorded the edge in
-- go_identity_convergence, but left their device's refresh-token families filed
-- under the id it had just abandoned. Those devices go on authenticating as a
-- subject that owns nothing. The server fix (owner_session_migration.go) closes
-- this at convergence time; it cannot reach backwards, because the convergences
-- have already committed.
--
-- WHY THIS IS SAFE, AND IT IS THE SAME ARGUMENT THE LIVE PATH RESTS ON. Every
-- row in go_identity_convergence was written only after rebindApple confirmed an
-- Apple binding actually MOVED between those two ids — same-human evidence, and
-- the RowsAffected guard refuses to write the edge without it. The codebase
-- already treats such an edge as an unconditional grant of DELETE authority over
-- the target (internal/store/account_deletion_scope.go: a Delete tap runs every
-- destructive step across the whole closure). Carrying a session across an edge
-- that already conveys the power to erase the account is strictly weaker than
-- the authority the edge conveys today. If an edge here is wrong, this statement
-- is not the first thing that will go wrong with it.
--
-- WHAT IT DOES AND DOES NOT TOUCH. It mirrors the Go path exactly:
--   * moves families that still hold an unrevoked, unexpired row, WHOLE, so
--     family_id continuity holds and rotation keeps working;
--   * leaves REVOKED families where they are, still revoked — revocation is a
--     decision somebody made, and nothing here ever clears the flag;
--   * leaves EXPIRED families where they are — there is no session to preserve;
--   * follows one hop only. A chain (A->B->C) heals a hop at a time; re-run.
--
-- BEFORE. Read what it would move. Expect ONE row for the known incident:
-- from cd38cb3eba91a3b9b69ff6eee5aedcc29 to cmnccy4tf0000ky04coe4is96.
--
--   SELECT c.from_user_id, c.to_user_id, t.family_id, count(*) AS rows
--   FROM go_identity_convergence c
--   JOIN go_refresh_tokens t ON t.user_id = c.from_user_id
--   WHERE EXISTS (
--       SELECT 1 FROM go_refresh_tokens live
--       WHERE live.family_id = t.family_id
--         AND live.user_id = c.from_user_id
--         AND live.revoked = FALSE
--         AND live.expires_at > NOW()
--   )
--   GROUP BY 1, 2, 3
--   ORDER BY 1, 3;
--
-- If that returns rows for edges you do not recognise, STOP and investigate
-- before running the UPDATE. Narrow it with an explicit
-- `AND c.from_user_id = '...'` rather than widening your trust.
--
-- AFTER. This must return zero rows:
--
--   SELECT t.user_id, count(*)
--   FROM go_refresh_tokens t
--   JOIN go_identity_convergence c ON c.from_user_id = t.user_id
--   WHERE t.revoked = FALSE AND t.rotated_to IS NULL AND t.expires_at > NOW()
--   GROUP BY 1;
--
-- The repair is idempotent: once a family has moved, `t.user_id =
-- c.from_user_id` no longer matches it and a re-run changes nothing.

BEGIN;

UPDATE go_refresh_tokens t
SET user_id = c.to_user_id
FROM go_identity_convergence c
WHERE t.user_id = c.from_user_id
  AND EXISTS (
      SELECT 1
      FROM go_refresh_tokens live
      WHERE live.family_id = t.family_id
        AND live.user_id = c.from_user_id
        AND live.revoked = FALSE
        AND live.expires_at > NOW()
  );

COMMIT;
