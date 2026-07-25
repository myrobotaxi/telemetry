-- 0008_vehicle_control_state.down.sql
--
-- MYR-269 rollback: drop the Go-owned owner-control side table. Reverting leaves
-- the four owner controls (Lock/Trunk/Climate/Charge port) live-WebSocket-only
-- again (unavailable on a non-streaming snapshot) but loses no Prisma-owned data.

DROP TABLE IF EXISTS go_vehicle_control_state;
