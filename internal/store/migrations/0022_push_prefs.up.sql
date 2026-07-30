-- 0022_push_prefs.up.sql
--
-- MYR-349: go_push_prefs -- the per-person notification preferences the app's
-- Settings toggles have always pretended to write.
--
-- The gap it closes is a LIE, not a missing feature. Both Settings screens have
-- shipped a Notifications card since MYR-170 whose switches were backed by a
-- private SwiftUI `@State` struct: they moved, they animated, they persisted
-- nowhere and they gated nothing. An owner who turned "Charging complete" off
-- kept receiving it; a rider who turned "Pick-up & arrival alerts" off kept
-- being woken. MYR-186 then built the delivery pipeline (go_push_devices, the
-- APNs sender, the ride-lifecycle notifier) with no preference layer at all, so
-- the switches were still decorative on the day push became real. This table is
-- the missing storage, and internal/push consults it before every send.
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case
-- columns, and NO foreign key to any table owned by the sibling app's ORM --
-- user_id holds a cuid identifying a row in that schema and is a plain column
-- here. Deleting a person therefore leaves a row behind. It is inert: nothing
-- reads a prefs row except a fan-out that first resolved that person into a
-- ride party, and a deleted person is never a ride party again. Unlike
-- go_push_devices there is no self-healing signal to lean on (APNs has no
-- verdict about a PREFERENCE), and none is needed -- an orphan row is five
-- booleans nobody asks about.
--
-- PRIMARY KEY (user_id) -- the natural key, and there is no surrogate id.
-- Every other Go-owned table in this schema carries a cuid `id` because its
-- rows are things that can exist more than once per person (a device, a share,
-- a ride). A person has exactly ONE preference set, so a surrogate key would
-- add a column that must then be constrained UNIQUE on user_id anyway, and
-- would let a bug write a second row that silently shadows the first. The
-- upsert conflict target is therefore the primary key itself.
--
-- FIVE CATEGORIES, one per notification the product can send:
--
--   ride_lifecycle    -- the whole ride status class: a new request reaching an
--                        owner, and accepted / declined / arrived / enroute /
--                        completed / expired reaching a rider. ONE column on
--                        purpose: every ride-lifecycle send site shares one
--                        recipient-facing concern ("tell me about this ride"),
--                        and splitting it would mint columns no send site
--                        currently distinguishes.
--   drive_started     -- the owner's car began a drive
--   drive_completed   -- the owner's car finished a drive
--   charging_complete -- the owner's car finished charging
--   viewer_joined     -- somebody redeemed an invite to the owner's car
--
-- Owner-only concepts and rider concepts SHARE ONE ROW. A person is not an
-- owner or a rider -- MYR-343 established that an account can be both at once,
-- and MYR-224's mode switch lets one person be either within a session. Keying
-- preferences by (user, role) would mean the same human has two answers to
-- "should this phone ring", and the phone is the same phone. The NOTIFIER
-- decides which column applies to a given send, because only the notifier knows
-- which side of a ride the recipient is standing on.
--
-- ONLY FOUR OF THE FIVE HAVE A SENDER TODAY -- in fact only one does.
-- internal/push has exactly three fan-out sites, all ride_lifecycle. There is
-- no drive-started, drive-completed, charging-complete or viewer-joined
-- notifier in this service at all; those four columns store the answer to a
-- question nothing asks yet. They are created NOW anyway, because the switches
-- for all four are already on the owner's screen and have been since MYR-170:
-- storing the owner's answer is what stops the toggle being a lie a second
-- time, and it means the notifier that eventually sends them is born gated
-- rather than retrofitted.
--
-- NOT NULL DEFAULT TRUE on every column -- the same reasoning migration 0021
-- gives for ride_share_enabled, and for the same reason it is written out
-- rather than assumed. There is no honest-unknown state here: a person who has
-- never opened Settings IS receiving notifications, which is exactly what TRUE
-- means, and a nullable column would invent a third state every reader would
-- have to collapse back to TRUE anyway. The DEFAULT is load-bearing TWICE, and
-- the second place is the one that matters: the row may not exist AT ALL for
-- anybody who has never touched a switch -- which is EVERYONE on the day this
-- ships -- so the read path returns the all-true default for a missing row and
-- the upsert COALESCEs an omitted key to TRUE. "No row", "row with the column
-- defaulted", and "explicitly enabled" must be indistinguishable to the
-- notifier, or this migration would silence every notification in production
-- the moment it landed.
--
-- THE WRITE IS PARTIAL. PUT /api/users/me/push-prefs (rest-api.md 7.19) carries
-- only the categories the client is changing, so the upsert assigns
-- COALESCE($n, existing) per column -- an omitted key means "leave alone",
-- never "set to false". A whole-object PUT would make two phones racing on two
-- different switches clobber each other, which for a preference surface is the
-- difference between a stale toggle and a notification the owner explicitly
-- turned off arriving anyway.
--
-- Classification: P0 throughout. user_id is an opaque cuid; the five booleans
-- are a person's own delivery preferences -- not identifying, no location, no
-- tokens, no PII, and nothing about a ride or a car. They are the least
-- sensitive rows in the push surface: go_push_devices.device_token is P1
-- because it is a capability, and this table holds no capability at all. See
-- data-classification.md section 1.16.

CREATE TABLE IF NOT EXISTS go_push_prefs (
    user_id           TEXT        PRIMARY KEY,
    ride_lifecycle    BOOLEAN     NOT NULL DEFAULT TRUE,
    drive_started     BOOLEAN     NOT NULL DEFAULT TRUE,
    drive_completed   BOOLEAN     NOT NULL DEFAULT TRUE,
    charging_complete BOOLEAN     NOT NULL DEFAULT TRUE,
    viewer_joined     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- No secondary index. Every access -- the endpoint read, the endpoint upsert,
-- and the notifier's per-send gate -- is a point lookup on user_id, which the
-- primary key already serves.
