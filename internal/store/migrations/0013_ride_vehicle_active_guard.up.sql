-- 0013_ride_vehicle_active_guard.up.sql
--
-- MYR-266: one active ride per VEHICLE. A partial UNIQUE index over vehicle_id,
-- scoped to the states in which a car is COMMITTED to a ride, is the
-- AUTHORITATIVE double-booking guard, mirroring the per-RIDER guard (0004,
-- MYR-230). Two concurrent accepts for the same car serialize in Postgres —
-- exactly one guarded requested->accepted UPDATE wins, the loser raises 23505
-- (unique_violation), which the repo maps to ErrVehicleRideActive and the HTTP
-- layer to 409. Same "let the database arbitrate the race" discipline as the
-- guarded UpdateStatusFrom transition (0002 / MYR-174), not a check-then-write.
--
-- Why this guard exists: the completion path historically completed by
-- (vehicle_id + status='enroute'), NOT by ride id, and the ONLY active-ride
-- uniqueness constraint was per-rider (0004) — never per-vehicle. So if a car
-- were ever double-booked (two accepted/enroute rides for one VIN) a single
-- drive-end could complete BOTH. MYR-270 has since removed the drive-end
-- auto-completer (completion is now the owner's per-ride "dropped off" tap), but
-- the underlying invariant — a car serves at most one ride at a time — was never
-- enforced. This index enforces it at the source: a car cannot be committed to a
-- second ride while one is live.
--
-- ACTIVE (committed) states = 'accepted' (leg 1, en route to pickup), 'arrived'
-- (rider aboard, awaiting start), 'enroute' (leg 2, en route to dropoff). These
-- are exactly the post-accept, pre-terminal states from
-- go_ride_requests_status_check (0002). 'requested' is DELIBERATELY EXCLUDED:
-- before the owner accepts, MANY riders may hold pending 'requested' requests to
-- the same car (that is the owner's incoming feed) — the car is not yet
-- committed, so requests must not block one another. Terminal states
-- ('declined','completed','cancelled') fall out of the predicate, freeing the
-- car for its next ride. If a new committed state is added to the lifecycle it
-- must be added here in the same migration.
--
-- Scheduled rides are EXEMPT (scheduled_for IS NULL), mirroring 0004: a future
-- reservation is not an "active" ride and never blocks the car's live instant
-- ride. Only instant requests participate in the uniqueness constraint.
--
-- Classification: no new columns; an index over existing P0/P1 columns. Not
-- exposed on the wire.

-- MYR-266: pre-index dedup, mirroring 0004. A bare CREATE UNIQUE INDEX would
-- abort the deploy if production already held a double-booked car (two active
-- instant rides for one vehicle_id). Prod was checked read-only before writing
-- this migration and holds ZERO such duplicates, but the dedup is retained as a
-- belt-and-suspenders self-heal (and to keep the migration replay-safe on any
-- database seeded with pre-guard debris): transition every OLDER active instant
-- ride for a vehicle to 'cancelled', keeping only each vehicle's MOST RECENT
-- committed instant ride. Deterministic keep-pick: ORDER BY created_at DESC with
-- id DESC as the tiebreaker — the same total order the list endpoints and the
-- 0004 dedup use. 'cancelled' is a legal member of go_ride_requests_status_check
-- (0002); entering 'cancelled' stamps neither accepted_at nor completed_at and
-- touches updated_at, mirroring the store's UpdateStatusFrom timestamp
-- discipline. Migrations run with no event bus, so no ride_status_changed WS
-- frames fire — clients refetch via REST (FR-9.1/FR-9.2) and observe the loser
-- as cancelled.
UPDATE go_ride_requests
SET status     = 'cancelled',
    updated_at = NOW()
WHERE scheduled_for IS NULL
  AND status IN ('accepted', 'enroute', 'arrived')
  AND id NOT IN (
      SELECT DISTINCT ON (vehicle_id) id
      FROM go_ride_requests
      WHERE scheduled_for IS NULL
        AND status IN ('accepted', 'enroute', 'arrived')
      ORDER BY vehicle_id, created_at DESC, id DESC
  );

CREATE UNIQUE INDEX IF NOT EXISTS uq_go_ride_requests_active_instant_vehicle
    ON go_ride_requests (vehicle_id)
    WHERE scheduled_for IS NULL
      AND status IN ('accepted', 'enroute', 'arrived');
