-- 0002_ride_requests.down.sql
--
-- Reverts MYR-173: drops the Go-owned go_ride_requests table (indexes and
-- CHECK constraints drop with it).

DROP TABLE IF EXISTS go_ride_requests;
