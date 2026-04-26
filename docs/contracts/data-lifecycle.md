# Data Lifecycle Contract

**Status:** Draft — v1
**Target artifact:** Lifecycle policy doc + AuditLog schema + pruning job spec
**Owner:** `sdk-architect` agent
**Last updated:** 2026-04-25

## Purpose

Defines — for every persisted field — its **single source of truth** (DB or WebSocket-only), its **retention window**, its **deletion semantics**, and the **audit log entry** written on mutation. Enforces the "raw telemetry is never persisted as a historical log" principle (`requirements.md` design principle 5) and the "single source of truth" principle (`requirements.md` design principle 8). This contract is consulted by `contract-guard` on every PR that modifies persistence paths, deletion logic, or scheduled jobs.

## Anchored requirements

- **FR-10.1** — user-initiated deletion of all user data (drive history, vehicle snapshot, invites, sessions)
- **FR-10.2** — immutable audit log entry per deletion (user ID, timestamp, what, initiator)
- **NFR-3.3** — DB snapshots MUST be self-consistent (partial groups invalid)
- **NFR-3.27** — drive records: 1 year rolling window, background pruning >365 days
- **NFR-3.28** — raw telemetry NOT persisted; only `Vehicle` snapshot (overwritten) and `Drive.routePoints` (bounded by drive lifetime)
- **NFR-3.29** — audit logs retained indefinitely

---

## 1. Single-source-of-truth mapping

Design principle 8 requires that every field has exactly one authoritative source: the database (cold-load / REST) or the WebSocket (real-time). This section is the authoritative mapping.

### 1.1 Source-of-truth definitions

| Source | Meaning |
|--------|---------|
| **DB** | The database column is the canonical value. Reads via REST API or cold-load snapshot return this value. Writes go through the store layer. |
| **WebSocket** | The real-time value delivered over the WebSocket connection. Not persisted as a historical log. The DB may hold a **snapshot** that is overwritten on each event, but the WebSocket is the real-time channel. |
| **DB-only** | The field exists only in the database. There is no corresponding WebSocket event. Managed by Prisma / Next.js app or the Go store layer. |

### 1.2 Vehicle table — dual-source (snapshot + real-time)

The Vehicle table is a **live snapshot**: the DB row is overwritten on each telemetry event. The DB is the SoT for cold-load (initial page load, reconnection), while the WebSocket is the SoT for real-time updates during an active session.

| Column | Cold-load SoT | Real-time SoT | Write path | Notes |
|--------|---------------|---------------|------------|-------|
| `id` | DB | -- | Prisma (create) | Immutable after creation |
| `userId` | DB | -- | Prisma (create) | Immutable after creation |
| `teslaVehicleId` | DB | -- | Go store (setup) | Set once during vehicle setup |
| `vin` | DB | -- | Go store (setup) | Set once during vehicle setup |
| `name` | DB | -- | Prisma (user edit) | User-assigned, not telemetry-driven |
| `model` | DB | -- | Prisma (setup) | Static vehicle metadata |
| `year` | DB | -- | Prisma (setup) | Static vehicle metadata |
| `color` | DB | -- | Prisma (setup) | Static vehicle metadata |
| `licensePlate` | DB | -- | Prisma (user edit) | User-assigned |
| `chargeLevel` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, charge group |
| `estimatedRange` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, charge group |
| `chargeState` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, charge group |
| `timeToFull` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, charge group |
| `status` | DB | WebSocket | Go store (overwrite) | Telemetry-driven |
| `speed` | DB | WebSocket | Go store (overwrite) | Telemetry-driven |
| `gearPosition` | DB | WebSocket | Go store (overwrite) | Telemetry-driven |
| `heading` | DB | WebSocket | Go store (overwrite) | Telemetry-driven |
| `locationName` | DB | WebSocket | Go store (overwrite) | Telemetry-driven |
| `locationAddress` | DB | WebSocket | Go store (overwrite) | Telemetry-driven |
| `latitude` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, AES-256-GCM encrypted |
| `longitude` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, AES-256-GCM encrypted |
| `interiorTemp` | DB | WebSocket | Go store (overwrite) | Telemetry-driven |
| `exteriorTemp` | DB | WebSocket | Go store (overwrite) | Telemetry-driven |
| `odometerMiles` | DB | WebSocket | Go store (overwrite) | Telemetry-driven |
| `fsdMilesSinceReset` | DB | WebSocket | Go store (overwrite) | Telemetry-driven |
| `virtualKeyPaired` | DB | -- | Prisma (setup) | Pairing status flag |
| `setupStatus` | DB | -- | Prisma (setup) | Prisma-owned lifecycle enum |
| `destinationName` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, navigation group |
| `destinationAddress` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, navigation group |
| `destinationLatitude` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, AES-256-GCM encrypted |
| `destinationLongitude` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, AES-256-GCM encrypted |
| `originLatitude` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, AES-256-GCM encrypted |
| `originLongitude` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, AES-256-GCM encrypted |
| `etaMinutes` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, navigation group |
| `tripDistanceMiles` | DB | WebSocket | Go store (overwrite) | Telemetry-driven. Not yet in `vehicle-state-schema.md` SDK schema — DB/store only until added |
| `tripDistanceRemaining` | DB | WebSocket | Go store (overwrite) | Telemetry-driven |
| `navRouteCoordinates` | DB | WebSocket | Go store (overwrite) | Telemetry-driven, AES-256-GCM encrypted |
| `lastUpdated` | DB | -- | Go store (overwrite) | Set on each telemetry write |
| `createdAt` | DB | -- | Prisma (create) | Immutable after creation |
| `updatedAt` | DB | -- | Prisma (auto) | Prisma auto-managed |

### 1.3 Drive table — DB-only (completed drives)

Live drive events (start, route point, speed update) flow over the WebSocket in real-time. Once a drive completes, the Go store writes the finalized `Drive` record to the database. After that point, the DB is the sole source of truth. There is no WebSocket channel for historical drive replay.

| Column | SoT | Write path | Notes |
|--------|-----|------------|-------|
| `id` | DB | Go store (create on drive completion) | Immutable |
| `vehicleId` | DB | Go store (create) | FK to Vehicle |
| `date` | DB | Go store (create) | Drive date |
| `startTime` | DB | Go store (create) | ISO 8601 |
| `endTime` | DB | Go store (create) | ISO 8601 |
| `startLocation` | DB | Go store (create) | Reverse-geocoded |
| `startAddress` | DB | Go store (create) | Reverse-geocoded |
| `endLocation` | DB | Go store (create) | Reverse-geocoded |
| `endAddress` | DB | Go store (create) | Reverse-geocoded |
| `distanceMiles` | DB | Go store (create) | Computed at completion |
| `durationMinutes` | DB | Go store (create) | Computed at completion |
| `avgSpeedMph` | DB | Go store (create) | Computed at completion |
| `maxSpeedMph` | DB | Go store (create) | Computed at completion |
| `energyUsedKwh` | DB | Go store (create) | Computed at completion |
| `startChargeLevel` | DB | Go store (create) | Captured at drive start |
| `endChargeLevel` | DB | Go store (create) | Captured at drive end |
| `fsdMiles` | DB | Go store (create) | Accumulated during drive |
| `fsdPercentage` | DB | Go store (create) | Computed at completion |
| `interventions` | DB | Go store (create) | Count accumulated during drive |
| `routePoints` | DB | Go store (create) | JSONB, AES-256-GCM encrypted, bounded by drive lifetime |
| `createdAt` | DB | Go store (create) | Immutable |

### 1.4 DB-only tables (Prisma-managed)

These tables have no WebSocket representation. They are managed entirely by the Next.js app's Prisma layer (with the exception of `Account`, which the Go telemetry server reads/writes for OAuth token management).

| Table | SoT | Telemetry server access | Notes |
|-------|-----|-------------------------|-------|
| `User` | DB-only | Read (FK resolution) | Prisma-owned. NextAuth manages lifecycle |
| `Account` | DB-only | Read + Write (OAuth token refresh) | Prisma-owned structure. Go store reads `access_token`/`refresh_token`, writes refreshed tokens |
| `Settings` | DB-only | None | Prisma-owned. User preferences |
| `Invite` | DB-only | None | Prisma-owned. Sharing invites |
| `TripStop` | DB-only | None | Prisma-owned. Trip waypoints |

### 1.5 Transient data — NOT persisted (NFR-3.28)

The following real-time telemetry fields are delivered over the WebSocket but are **never written to the database** as historical records. Per design principle 5 ("raw telemetry is never persisted as a historical log") and NFR-3.28:

| Data | Channel | Persistence | Rationale |
|------|---------|-------------|-----------|
| Raw protobuf telemetry payload | Tesla mTLS WebSocket (inbound) | None | Decoded, transformed, and discarded after processing |
| Per-second speed/heading/GPS during active drive | WebSocket (outbound to clients) | None as individual events | Aggregated into `Drive.routePoints` at drive completion only |
| Real-time charge rate | WebSocket | Snapshot only (`Vehicle.chargeLevel` overwritten) | No charge history table |
| Real-time interior/exterior temperature stream | WebSocket | Snapshot only (`Vehicle.interiorTemp`/`exteriorTemp` overwritten) | No temperature history |
| WebSocket connection metadata (client IP, user agent) | In-memory | None | Ephemeral connection state |
| In-memory drive state machine state | In-memory | None | Reconstructed from last Drive record + live telemetry on restart |

> **Key invariant (NFR-3.28):** The only two persistence artifacts from telemetry are: (1) the `Vehicle` row, overwritten on each event, and (2) `Drive` rows with `routePoints`, written once at drive completion and bounded by the drive's retention window.

---

## 2. Retention windows per table

| Table | Retention policy | Window | Pruning mechanism | Anchored requirement |
|-------|-----------------|--------|-------------------|---------------------|
| `User` | Lifetime of user account | Until account deletion | Cascade from FR-10.1 deletion | FR-10.1 |
| `Account` | Lifetime of user account | Until account deletion | Cascade (FK to User, `onDelete: Cascade`) | FR-10.1 |
| `Vehicle` | Lifetime of vehicle record | Until vehicle or user deletion | Cascade (FK to User, `onDelete: Cascade`). Snapshot is overwritten, not versioned. | NFR-3.28, FR-10.1 |
| `Drive` | **1 year rolling window** | 365 days from `createdAt` | Background pruning job (Section 5) + cascade on vehicle/user deletion | **NFR-3.27** |
| `Drive.routePoints` | Bounded by Drive lifetime | Pruned with parent Drive row | Deleted when Drive row is deleted | NFR-3.28 |
| `TripStop` | Lifetime of vehicle record | Until vehicle or user deletion | Cascade (FK to Vehicle, `onDelete: Cascade`) | FR-10.1 |
| `Invite` | Lifetime of vehicle record | Until vehicle or user deletion | Cascade (FK to Vehicle, `onDelete: Cascade`; FK to User sender, `onDelete: Cascade`) | FR-10.1 |
| `Settings` | Lifetime of user account | Until account deletion | Cascade (FK to User, `onDelete: Cascade`) | FR-10.1 |
| `AuditLog` | **Indefinite** | Never deleted | No pruning. Append-only. | **NFR-3.29** |

### 2.1 Vehicle snapshot — overwrite semantics (NFR-3.28)

The Vehicle table does **not** maintain historical versions. Each telemetry event overwrites the current row:

- No `vehicle_history` or `vehicle_snapshots` table exists or will be created.
- The `lastUpdated` timestamp on the Vehicle row reflects the most recent telemetry write.
- If the vehicle goes offline, the DB retains the last-known snapshot until the next event arrives.
- On user deletion, the entire Vehicle row is deleted (not archived).

### 2.2 Drive — 1 year rolling window (NFR-3.27)

- Drives with `createdAt` older than 365 days are eligible for pruning.
- The pruning job (Section 5) runs daily and deletes eligible drives in batches.
- `Drive.routePoints` (JSONB) is deleted with the parent row — there is no separate retention policy for route data.
- On user-initiated deletion (FR-10.1), ALL drives are deleted immediately regardless of age.

### 2.3 AuditLog — indefinite retention (NFR-3.29)

- Audit log rows are never deleted, never updated.
- The AuditLog table is append-only (enforced by database-level policy — see Section 4.3).
- Even when the user who triggered the audited action is deleted, the AuditLog entry remains. The `userId` becomes an orphaned reference (no FK constraint to User — by design, so cascading User deletion does not destroy audit history).

---

## 3. Deletion cascade for FR-10.1

When a user requests deletion of their account (FR-10.1), the system MUST delete all user data and write an immutable audit log entry (FR-10.2).

### 3.1 Deletion ordering

The deletion is executed as a single database transaction with the following steps in order:

```
BEGIN TRANSACTION;

-- Step 1: Write audit log FIRST (before any destructive operations)
INSERT INTO "AuditLog" ("id", "userId", "timestamp", "action", "targetType", "targetId", "initiator", "metadata")
VALUES (
  cuid(),
  '<user-id>',
  NOW(),
  'account_deleted',
  'user',
  '<user-id>',
  'user',
  '{"vehicleCount": N, "driveCount": M, "inviteCount": K}'
);

-- Step 2: Delete the User row — Prisma cascades handle the rest
DELETE FROM "User" WHERE "id" = '<user-id>';

-- Prisma onDelete: Cascade propagation (automatic):
--   User delete  -> Account[]      (all OAuth tokens for this user)
--   User delete  -> Vehicle[]      (all vehicles owned by this user)
--   User delete  -> Invite[]       (all invites SENT by this user)
--   User delete  -> Settings?      (user preferences)
--
--   Vehicle delete -> Drive[]      (all drive history for this vehicle)
--   Vehicle delete -> TripStop[]   (all trip stops for this vehicle)
--   Vehicle delete -> Invite[]     (all invites TO this vehicle)

-- Step 3: Invalidate sessions (NextAuth sessions table)
-- NextAuth sessions are FK'd to User — cascade delete handles this.
-- Active WebSocket connections for this user's vehicles are terminated
-- by the telemetry server when it detects the vehicle record is gone.

COMMIT;
```

### 3.2 Cascade map

```
User (deleted)
 ├── Account[]           (onDelete: Cascade)
 ├── Vehicle[]           (onDelete: Cascade)
 │    ├── Drive[]        (onDelete: Cascade)
 │    ├── TripStop[]     (onDelete: Cascade)
 │    └── Invite[]       (onDelete: Cascade — vehicle-scoped invites)
 ├── Invite[]            (onDelete: Cascade — invites sent by user)
 └── Settings?           (onDelete: Cascade)
```

### 3.3 What is NOT deleted

| Record | Reason |
|--------|--------|
| `AuditLog` entries | Retained indefinitely per NFR-3.29. No FK to User — orphaned `userId` is intentional. |
| Invites where user is the recipient (by email) | Invite table FKs are to sender (`senderId`) and vehicle (`vehicleId`). If the deleted user was an invite recipient (matched by email), the invite row is orphaned but harmless — it references a non-existent email. These are cleaned up by the vehicle owner's cascade. |

### 3.4 Transactional guarantees

- The audit log write and the User delete MUST be in the same transaction. If the audit log insert fails, the deletion is aborted.
- If the transaction fails at any point, no data is deleted and no audit log entry is created. The operation is atomic.
- The Next.js app layer is responsible for initiating this transaction (Prisma `$transaction`). The telemetry server does not initiate account deletions.

### 3.5 WebSocket session cleanup

After the database transaction commits:

1. The Next.js app invalidates the user's HTTP sessions (NextAuth session table is cascade-deleted).
2. The telemetry server detects vehicle deletion on its next DB read cycle and terminates any active WebSocket connections for those vehicles.
3. Active Tesla Fleet Telemetry streams for deleted vehicles are unsubscribed.

---

## 4. Audit log table schema

### 4.1 Table definition

```sql
CREATE TABLE "AuditLog" (
    "id"          TEXT        NOT NULL PRIMARY KEY,   -- cuid, generated by application
    "userId"      TEXT        NOT NULL,               -- user who owns the affected data (NOT an FK — intentional)
    "timestamp"   TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- when the action occurred
    "action"      TEXT        NOT NULL,               -- enum-like: see §4.2
    "targetType"  TEXT        NOT NULL,               -- entity type affected: see §4.2
    "targetId"    TEXT        NOT NULL,               -- ID of the affected entity
    "initiator"   TEXT        NOT NULL,               -- who triggered it: see §4.2
    "metadata"    JSONB                DEFAULT '{}',  -- additional context (counts, batch IDs, etc.)
    "createdAt"   TIMESTAMPTZ NOT NULL DEFAULT NOW()  -- row creation timestamp (matches "timestamp" for new rows)
);

-- Index for querying audit history by user
CREATE INDEX "AuditLog_userId_idx" ON "AuditLog" ("userId");

-- Index for querying by action type
CREATE INDEX "AuditLog_action_idx" ON "AuditLog" ("action");

-- Index for time-range queries
CREATE INDEX "AuditLog_timestamp_idx" ON "AuditLog" ("timestamp");
```

### 4.2 Enum values

**`action` values:**

| Action | Description | Triggered by |
|--------|-------------|--------------|
| `account_deleted` | User account and all associated data deleted | User (FR-10.1) |
| `vehicle_deleted` | Single vehicle and its drives/stops/invites deleted | User |
| `drives_pruned` | Batch of drives older than 365 days deleted | System pruning job (NFR-3.27) |
| `drive_deleted` | Single drive record deleted | User |
| `invite_revoked` | Sharing invite revoked | User |
| `tokens_refreshed` | OAuth tokens rotated | System (token refresh) |

**`targetType` values:**

| Target type | Description |
|-------------|-------------|
| `user` | A User record |
| `vehicle` | A Vehicle record |
| `drive` | A Drive record (or batch of drives) |
| `invite` | An Invite record |
| `account` | An Account (OAuth) record |

**`initiator` values:**

| Initiator | Description |
|-----------|-------------|
| `user` | Action initiated by the user (via UI / API) |
| `system_pruner` | Action initiated by the background pruning job |
| `system_auth` | Action initiated by the system auth/token refresh flow |

### 4.3 Append-only enforcement

The AuditLog table MUST be append-only. No rows may be updated or deleted. This is enforced at the database level:

**Supabase RLS + trigger approach:**

```sql
-- Prevent UPDATE on AuditLog
CREATE OR REPLACE FUNCTION prevent_audit_log_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'AuditLog is append-only: UPDATE and DELETE operations are prohibited';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_log_no_update
    BEFORE UPDATE ON "AuditLog"
    FOR EACH ROW
    EXECUTE FUNCTION prevent_audit_log_mutation();

CREATE TRIGGER audit_log_no_delete
    BEFORE DELETE ON "AuditLog"
    FOR EACH ROW
    EXECUTE FUNCTION prevent_audit_log_mutation();
```

**Application-level enforcement:**

- The Go store layer provides only an `InsertAuditLog()` method. No `UpdateAuditLog()` or `DeleteAuditLog()` methods exist.
- The Next.js Prisma layer should similarly expose only `create` operations for the AuditLog model.
- `contract-guard` blocks any PR that adds UPDATE or DELETE queries targeting the AuditLog table.

### 4.4 Data classification

Per `data-classification.md` Section 2.3: audit log entries are classified **P0** because they contain only opaque identifiers (cuid-format IDs), action enums, and timestamps. They do not contain actual sensitive data (no GPS coordinates, no tokens, no PII). The `metadata` JSONB field MUST contain only aggregate counts and opaque IDs — never P1 values.

| Column | Classification | Log-safe | Rationale |
|--------|---------------|----------|-----------|
| `id` | P0 | Yes | Opaque cuid |
| `userId` | P0 | Yes | Opaque cuid (may be orphaned after deletion) |
| `timestamp` | P0 | Yes | Non-sensitive timestamp |
| `action` | P0 | Yes | Enum value |
| `targetType` | P0 | Yes | Enum value |
| `targetId` | P0 | Yes | Opaque cuid |
| `initiator` | P0 | Yes | Enum value |
| `metadata` | P0 | Yes | Aggregate counts and opaque IDs only |
| `createdAt` | P0 | Yes | Non-sensitive timestamp |

### 4.5 No FK to User (intentional design decision)

The `AuditLog.userId` column is **not** a foreign key to the User table. This is intentional:

- When a user is deleted (FR-10.1), the audit log entry recording that deletion must survive. A cascading FK would destroy the audit trail.
- The `userId` value becomes an orphaned reference after account deletion. This is acceptable because the audit log's purpose is to prove that data was deleted, not to reconstruct the user's profile.
- Queries against the audit log use `userId` as a filter, not a join target.

---

## 5. Pruning job spec (NFR-3.27)

### 5.1 Purpose

A background job that enforces the 1-year rolling retention window for Drive records. Drives with `createdAt` older than 365 days are deleted in batches.

### 5.2 Schedule

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| Schedule | Daily at **03:00 UTC** | Low-traffic window; avoids peak usage hours |
| Frequency | Once per day | Drive creation rate does not justify more frequent runs |
| Timezone | UTC | Server operates in UTC |

### 5.3 Recommended index

The pruning query filters on `createdAt` and the audit entry groups by vehicle owner (via `vehicleId`). A composite index supports both the range scan and the owner lookup:

```sql
CREATE INDEX "Drive_createdAt_vehicleId_idx" ON "Drive" ("createdAt", "vehicleId");
```

This index should be added alongside the pruning job implementation. It covers the `WHERE createdAt < ... ORDER BY createdAt ASC LIMIT 100` scan and allows the job to efficiently resolve the vehicle owner for the audit log entry.

### 5.4 Execution

```
FOR each batch:
  1. SELECT up to 100 Drive records WHERE createdAt < NOW() - INTERVAL '365 days'
     ORDER BY createdAt ASC
     LIMIT 100

  2. IF no rows returned → job complete, exit loop

  3. BEGIN TRANSACTION
       -- Delete the batch (routePoints JSONB is deleted with the row)
       DELETE FROM "Drive" WHERE id IN (<batch_ids>)

       -- Write audit log entry for this batch
       INSERT INTO "AuditLog" ("id", "userId", "timestamp", "action", "targetType", "targetId", "initiator", "metadata")
       VALUES (
         cuid(),
         '<vehicle-owner-user-id>',
         NOW(),
         'drives_pruned',
         'drive',
         '<vehicle-id>',
         'system_pruner',
         '{"driveCount": N, "oldestDriveDate": "<date>", "newestDriveDate": "<date>"}'
       )
     COMMIT

  4. Continue to next batch
```

### 5.5 Batch configuration

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| Batch size | 100 drives | Balances transaction size with throughput. Large enough for efficiency, small enough to avoid long-held locks. |
| Audit granularity | One audit entry per batch per vehicle owner | Groups pruned drives by owner for readable audit history |
| Iteration limit | None (runs until no eligible drives remain) | Daily schedule means at most ~365 new eligible drives per vehicle per run |

### 5.6 Failure handling

| Scenario | Behavior |
|----------|----------|
| Batch transaction fails | Retry with exponential backoff: wait 1s, 2s, 4s (3 attempts max) |
| All 3 retries fail for a batch | Log error at `slog.Error` level, skip to next batch. The failed batch will be retried on the next daily run. |
| Database connection lost | Abort the job. Next daily run will pick up where this one left off (idempotent — only deletes drives older than 365 days). |
| Audit log insert fails | The entire batch transaction rolls back. No drives are deleted without an audit trail. |
| Job takes longer than expected | No hard timeout. The job processes all eligible drives. If this becomes a concern, the batch size can be tuned. |

### 5.7 Observability

The pruning job emits the following metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `pruner_drives_deleted_total` | Counter | Total drives deleted across all batches in this run |
| `pruner_batches_processed_total` | Counter | Number of batches processed |
| `pruner_batch_errors_total` | Counter | Number of batch failures (after retries) |
| `pruner_run_duration_seconds` | Histogram | Wall-clock time for the entire pruning run |
| `pruner_last_success_timestamp` | Gauge | Unix timestamp of last successful completion |

### 5.8 Deployment

The pruning job runs as a scheduled task within the telemetry server process (not a separate service). On Fly.io, this is implemented as a goroutine with a `time.Ticker` that fires daily at 03:00 UTC. The job is leader-elected if multiple instances are running (only one instance executes the prune).

---

## 6. Partial-group persistence rules (NFR-3.3)

### 6.1 Navigation group atomicity

Per NFR-3.3 and `vehicle-state-schema.md` Section 3, the following fields form an atomic group. A Vehicle snapshot write MUST persist all members or none:

**Rule (active navigation completeness):** If `destinationName` is non-null, then `destinationLatitude`, `destinationLongitude`, and `navRouteCoordinates` MUST also be non-null (and vice versa). Per `vehicle-state-schema.md` Section 3.1 predicate 4, `etaMinutes` and `tripDistanceRemaining` MAY arrive slightly after other nav fields during the 500ms accumulation window, but the DB snapshot MUST be fully consistent — these fields are either all present or all null. When all navigation fields are null, this represents "no active navigation" and is valid.

| Field | Required when navigation active | May be null when navigation inactive |
|-------|-------------------------------|--------------------------------------|
| `destinationName` | Yes | Yes |
| `destinationAddress` | Yes* | Yes |
| `destinationLatitude` | Yes | Yes |
| `destinationLongitude` | Yes | Yes |
| `originLatitude` | Yes | Yes |
| `originLongitude` | Yes | Yes |
| `etaMinutes` | Yes | Yes |
| `tripDistanceRemaining` | Yes | Yes |
| `navRouteCoordinates` | Yes | Yes |

> `destinationAddress` is loaded by the Go `Vehicle` struct as of MYR-24 (2026-04-23); the prior spec-only exemption from the active-navigation completeness predicate no longer applies. The field remains nullable on the wire because the underlying Prisma column is `String?`. See `vehicle-state-schema.md` §3.1 predicate 3.

### 6.2 Coordinate pair atomicity

Coordinate pairs MUST be written together:

- `latitude` and `longitude` — both non-null or both null
- `destinationLatitude` and `destinationLongitude` — both non-null or both null
- `originLatitude` and `originLongitude` — both non-null or both null

### 6.3 Enforcement

- **Write path:** The Go store layer validates atomic group completeness before every Vehicle UPDATE. If a partial group is detected, the write is rejected with an error (not silently fixed).
- **Read path:** The SDK validates group completeness on snapshot load. A partial group in the DB indicates a bug in the write path and is logged as an error.
- **contract-guard:** Blocks PRs that add Vehicle write paths without group-completeness validation.

---

## 7. contract-guard rules

The `contract-guard` agent/CI check enforces the following rules derived from this document:

### Rule CG-DL-1: No raw telemetry persistence

**Trigger:** Any PR that adds INSERT or UPDATE queries in `internal/store/`.

**Check:** No new table or column may persist raw telemetry events as a historical log. The only permitted telemetry persistence patterns are: (1) Vehicle snapshot overwrite (single-row UPDATE per vehicle), and (2) Drive record creation (INSERT on drive completion with aggregated data).

**Violation examples:**
- Creating a `telemetry_events` or `telemetry_history` table
- Adding a `vehicle_snapshots` table that stores historical versions
- Inserting individual telemetry data points as separate rows

**Fix:** Remove the historical persistence. Use the Vehicle snapshot (overwrite) or Drive (completion-time insert) patterns per NFR-3.28.

### Rule CG-DL-2: Audit log immutability

**Trigger:** Any PR that modifies `internal/store/` files or SQL migration files.

**Check:** No UPDATE or DELETE statement may target the `AuditLog` table. The only permitted operation is INSERT. This applies to Go code, SQL migrations, and Prisma schema changes.

**Fix:** Remove the UPDATE/DELETE. AuditLog is append-only per NFR-3.29 and FR-10.2.

### Rule CG-DL-3: Deletion requires audit entry

**Trigger:** Any PR that adds DELETE statements targeting User, Vehicle, Drive, Invite, or Account tables.

**Check:** Every deletion path must include a corresponding AuditLog INSERT within the same transaction. The audit entry must be written BEFORE the delete (so it captures the action even if the delete partially fails).

**Fix:** Wrap the deletion in a transaction that writes an AuditLog entry first. See Section 3.1 for the pattern.

### Rule CG-DL-4: Drive pruning boundary

**Trigger:** Any PR that modifies the pruning job or adds Drive deletion logic.

**Check:** Drive deletion by the pruning job MUST only target rows where `createdAt < NOW() - INTERVAL '365 days'`. The 365-day boundary is a constant, not configurable at runtime (to prevent accidental mass deletion).

**Fix:** Use the 365-day threshold per NFR-3.27. If a different retention window is needed, update this contract first.

### Rule CG-DL-5: AuditLog metadata must be P0

**Trigger:** Any PR that writes to the `AuditLog.metadata` JSONB column.

**Check:** The metadata JSON MUST contain only P0 values (opaque IDs, counts, timestamps, enum values). It MUST NOT contain P1 values (GPS coordinates, addresses, tokens, emails, names). Cross-reference with `data-classification.md` Section 1 for tier definitions.

**Violation examples:**
- `{"deletedAddress": "123 Main St"}` — P1 value in metadata
- `{"lastLocation": {"lat": 37.7749, "lng": -122.4194}}` — P1 coordinates in metadata

**Fix:** Replace P1 values with opaque references: `{"driveCount": 42, "vehicleId": "clx..."}`.

### Rule CG-DL-6: Partial group writes

**Trigger:** Any PR that modifies Vehicle UPDATE paths in `internal/store/`.

**Check:** Vehicle writes that touch any navigation group field MUST validate the full group per Section 6.1. A write that sets `destinationName` without also setting `destinationLatitude`, `destinationLongitude`, and `navRouteCoordinates` is invalid. The DB snapshot must also be fully consistent for `etaMinutes` and `tripDistanceRemaining` (all present or all null).

**Fix:** Implement group-completeness validation before the UPDATE. See `vehicle-state-schema.md` Section 3 for the predicate definitions.

### Rule CG-DL-7: AuditLog has no FK to User

**Trigger:** Any PR that modifies the AuditLog table schema or adds Prisma relations.

**Check:** The `AuditLog.userId` column MUST NOT have a foreign key constraint to the User table. Adding a relation (Prisma `@relation`) or FK constraint would cause audit entries to be cascade-deleted when the user is deleted, violating NFR-3.29.

**Fix:** Keep `userId` as an unlinked TEXT column. See Section 4.5 for rationale.

---

## 8. Cross-references

| Topic | Document |
|-------|----------|
| Field-level classification (P0/P1/P2) | `data-classification.md` |
| Atomic group definitions and predicates | `vehicle-state-schema.md` |
| Navigation group field set | `vehicle-state-schema.md` Section 2.1 |
| AES-256-GCM encryption scope | `data-classification.md` Section 3 |
| Functional requirements (FR-10.x) | `requirements.md` Section 10 |
| Non-functional requirements (NFR-3.x) | `requirements.md` Section 3 |
