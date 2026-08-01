-- 0029_live_activity_alerted_phase.up.sql
--
-- MYR-398: the one column that keeps the Dynamic Island from strobing.
--
-- The v3 card asks iOS to EXPAND the island for about three seconds on each of
-- six phase changes — Dispatch, Enroute, Arriving, Arrived, On trip, Completed
-- — which is done by attaching an `aps.alert` dictionary to that phase's
-- Activity update. The design forbids the expansion everywhere else in as many
-- words: not on ETA ticks, not on the stale flip. A compact island that swaps
-- one figure for another is a state change nobody sees; an island that opens
-- every seventy-five seconds for the length of a ride is a widget the rider
-- turns off.
--
-- So the server has to know which phases a given Activity has ALREADY been
-- expanded for, and this column is that memory: the highest ladder position
-- alerted so far, compared with `>` before every alert is attached.
--
-- WHY A LADDER ORDINAL AND NOT A BOOLEAN PER PHASE, OR THE LAST PHASE SENT.
--
-- A ride passes through the six in one order and never goes back, so a single
-- high-water mark expresses "each at most once" in one comparison. It also
-- makes the guarantee hold under things the ladder was not built for: a
-- duplicated or out-of-order `ride.status.changed` replaying `accepted` after
-- `arrived` compares 2 > 4 and alerts nothing, where an equality test against
-- the last phase sent would happily alert again. Six booleans would say the
-- same thing in six columns and let them disagree.
--
-- WHY IT IS IN THE DATABASE. The same two reasons as migration 0027's progress
-- anchor, and they are stronger here. The ETA ticker is NOT sharded — every
-- replica lists every live Activity every pass — so an in-memory mark would let
-- each replica open the island once for the same phase. And Arriving is a
-- THRESHOLD on the car's ETA rather than a transition: it is true on every
-- remaining pass of the pickup leg once it is true at all, so a mark forgotten
-- by a deploy mid-pickup means an expansion every 75 seconds until the car
-- reaches the kerb.
--
-- WHY ON go_live_activities. It records what one PHONE has been shown, exactly
-- like progress_value beside it — not a fact about the ride. A second Activity
-- on the same ride (the owner-side variant MYR-172 left as a follow-up) is
-- entitled to its own expansions rather than to have them consumed by somebody
-- else's send path.
--
-- A TOKEN ROTATION MUST NOT RESET IT. 0025's upsert names its SET list
-- explicitly, so this column survives a re-registration untouched — which is
-- right: ActivityKit rotating the token mid-ride is the same Activity, and
-- re-running the whole ladder would open the island once for every phase the
-- ride has already passed through.

ALTER TABLE go_live_activities
    -- The highest phase this Activity's island has been expanded for.
    -- 0 = none yet, 1 = Dispatch (`requested`), 2 = Enroute (`accepted`),
    -- 3 = Arriving (pickup ETA at or under two minutes — the one phase that is
    -- not a ride status), 4 = Arrived, 5 = On trip (`enroute`),
    -- 6 = Completed. The NUMBERS are the contract, not the names: the
    -- once-per-phase rule is a `>` between two of them, so a value may never be
    -- renumbered and a new phase may only be inserted where the ride's real
    -- order stays ascending. The unhappy endings — declined, cancelled,
    -- reservation_expired — are outside the six and never write here.
    --
    -- NOT NULL with a default of 0 rather than nullable: unlike the progress
    -- anchor, where NULL is the honest "we do not know where the car is", there
    -- is no unknown state here. An Activity has either been alerted at some
    -- phase or it has not, and 0 says the second exactly.
    ADD COLUMN IF NOT EXISTS alerted_phase SMALLINT NOT NULL DEFAULT 0
        CHECK (alerted_phase >= 0 AND alerted_phase <= 6);

-- BACKFILL, so the deploy itself does not open every live island.
--
-- Without this, every Activity in flight when this migration lands carries the
-- default 0, and its next push — within 75 seconds — computes a phase above it
-- and alerts. Every rider currently mid-ride would get one unexplained
-- expansion at deploy time. Seeding the mark from the ride's CURRENT status
-- says the true thing instead: that phase has already happened and its
-- expansion is not owed, because the card was drawn for it long ago.
--
-- Deliberately seeds from the STATUS ALONE and so cannot express Arriving,
-- which needs the car's ETA. An Activity that is mid-pickup and already inside
-- the two-minute threshold when this lands will therefore get its Arriving
-- expansion on the next tick — which is correct: that expansion has genuinely
-- not happened yet.
UPDATE go_live_activities a
SET alerted_phase = CASE r.status
        WHEN 'requested' THEN 1
        WHEN 'accepted'  THEN 2
        WHEN 'arrived'   THEN 4
        WHEN 'enroute'   THEN 5
        WHEN 'completed' THEN 6
        ELSE 0
    END
FROM go_ride_requests r
WHERE r.id = a.ride_request_id
  AND a.ended_at IS NULL;
