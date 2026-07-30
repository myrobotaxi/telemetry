-- 0022_push_prefs.down.sql
--
-- Reverts MYR-349: drops the Go-owned go_push_prefs table.
--
-- Reverting FAILS OPEN, deliberately and unavoidably: the notifier's gate reads
-- an all-true default whenever a preference cannot be resolved, so a rollback
-- restores the pre-MYR-349 behaviour exactly -- every notification sends,
-- regardless of what anyone had switched off. That is the correct direction for
-- a rollback (a missed ride notification is a rider standing on a sidewalk;
-- an unwanted one is an annoyance), but it is worth naming: rolling this back
-- discards every stored preference AND resumes sending the categories people
-- turned off. Re-applying the up-migration does not restore them -- the rows
-- are gone, and each person's switches return to their all-on defaults until
-- they set them again.
--
-- Schema rollback only; no Prisma-owned data is touched.

DROP TABLE IF EXISTS go_push_prefs;
