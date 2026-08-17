-- 0042_profile_name_confirmations.up.sql
--
-- MYR-583: go_profile_name_confirmations — the server-side record that a person
-- has CONFIRMED the name the platform shows other people.
--
-- THE CLIENT RULING THIS TABLE IMPLEMENTS (2026-08-17, verbatim): "Any names not
-- entered/confirmed should be in Setting Up. So please ensure even cars with
-- names but not confirmed show that label." A name only counts once its owner
-- has confirmed it through the MYR-581 prompt.
--
-- WHY A NAME CAN EXIST WITHOUT ANYBODY HAVING CONFIRMED IT — this is the whole
-- reason the table is needed, and it is not hypothetical. Two of the three rungs
-- of the display-name ladder are filled by machinery the person never saw:
--
--   * `go_identity_apple.name` is captured from Apple's FIRST-consent payload.
--     Apple sends it once, the client forwards it, and nobody is ever shown it
--     for approval. It can be a legal name the person would not choose as a
--     display name, and it can be stale by years.
--   * Prisma `"User".name` was written by the legacy web onboarding, which
--     seeded a literal placeholder ('Tesla User') for accounts created in the
--     2026-03 web era.
--
-- Before this table the ladder could not tell those apart from a name somebody
-- deliberately typed. A LADDER-RESOLVABLE NAME IS NOT EVIDENCE OF CONSENT, and
-- the two surfaces that show a name to a COUNTERPARTY — the vehicle catalog's
-- `ownerFirstName` and the share listing's `acceptedByName` — plus the
-- offerability gate that refuses a nameless car, all now read this table first.
--
-- WHY A SEPARATE TABLE RATHER THAN A COLUMN ON A NAME ROW. The confirmation is a
-- fact about the PERSON, not about any one identity rung, and the name itself
-- lives on up to three rows across two identity sources (see
-- internal/store/user_profile_name.go). A `name_confirmed` boolean would have to
-- be written on every rung and read through the same COALESCE — a fourth ladder,
-- with a fourth chance to disagree. One row per person, keyed by the id the
-- writer is already scoped to, cannot disagree with itself.
--
-- Naming convention (CG-DL-9): Go-owned table, `go_` prefix, snake_case, and NO
-- foreign key to any table owned by the sibling app's ORM — `user_id` holds a
-- cuid identifying a row in that schema (or in `go_users`, for an Apple-native
-- account) and is a plain column here. No sibling-owned relation is named
-- anywhere in this file, which is also why the MYR-583 BACKFILL is not here: it
-- has to read the sibling `name` column, so it ships as the operator binary
-- cmd/backfill-name-confirmations instead. See docs/contracts/data-lifecycle.md
-- §1.4.2.
--
-- user_id IS THE PRIMARY KEY, not a surrogate id with a UNIQUE on top: a person
-- either has confirmed their name or has not, so a second row for the same
-- person is not a second fact, it is a bug. The same reasoning migration 0022
-- gives for its single-column key.
--
-- ROW PRESENCE IS THE WHOLE SIGNAL. There is no `confirmed BOOLEAN`, because
-- `confirmed = FALSE` would be a state nothing writes and nothing renders — the
-- absent row already means "not confirmed", the state every account starts in.
-- Consequently NO ROW IS EVER DELETED by this feature: confirmation is
-- monotonic, exactly as the MYR-581 name write is (it refuses the empty string
-- on every rung, so no path can un-name an account). The single deletion is
-- ACCOUNT DELETION, step 8b of the rest-api.md §7.6 sequence, which takes the
-- row with the person.
--
-- confirmed_at IS FOR OPERATORS, NOT FOR THE GATE. Every reader asks only
-- whether the row EXISTS; nothing compares this timestamp to anything, and no
-- window expires a confirmation. It is here so a support question ("when did
-- James confirm?") is answerable, and so the backfill's provenance is legible —
-- backfilled rows carry the `"User"."updatedAt"` they were inferred from rather
-- than the moment the backfill ran.
--
-- CLASSIFICATION: P0 in full (docs/contracts/data-classification.md §1.18). An
-- opaque cuid and a timestamp — the same tier as go_removed_vehicles. The NAME
-- is P1 and is nowhere in this table; that is the point of splitting the fact
-- from the value, and it is what lets the offerability gate read confirmation
-- without the P1 name entering the enforcement path at all.

CREATE TABLE IF NOT EXISTS go_profile_name_confirmations (
    -- The account that confirmed. Opaque cuid; no FK (CG-DL-9). Keyed exactly
    -- as the MYR-581 name write is keyed — the caller's authenticated subject —
    -- so the confirmation and the name it confirms can never be filed under
    -- different ids.
    user_id      TEXT        NOT NULL,

    -- When the confirmation happened. NOT NULL: a row with no timestamp would be
    -- a confirmation nobody can date, and the presence of the row is the signal
    -- either way, so there is nothing a NULL could usefully mean.
    confirmed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT go_profile_name_confirmations_pkey PRIMARY KEY (user_id)
);

-- No secondary index. Every access — the upsert on PATCH /api/users/me, the
-- EXISTS probe on the catalog/share/gate reads, and the account-deletion sweep —
-- is a point lookup on user_id, which the primary key's own index serves.
