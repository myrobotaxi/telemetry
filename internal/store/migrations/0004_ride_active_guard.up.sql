-- 0004_ride_active_guard.up.sql
--
-- MYR-230: one active INSTANT ride per rider. A partial UNIQUE index over
-- rider_id, scoped to the OPEN lifecycle states of an on-demand ("Now")
-- request, is the AUTHORITATIVE race guard: two concurrent creates for the
-- same rider serialize in Postgres — exactly one INSERT wins, the loser
-- raises 23505 (unique_violation), which the repo maps to
-- ErrRideRequestActive and the HTTP layer to 409 `ride_active`. This is the
-- same "let the database arbitrate the race" discipline as the guarded
-- UpdateStatusFrom transition (0002 / MYR-174), not a check-then-write.
--
-- OPEN states = everything that is NOT terminal: 'requested' (the wire
-- 'pending'), 'accepted', 'enroute', 'arrived'. Terminal states
-- ('declined', 'completed', 'cancelled') fall out of the predicate, so a
-- rider whose previous ride reached a terminal state can freely request
-- again. The list is derived from go_ride_requests_status_check (0002); if
-- a new open state is added to the lifecycle it must be added here in the
-- same migration.
--
-- Scheduled rides are EXEMPT (scheduled_for IS NOT NULL): a future
-- reservation is not an "active" ride and never blocks — a rider may hold
-- many scheduled rides plus one open instant ride. Only instant requests
-- (scheduled_for IS NULL) participate in the uniqueness constraint.

-- MYR-230: pre-index dedup — the guard postdates the create endpoint, so
-- production already holds riders with several concurrently-open instant
-- rides (stale test debris from pre-guard client QA). CREATE UNIQUE INDEX
-- would abort on those rows and fail the deploy, so first transition every
-- OLDER open instant ride to 'cancelled', keeping only each rider's MOST
-- RECENT open instant ride — keeping newest matches rider expectation (the
-- ride they asked for last is the one they are waiting on). Deterministic
-- keep-pick: ORDER BY created_at DESC with id DESC as the tiebreaker, the
-- same total order the list endpoints use. 'cancelled' is a legal member of
-- go_ride_requests_status_check (0002), and the write mirrors the store's
-- UpdateStatusFrom timestamp discipline: entering 'cancelled' stamps
-- neither accepted_at nor completed_at, and the transition touches
-- updated_at. Migrations run with no event bus, so no ride_status_changed
-- WS frames fire for these rows — clients refetch state via REST
-- (FR-9.1/FR-9.2 reconciliation) and observe the stale rides as cancelled.
UPDATE go_ride_requests
SET status     = 'cancelled',
    updated_at = NOW()
WHERE scheduled_for IS NULL
  AND status IN ('requested', 'accepted', 'enroute', 'arrived')
  AND id NOT IN (
      SELECT DISTINCT ON (rider_id) id
      FROM go_ride_requests
      WHERE scheduled_for IS NULL
        AND status IN ('requested', 'accepted', 'enroute', 'arrived')
      ORDER BY rider_id, created_at DESC, id DESC
  );

CREATE UNIQUE INDEX IF NOT EXISTS uq_go_ride_requests_active_instant_rider
    ON go_ride_requests (rider_id)
    WHERE scheduled_for IS NULL
      AND status IN ('requested', 'accepted', 'enroute', 'arrived');
