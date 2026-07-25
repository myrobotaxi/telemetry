-- 0009_ride_pickup_handshake.up.sql
--
-- MYR-270: owner-driven dispatch v2. The autonomous two-leg ride gains an
-- explicit OWNER-confirmed pickup handshake between the two nav legs, replacing
-- the MYR-265 "rider boards / drive-end auto-completes" model:
--
--   accepted  (leg 1, car en route to pickup — owner accepted)
--     -> arrived   (rider picked up, awaiting rider start — OWNER taps "Picked up")
--     -> enroute   (leg 2, car en route to dropoff — RIDER taps "Start ride")
--     -> completed (dropped off — OWNER taps "Dropped off")
--
-- picked_up_at pins WHEN the owner confirmed the pickup (accepted -> arrived),
-- stamped first-entry-only in the SAME guarded UPDATE that performs the
-- transition (queryRideRequestUpdateStatus / *StatusFrom), mirroring the
-- existing accepted_at / enroute_at / completed_at lifecycle timestamps. It is
-- an audit/operability timestamp only.
--
-- Classification (data-classification.md §1.9): P0 — an opaque timestamp
-- (never a coordinate, address, token, or raw VIN). Not exposed on the wire.

ALTER TABLE go_ride_requests
    ADD COLUMN IF NOT EXISTS picked_up_at TIMESTAMPTZ;
