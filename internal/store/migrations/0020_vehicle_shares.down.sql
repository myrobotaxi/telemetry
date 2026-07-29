-- 0020_vehicle_shares.down.sql
--
-- Reverts MYR-184: drops the Go-owned go_vehicle_shares grant table (its two
-- partial indexes and the partial-unique accepted-grant index drop with it).
--
-- Reverting REVOKES EVERY SHARE. The viewer access set collapses back to
-- "vehicles you own": shared cars vanish from their viewers' lists, viewer
-- WebSocket subscriptions stop resolving, and every outstanding invite code
-- becomes unredeemable. It also discards the revoked-row tombstones the table
-- keeps for audit. Schema rollback only; no sibling-owned data is touched.

DROP TABLE IF EXISTS go_vehicle_shares;
