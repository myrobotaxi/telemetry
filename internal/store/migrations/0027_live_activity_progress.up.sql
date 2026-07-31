-- 0027_live_activity_progress.up.sql
--
-- MYR-398: the four columns that let a Live Activity draw a PROGRESS TRACK
-- without lying on it.
--
-- The redesigned lock-screen card shows the car moving along a track toward the
-- pickup (leg 1) and then toward the dropoff (leg 2). What the server pushes for
-- that is a single scalar — `progress`, a 0..1 fraction of the CURRENT leg —
-- and a fraction is meaningless without something to be a fraction OF. The car
-- reports how far it still has to go (Tesla's tripDistanceRemaining, and
-- minutesToArrival behind it); it does not report how far it had to go when the
-- leg began. That number is ours to remember, and these columns are where.
--
-- WHY THIS IS IN THE DATABASE AND NOT IN A MAP IN THE PROCESS.
--
-- Two reasons, either of which is sufficient.
--
-- The ETA ticker is NOT sharded across replicas: every process lists every live
-- Activity on every pass and pushes to all of them. Two replicas holding two
-- in-memory baselines for one ride would compute two different fractions for
-- the same car and the phone would watch the arrow jump between them every 75
-- seconds. A wrong progress is worse than no progress — that is the whole
-- premise of the field — and an arrow that oscillates is wrong twice a minute.
--
-- A deploy in the middle of a ride would forget the baseline. Re-capturing it
-- from the car's CURRENT remaining distance would restart the track at zero
-- with the car three minutes from the kerb, which is the single most alarming
-- thing this card could do to somebody waiting on a sidewalk.
--
-- WHY ON go_live_activities AND NOT ON go_ride_requests.
--
-- `progress_value` is not a fact about the ride; it is the last fraction this
-- PHONE was actually shown. The monotonicity rule the sender enforces — the
-- track never runs backwards — is a promise made to a client, so its state
-- belongs with the client's registration, and a second Activity on the same
-- ride (the owner-side variant MYR-172 left as a follow-up) is entitled to its
-- own, self-consistent history rather than to a fraction some other device's
-- send path advanced. It also keeps the ride row free of rendering state and
-- costs the ticker no join: 0025's read already selects from this table.
--
-- ALL FOUR ARE NULLABLE, AND NULL IS THE ORDINARY STATE.
--
-- No anchor means no progress key on the wire, which renders a card with a
-- headline and no track. Every degraded path in the sender resolves to that:
-- a car with no active nav route, a car whose telemetry has gone quiet, an
-- Activity registered before dispatch. A DEFAULT of 0 would have been a lie
-- with a number on it — "the car has covered none of the distance" instead of
-- "we do not know where the car is" — so there is no default and no backfill.
-- Existing rows stay NULL and acquire an anchor on their next push, if their
-- leg is still running.
--
-- A TOKEN ROTATION MUST NOT RESET THE TRACK. 0025's upsert names its SET list
-- explicitly (activity_push_token, sandbox, updated_at, ended_at), so these
-- four survive a re-registration untouched — which is correct: ActivityKit
-- rotating the token mid-ride is the same Activity on the same leg, and the
-- rider would see the arrow snap back to the start of the journey for no reason
-- they could ever discover.

ALTER TABLE go_live_activities
    -- Which leg the anchor below describes: 'pickup' is the car driving to the
    -- rider (ride status `accepted`), 'dropoff' is the car driving the rider to
    -- their destination (`enroute`). Stored rather than derived from the ride's
    -- status at read time because it is what makes the leg FLIP detectable: the
    -- sender compares the leg it is pushing against the leg the anchor was cut
    -- for, and a mismatch discards the anchor instead of measuring leg two
    -- against leg one's distance. `arrived` and the terminal statuses hold no
    -- anchor at all — their progress is asserted by the ride record, not
    -- estimated — so they clear these columns back to NULL.
    ADD COLUMN IF NOT EXISTS progress_leg TEXT
        CHECK (progress_leg IN ('pickup', 'dropoff')),

    -- Which quantity `progress_baseline` is measured in: 'nav_distance' is
    -- Tesla's tripDistanceRemaining (miles), 'eta' is minutesToArrival
    -- (minutes). Recorded because the two are not interchangeable and the
    -- sender falls back from the first to the second mid-leg when a car stops
    -- reporting distance: a baseline of 8 that silently changes from miles to
    -- minutes would produce a fraction with no meaning at all. A source change
    -- re-anchors, exactly as a route change does.
    ADD COLUMN IF NOT EXISTS progress_source TEXT
        CHECK (progress_source IN ('nav_distance', 'eta')),

    -- The leg's total, in the unit named by progress_source: the value that
    -- corresponds to progress 0. Not necessarily the reading at the instant the
    -- leg began — it is the reading the FIRST TRUSTWORTHY OBSERVATION of the leg
    -- saw, and it is re-anchored whenever the route gets longer than it has ever
    -- been (a reroute, or the car's nav destination changing under us). Both are
    -- honesty bounds the contract states in as many words: the fraction is of
    -- the journey as the car's own navigation currently understands it.
    ADD COLUMN IF NOT EXISTS progress_baseline DOUBLE PRECISION,

    -- The last fraction actually DELIVERED to this Activity — written only
    -- after APNs accepted the push, never before, so it is a record of what the
    -- phone has seen rather than of what we intended it to see. It is the floor
    -- the next push clamps to, which is how "the track never runs backwards"
    -- becomes a property of the sequence the CLIENT observes rather than an
    -- aspiration about the car.
    ADD COLUMN IF NOT EXISTS progress_value DOUBLE PRECISION
        CHECK (progress_value >= 0 AND progress_value <= 1);
