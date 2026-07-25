-- 0009_ride_pickup_handshake.down.sql — reverse of the up migration (MYR-270).
ALTER TABLE go_ride_requests
    DROP COLUMN IF EXISTS picked_up_at;
