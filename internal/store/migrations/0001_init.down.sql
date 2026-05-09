-- 0001_init.down.sql
--
-- Rolls back the placeholder bootstrap migration.
-- Drops the _telemetry_server_meta table created in 0001_init.up.sql.

DROP TABLE IF EXISTS _telemetry_server_meta;
