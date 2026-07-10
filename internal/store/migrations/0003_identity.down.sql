-- 0003_identity.down.sql
--
-- Reverts MYR-193: drops the Go-owned identity tables (indexes drop with
-- them). Order is irrelevant — there are no foreign keys (CG-DL-9).

DROP TABLE IF EXISTS go_refresh_tokens;
DROP TABLE IF EXISTS go_identity_apple;
DROP TABLE IF EXISTS go_users;
