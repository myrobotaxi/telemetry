-- 0023_saved_places.down.sql
--
-- Reverts MYR-321: drops the Go-owned go_saved_places table.
--
-- Reverting FAILS CLOSED, which is the right direction here and the opposite
-- of the 0022 rollback. A missing saved place is not a silent wrong answer --
-- the read path returns an empty list, the client renders "Set home", and the
-- person is told plainly that nothing is stored. Nobody is sent anywhere they
-- did not ask to go and nothing they switched off starts happening again.
--
-- It is still destructive, and worth naming: rolling this back DISCARDS every
-- saved place on the platform. The ciphertext goes with the rows, so re-
-- applying the up-migration does not restore them -- there is no plaintext
-- mate and no shadow column to recover from, which is the cost of the
-- encrypt-only posture and is accepted deliberately. Each person's Home and
-- Work return to unset until they save them again.
--
-- Schema rollback only; no sibling-owned data is touched.

DROP TABLE IF EXISTS go_saved_places;
