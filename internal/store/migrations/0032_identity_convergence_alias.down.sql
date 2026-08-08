-- Reverses 0032. The backfilled values go with the column; the convergences
-- they describe are not undone (they live in go_identity_apple.user_id).
DROP INDEX IF EXISTS idx_go_users_converged_to;
ALTER TABLE go_users DROP COLUMN IF EXISTS converged_to;
