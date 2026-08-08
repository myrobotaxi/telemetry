-- Reverses 0033.
--
-- NOTE: this discards every convergence recorded at runtime since the table was
-- created, and those cannot be reconstructed — the re-point they describe left
-- no other trace, which is the whole reason the table exists. A down/up cycle
-- therefore returns the affected accounts to the MYR-452 failure mode. Dump the
-- table before running this if any rows are present.
DROP INDEX IF EXISTS idx_go_identity_convergence_to;
DROP TABLE IF EXISTS go_identity_convergence;
