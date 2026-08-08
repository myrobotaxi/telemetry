-- 0031_fleet_config_attempts.down.sql
--
-- Drop the fleet-config reconciler's scheduling state (MYR-448).
--
-- The table holds only scheduling bookkeeping — when we last tried a vehicle
-- and when to try it next. Dropping it loses the backoff history, so the next
-- reconciler pass treats every candidate as never-attempted and retries them
-- all at the base interval. That is noisier against Tesla but not incorrect,
-- and it self-heals as soon as attempts start being recorded again.
--
-- Nothing else reads this table, and no Prisma-owned column depends on it.

DROP INDEX IF EXISTS idx_go_fleet_config_attempts_due;
DROP TABLE IF EXISTS go_fleet_config_attempts;
