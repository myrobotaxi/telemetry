-- 0019_push_devices.down.sql
--
-- Reverts MYR-186: drops the Go-owned go_push_devices registry (the user_id
-- index and the device_token unique index drop with it). Reverting silently
-- turns every ride-lifecycle push into a no-op — the senders resolve an empty
-- audience and log a skip — and it discards every registered token, so each
-- installed app must re-register on its next launch before it can be reached
-- again. Schema rollback only; no Prisma-owned data is touched.

DROP TABLE IF EXISTS go_push_devices;
