-- 0036_fleet_config_pairing_epoch.down.sql
--
-- Drops the MYR-489 pairing-epoch columns. Losing them reverts the reconciler
-- to the MYR-448 behaviour (back off on synced-not-streaming, never escalate),
-- which is degraded but safe — the rows themselves still schedule correctly.

ALTER TABLE go_fleet_config_attempts
    DROP COLUMN IF EXISTS forced_repush_at,
    DROP COLUMN IF EXISTS signed_command_at;
