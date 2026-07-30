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
| `color` | DB | -- | **Go store (Tesla-sourced, MYR-320)** | Static vehicle metadata, but no longer web-app-sourced. The column and the wire field both predate MYR-320 and NEITHER CHANGED — same type, same masks, **no contract change** — only the writer did: it was never actually populated (the MYR-257 provisioning INSERT seeds `''`), and is now filled from Tesla REST `vehicle_data.vehicle_config.exterior_color` through the narrow §1.4 carve-out (`store.VehicleRepo.UpdateVehicleColor`), on the non-waking connectivity-edge read and the periodic in-service re-poll. An EMPTY colour is NEVER written, so a partial Tesla payload cannot blank a good value. No real-time SoT: v1 pushes no WebSocket delta for this column. |
| `licensePlate` | DB | -- | **Go store (owner edit, MYR-286)** | Owner-entered, NOT telemetry and NOT from Tesla — the Fleet API exposes no plate anywhere. Written ONLY by `PUT /api/tesla/vehicles/{vehicleId}/plate` ([`rest-api.md`](rest-api.md) §7.14) through the narrow §1.4 carve-out, normalized on write, and cleared by writing `''`. No real-time SoT: v1 pushes no WebSocket delta for this column. |
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
| `startChargeLevel` | DB | Go store (complete) | SOC at drive start; the create-time insert defaults it to 0 (drive.started carries no charge), so it is persisted on the completion UPDATE from the detector's last-known/first-in-drive SOC (MYR-241) |
| `endChargeLevel` | DB | Go store (create) | Captured at drive end |
| `fsdMiles` | DB | Go store (create) | Accumulated during drive |
| `fsdPercentage` | DB | Go store (create) | Computed at completion |
| `interventions` | DB | Go store (create) | Count accumulated during drive |
| `routePoints` | DB | Go store (create) | JSONB, AES-256-GCM encrypted, bounded by drive lifetime |
| `createdAt` | DB | Go store (create) | Immutable |

### 1.4 DB-only tables (Prisma-managed)

These tables have no WebSocket representation. They are managed primarily by the Next.js app's Prisma layer, with a small set of narrowly-scoped, sanctioned exceptions where the Go telemetry server owns a specific owner-facing flow: `Account`, which the Go server reads/writes for OAuth token management; `AuditLog`, which the Go server writes (Insert-only) via raw pgx; the owner-onboarding **provision** carve-out (`store.OwnerProvisioner`, MYR-257) that upserts `User`/`Settings`/`Account`/`Vehicle` on a completed Tesla link; and its exact inverse, the owner-offboarding **teardown** carve-out (`store.OwnerTeardown`, MYR-258) that owner-scoped **DELETEs** a single `Vehicle` (+ its cascade) plus the Go-owned `go_ride_requests` rows for that vehicle (P1 encrypted pickup/dropoff GPS + passenger PII — no FK cascade reaches them, so a complete removal deletes them explicitly) and, on the owner's last vehicle, clears the `Account` tokens + resets `Settings`, writing the user-initiated `vehicle_deleted` audit row in the same transaction. Since **MYR-261** the teardown also writes a **removed-vehicle tombstone** into the Go-owned `go_removed_vehicles` table in that same transaction (see §1.4.1); since **MYR-286**, the owner **license-plate** carve-out (`store.VehicleRepo.UpdateLicensePlate`) that performs a **single-column owner-scoped UPDATE** of `Vehicle.licensePlate` and nothing else; and, since **MYR-320**, the **exterior-colour** carve-out (`store.VehicleRepo.UpdateVehicleColor`, [`internal/store/vehicle_color.go`](../../internal/store/vehicle_color.go)) that likewise performs a **single-column owner-scoped UPDATE** of `Vehicle.color` and nothing else, from Tesla's `vehicle_data.vehicle_config.exterior_color`. and, since **MYR-355**, the **account-deletion** carve-out (`store.AccountDeleter`, [`internal/store/account_deletion_identity.go`](../../internal/store/account_deletion_identity.go)) — the only one that **DELETEs the `User` row itself**, described in full below. All five carve-outs are gated by `sdk-architect` + `contract-guard` and a cross-repo schema-verification against the Next.js Prisma source.

**Account-deletion carve-out (MYR-355) — scope and rationale.** This is the widest of the five and the only one that touches `User`, so its justification has to be the strongest. It is the FR-10.1 deletion transaction itself, moved to the Go server because the native iOS client is the only consumer and never reaches the Next.js app (§3). Its scope:

- **One transaction, three DELETEs, one INSERT.** The `account_deleted` AuditLog INSERT first (CG-DL-3), then `go_identity_apple`, `go_users`, and `"User"`. Nothing else — every other table the deletion touches is handled by an earlier, separately-atomic step of §3.1 (the per-vehicle teardown, or a single caller-scoped statement).
- **Caller-scoped in SQL.** Every statement is `WHERE … = $1` on the caller's own cuid. There is no vehicle id, no other user's id, and no path by which a caller reaches a second account: the endpoint is `/users/me`, so the id is the token subject and nothing else.
- **The `User` DELETE is a BACKSTOP, not the mechanism.** The Prisma cascade (`Account`, `Settings`, `Invite`, `Vehicle` → `Drive`/`TripStop`) is real, but by the time this statement runs, step 3 of §3.1 has already torn each owned vehicle down through the audited `store.OwnerTeardown` transaction — which is what writes the per-vehicle `vehicle_deleted` audit rows and fires the NOTIFY. Relying on the cascade alone would delete the same cars **silently and unaudited**.
- **It runs LAST and it may run again.** See §3.4. The already-gone arm commits an empty transaction and writes no audit row.
- **Audit row required.** Unlike the plate and colour carve-outs, this one destroys data, so CG-DL-3 applies in full: `action='account_deleted'`, `targetType='user'`, `targetId`=the caller's own cuid, `initiator='user'`, `metadata` = P0 counts only (CG-DL-5).
- **No migration.** All three tables already exist. **CG-DL-9 does not fire:** that rule constrains Go *migration SQL* referencing Prisma-owned tables, and this ships none — an application-runtime Prisma DELETE is the sanctioned class, exactly as `store.OwnerTeardown`'s runtime DELETE is.

**License-plate carve-out (MYR-286) — scope and rationale.** The plate is a `Vehicle` column with **no other possible writer**. It is not telemetry and not a Tesla value: the Fleet API exposes no license plate on any endpoint, in any telemetry field, or in any proto, so there is nothing to sync or decode — and no Next.js/Prisma surface writes it either. The column is populated **only** by the owner typing it into `PUT /api/tesla/vehicles/{vehicleId}/plate` ([`rest-api.md`](rest-api.md) §7.14), which the Go server owns. (Since **MYR-320** it is no longer the *only* such column — `color` is the other, for the mirror-image reason: Tesla is its sole source and Prisma never writes it either. The two single-column carve-outs are deliberately shaped alike; see the MYR-320 note below.) The carve-out is deliberately narrow — at the time it landed, the narrowest of the three:

- **One column.** `SET "licensePlate" = $1, "updatedAt" = NOW()`. No telemetry column is touched, so the write can never race or clobber the streaming pipeline.
- **Owner-scoped in SQL.** `WHERE "id" = $2 AND "userId" = $3` — ownership is a predicate, not a caller precondition, so a mismatched user updates zero rows rather than another owner's car. The handler checks ownership too; the SQL scope is the fail-closed backstop.
- **UPDATE only.** No INSERT, no DELETE, no cascade, no transaction. Clearing a plate writes the empty string (the column is `TEXT NOT NULL DEFAULT ''`), never NULL.
- **No audit row.** Unlike the teardown, this is a non-destructive edit of a value the owner supplied about their own car; CG-DL-3 (deletion requires an audit entry) does not apply because nothing is deleted.
- **No migration.** The column already exists in the Prisma schema. **CG-DL-9 does not fire:** that rule constrains Go *migration SQL* referencing Prisma-owned tables, and this ships none — an application-runtime Prisma UPDATE is the sanctioned class, exactly as `store.OwnerProvisioner`'s runtime upsert is.

The value is **P1** ([`data-classification.md`](data-classification.md) §1.3): it must be redacted from logs and never emitted outside the vehicle's party. It IS on the wire to both roles as of MYR-286 (`rest-api.md` §5.2.0 / §5.2.1) — a rider needs it to identify the car at pickup. The teardown backs `DELETE /api/tesla/vehicles/{vehicleId}` ([`rest-api.md`](rest-api.md) §7.12; design [`../architecture/car-offboarding.md`](../architecture/car-offboarding.md)).

**Exterior-colour carve-out (MYR-320) — scope and rationale.** The colour is the mirror image of the plate: the `Vehicle.color` column has ALREADY existed for the whole life of the schema and is ALREADY on the wire to both roles — but it was **never actually populated**. The MYR-257 provisioning INSERT seeds it as `''`, no Next.js/Prisma surface ever writes it, and nothing else in the stack knew the car's colour, so every consumer has been rendering an empty string since day one. Tesla is the **only** source that has the value (`vehicle_data.vehicle_config.exterior_color`, live-verified as `"Quicksilver"` against the owner's own car), and the Go server is the only component that reads Tesla — so unlike the telemetry columns there is **no Prisma writer to defer to**, and unlike the plate there is no owner-entry surface either. The carve-out is the narrowest yet:

- **One column.** `SET "color" = $1, "updatedAt" = NOW()`. No telemetry column and no identity column is touched, so the write can never race or clobber the streaming pipeline.
- **Owner-scoped in SQL.** `WHERE "vin" = $2 AND "userId" = $3` — ownership is a predicate, exactly as in the MYR-286 plate write, so a mismatched user updates zero rows rather than another owner's car. The lookup key is the VIN rather than the cuid because the caller is a Tesla read keyed by VIN, and the zero-rows outcome is deliberately indistinguishable between "unknown VIN" and "owned by someone else".
- **UPDATE only, and an EMPTY colour is NEVER written.** No INSERT, no DELETE, no cascade. A partial or degraded Tesla payload that omits `exterior_color` must not BLANK a colour we already know, so the empty case is a no-op rather than a write of `''` — the same "never fabricate, never regress" discipline the nullable side-table columns get, expressed here on a `NOT NULL` column.
- **No audit row.** This is a non-destructive refresh of a factual attribute of the owner's own car; CG-DL-3 (deletion requires an audit entry) does not apply because nothing is deleted.
- **No migration.** The column already exists in the Prisma schema. **CG-DL-9 does not fire:** that rule constrains Go *migration SQL* referencing Prisma-owned tables, and this ships none — an application-runtime Prisma UPDATE is the sanctioned class, exactly as the MYR-286 plate UPDATE and `store.OwnerProvisioner`'s runtime upsert are.

The value is **P0** ([`data-classification.md`](data-classification.md) §1.3) and **log-safe in full** — an appearance fact about a car, the same tier as `model` and `year`, correlating to no person. That is the one place it diverges from the plate carve-out it otherwise copies: the plate is **P1** and must be redacted from logs, because a plate is externally correlatable to a person via a registry lookup and a paint colour is not. **There is no contract change:** same field, same type, same nullability, same masks, no schema version bump on `color` itself — only its provenance moved, which is why this note lives here rather than in a wire-shape doc. The write rides the existing non-waking connectivity-edge read plus the MYR-320 periodic in-service re-poll, so it costs no extra Tesla call and never wakes a sleeping car.

#### 1.4.1 Removed-vehicle tombstone — `go_removed_vehicles` (MYR-261)

The MYR-258 teardown deletes the local `Vehicle` row but does **not** revoke the owner's access or virtual key at Tesla (that is the owner-confirmed consent-revoke page, §1.2). So the still-Tesla-owned car remains visible to the Fleet API, and the best-effort post-link vehicle sync (`ownerStreamHook.AfterLink` → `store.OwnerProvisioner.UpsertOwnedVehicle`, an `INSERT … ON CONFLICT ("teslaVehicleId")` upsert) would **re-insert the removed VIN on the very next Tesla re-link** — the car reappeared. `go_removed_vehicles` is the durable per-owner tombstone that closes this gap:

- **Schema.** Go-owned table (migration `0006_removed_vehicles`), `go_` prefix, snake_case, natural composite **primary key `(user_id, tesla_vehicle_id)`** plus a nullable `vin` (operator correlation) and `removed_at`. **No Prisma FK** to `Vehicle`/`User` (CG-DL-9) — the ids are plain columns. All columns are **P0** (opaque cuid + opaque Tesla vehicle id + redactable VIN + timestamp).
- **Write (create).** The teardown inserts the tombstone (`ON CONFLICT DO UPDATE` refreshes `removed_at`, idempotent) inside the **same transaction** as the `Vehicle` delete, so tombstone and delete are atomic. Skipped for a car with no `teslaVehicleId` (nothing a Fleet-API sync could resurrect). The create is recorded on the existing `vehicle_deleted` audit row's metadata (`tombstoned: true`) — no second audit row.
- **Honor (skip).** `UpsertOwnedVehicle` checks the tombstone **before** upserting and returns the `skipped_tombstoned` outcome for any tombstoned `(user, teslaVehicleId)`. The check lives in the shared upsert method so it covers **every** re-add sync route, not just `AfterLink`. A passive re-link can therefore never resurrect a removed car — **the tombstone wins by default.**
- **Clear (deliberate re-add).** `AfterLink` is a *bulk* sync of all Fleet-API vehicles and cannot, on its own, distinguish "the owner deliberately wants this removed car back" from "the owner is just re-linking and this removed car is still in their Tesla account." The safe default is therefore tombstone-wins, with an **explicit un-tombstone entry point**: `store.RemovedVehicleRegistry.ClearTombstone(userID, teslaVehicleId)` deletes the tombstone (transactional, idempotent) and, on an actual clear, writes a `vehicle_readd_allowed` audit row (§4.2) in the same transaction. After a clear, the next sync provisions the car normally. A deliberate re-add flow MUST call `ClearTombstone` first; the passive sync never clears a tombstone. Since **MYR-262** the sanctioned deliberate-re-add path is the owner-authenticated `POST /api/tesla/vehicles/{teslaVehicleId}/re-add` ([`rest-api.md`](rest-api.md) §7.13), which clears the caller's own tombstone then best-effort re-provisions the car; an operator stopgap (`ops vehicles re-add`) clears a tombstone out-of-band over the same registry.

| Table | SoT | Telemetry server access | Notes |
|-------|-----|-------------------------|-------|
| `User` | DB-only | Read (FK resolution) + **Insert-only** (owner provisioning, MYR-257) | Prisma-owned. NextAuth manages lifecycle. The Go server may `INSERT ... ON CONFLICT ("id") DO NOTHING` a minimal owner row (id/name/email/updatedAt) via `store.OwnerProvisioner`, **only** on a completed Tesla link (see [`../architecture/self-serve-onboarding.md`](../architecture/self-serve-onboarding.md)); never UPDATE/DELETE. |
| `Account` | DB-only | Read + **Upsert** (OAuth tokens) + **Delete (last-vehicle teardown, MYR-258)** | Prisma-owned structure. Go store reads `access_token`/`refresh_token`, writes refreshed tokens (`UpdateTeslaToken`) and, on first in-app link, INSERTs the row (`ON CONFLICT (provider, providerAccountId) DO UPDATE`, MYR-257) with dual-write-encrypted tokens. On the owner's **last** vehicle teardown, `store.OwnerTeardown` DELETEs the row owner+provider-scoped (`WHERE "userId"=… AND "provider"='tesla'`) to clear our access — this removes OUR tokens, NOT the Tesla-side grant (owner-confirmed consent page; car-offboarding.md §1.2) |
| `Settings` | DB-only | **Insert/upsert-only** (owner provisioning, MYR-257; link/pairing reset, MYR-258) | Prisma-owned. User preferences. The Go server may upsert a minimal row (`teslaLinked=true`, `ON CONFLICT ("userId")`) via `store.OwnerProvisioner` on a completed Tesla link, and on the owner's last-vehicle teardown reset the link/pairing flags (`teslaLinked=false`, `virtualKeyPaired=false`, `keyPairingReminderCount=0`) via `store.OwnerTeardown`; never touches other Settings columns |
| `Vehicle` | DB-only | Read + Update (telemetry) + **Insert (identity seed, MYR-257)** + **Delete (owner teardown, MYR-258)** | Prisma-owned. The streaming pipeline updates live columns; the in-app link's best-effort sync may INSERT identity columns (`teslaVehicleId`/`vin`/`name`, `ON CONFLICT ("teslaVehicleId")`) so a new owner's car appears without the web app. The owner "Remove this car" flow DELETEs one row owner-scoped (`WHERE "id"=… AND "userId"=…`) via `store.OwnerTeardown` — cascading `Drive`/`TripStop`/vehicle-scoped `Invite` + the encrypted route blobs and firing the existing `vehicle_deleted` NOTIFY (§3.5). The delete is paired with a `vehicle_deleted` AuditLog INSERT in the same transaction (CG-DL-3) |
| `Invite` | DB-only | None | Prisma-owned. Sharing invites. Per [`rest-api.md`](rest-api.md) §10 DV-23 (RESOLVED 2026-05-08, MYR-69), the Next.js app serves the §7.5 invite endpoints directly; no `InviteRepo` exists in `internal/store/`. |
| `TripStop` | DB-only | None | Prisma-owned. Trip waypoints |
| `AuditLog` | DB-only | **Insert-only** (raw pgx) | Prisma-owned schema. **Since MYR-355 the Go server initiates the FR-10.1 account deletion and writes the `account_deleted` row itself**, inside the same transaction as the identity delete (§3.1 step 9). The Go telemetry server holds Insert-only access via raw pgx for system-initiated rows — `drives_pruned` (NFR-3.27 pruning job, §5), `mask_applied` (1% sampling, §4.2 / [`rest-api.md`](rest-api.md) §5.3), `tokens_refreshed` (OAuth refresh) — **and, since MYR-258, the ONE user-initiated row it owns: `vehicle_deleted`**, written by `store.OwnerTeardown` inside the same transaction as the owner-scoped `Vehicle` delete (CG-DL-3 requires the audit BEFORE the delete). `targetType='vehicle'`, `initiator='user'`, `metadata={driveCount, wasLastVehicle, tombstoned}` — P0 counts/flags only (CG-DL-5). Since **MYR-261** the Go server also owns the user-initiated `vehicle_readd_allowed` row (written by `store.RemovedVehicleRegistry.ClearTombstone` when an owner deliberately re-adds a previously removed car; `targetType='vehicle'`, `targetId`=Tesla vehicle id, `initiator='user'`, `metadata={existed}`). UPDATE/DELETE remain prohibited at the database level (§4.3 triggers) and the application level (no `UpdateAuditLog` / `DeleteAuditLog` methods exist; `contract-guard` CG-DL-2 enforces this on every PR). |

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

> **REWRITTEN BY [MYR-355](https://linear.app/myrobotaxi/issue/MYR-355) (2026-07-30).** This section previously specified a single Prisma `$transaction` initiated by the Next.js app, and stated that "the telemetry server does not initiate account deletions". Both are superseded. The **Go telemetry server** owns `DELETE /api/users/me` ([`rest-api.md`](rest-api.md) §7.6) because the native iOS client is the only consumer and never reaches the Next.js app — and because the `User` cascade was never the whole deletion: `go_ride_requests`, `go_vehicle_shares`, `go_push_devices`, `go_refresh_tokens`, `go_users`, `go_identity_apple` and `go_removed_vehicles` carry no FK to `User` (CG-DL-9 forbids one), so no Prisma cascade has ever reached a single one of them.
>
> **The consequence is that the deletion is NOT one transaction, and the contract's guarantee changes shape.** §3.4 below states the new one.

### 3.1 Deletion ordering

The deletion is a SEQUENCE of independently-atomic steps, executed in this order by `telemetry.AccountDeletionHandler` (`internal/telemetry/account_deletion_sequence.go`) over `store.OwnerTeardown` and `store.AccountDeleter`:

| # | Step | Writer | Idempotent because |
|---|------|--------|--------------------|
| 1 | Count the user's drives (audit metadata only) | `AccountDeleter.CountUserDrives` | Read-only; a failure is logged and ignored — a missing statistic must never block erasure |
| 2 | **Revoke the Tesla OAuth grant AT TESLA** (MYR-366) | `telemetry.TeslaLinkRevoker` → `POST https://auth.tesla.com/oauth2/v3/revoke` | A re-run finds no `Account` row, so there is no token to present and the step skips without calling Tesla |
| 3 | For EACH owned vehicle: the §1.4 owner-teardown transaction | `OwnerTeardown.RemoveVehicle` | An already-removed car returns `AlreadyGone` — a clean no-op success with no duplicate audit row |
| 4 | Revoke every grant the user REDEEMED | `AccountDeleter.RevokeSharesReceived` | `WHERE accepted_by_user_id = $1 AND status <> 'revoked'` matches nothing on a re-run |
| 5 | Cancel every OPEN ride the user holds as RIDER | the guarded §7.8 transition + `ride_status_changed` publish | The guarded `UPDATE … WHERE status = ANY(from)` cannot re-fire; a lost race is not an error |
| 6 | Delete the user's push devices | `AccountDeleter.DeletePushDevices` | `DELETE … WHERE user_id = $1` affects zero rows on a re-run |
| 7 | Delete the user's saved places (MYR-321) | `AccountDeleter.DeleteSavedPlaces` | `DELETE … WHERE user_id = $1` affects zero rows on a re-run |
| 8 | Revoke the user's refresh tokens | `AccountDeleter.RevokeRefreshTokens` | `WHERE user_id = $1 AND revoked = FALSE` matches nothing on a re-run |
| 9 | Identity + audit, ONE transaction | `AccountDeleter.DeleteIdentity` | The transaction probes the three identity sources first; finding none it commits empty and writes NO audit row |
| 10 | Invalidate the auth caches | `auth.JWTAuthenticator` | Pure cache eviction |

**Step 2 must precede step 3, and this ordering is normative too** ([MYR-366](https://linear.app/myrobotaxi/issue/MYR-366)). The revoke call presents the stored `refresh_token`; step 3's last-vehicle arm DELETEs the `Account` row that holds it, and step 9's `User` cascade takes any row that survived. After either, the credential the revocation needs no longer exists and only the owner can withdraw the grant by hand from the consent page. Revoking first is the only ordering in which the server can do it at all.

**Step 2 is BEST-EFFORT and its failure is NOT an error.** Every failure mode — no `Account` row, a database read error, a network error, a Tesla 5xx, an already-invalid token — is logged at WARN and the sequence continues. Tesla's availability MUST NOT be able to block a person's erasure of their own account, so the step has no error path a caller could propagate. It is skipped entirely when no Tesla OAuth `client_id` is configured. The step writes **no `AuditLog` row**: it records a P0 structured log line `event=tesla_tokens_revoked` carrying the `user_id` and nothing else — never the token, its prefix, its length, or a VIN.

**Step 9 is the only transaction that deletes identity, and it runs LAST. That ordering is normative**, because the caller authenticates with a token that resolves through exactly those rows: deleting them earlier would leave a half-deleted account that nobody — not even its owner — could finish deleting.

Step 9, in full (CG-DL-3 requires the audit BEFORE the destructive delete):

```
BEGIN TRANSACTION;

-- Step 1: Write audit log FIRST (before the destructive deletes in this tx)
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

-- Step 2: Delete the Go-owned identity rows. An Apple-native user has no
-- "User" row and a legacy web user has no go_users row; BOTH statements run
-- unconditionally and simply affect zero rows in the case that does not apply
-- (dual-source identity — neither case is special-cased, neither is an error).
DELETE FROM go_identity_apple WHERE user_id = '<user-id>';
DELETE FROM go_users         WHERE id      = '<user-id>';

-- Step 3: Delete the Prisma User row IF one exists — its cascades are the
-- BACKSTOP, not the mechanism: by now step 3 of §3.1 has already torn down
-- every owned vehicle one transaction at a time, so the cascade normally has
-- nothing left to take.
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

COMMIT;

-- Sessions are already gone before this transaction opens: step 8 of §3.1
-- revoked every go_refresh_tokens row, and step 10 evicts the user-existence
-- cache immediately after the commit so the caller's still-unexpired ES256
-- access token stops validating at once rather than at the cache TTL. Active
-- WebSocket connections for this user's vehicles were closed during step 3 by
-- the vehicle_deleted NOTIFY (§3.5).
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
| **Terminal `go_ride_requests` rows where the deleted user was the RIDER** | **Counterparty records — see §3.3.1.** |
| Revoked `go_vehicle_shares` tombstones | Revocation has always been a tombstone rather than a delete (migration 0020). The owner's trail of who could see their car outlives the viewer's account. |
| Revoked `go_refresh_tokens` rows | The rotation lineage is reuse-detection evidence. Only the SHA-256 digest was ever stored — the raw token never was — so the retained row is not a credential. |
| `go_removed_vehicles` tombstones | They exist to stop a removed VIN being resurrected by a later Tesla sync; deleting them would restore exactly the bug MYR-261 closed. |
| The Tesla virtual key | There is no Fleet API path to remove it; only the owner can, from the car's touchscreen ([`../architecture/car-offboarding.md`](../architecture/car-offboarding.md) §1.3). |
| The Tesla-side grant, **when revocation fails** | Since [MYR-366](https://linear.app/myrobotaxi/issue/MYR-366) step 2 of §3.1 actively revokes it, so the grant normally DOES go. But the call is best-effort: if Tesla refuses or is unreachable, the deletion still completes and the grant survives on the owner's tesla.com third-party-apps page for them to remove. The owner-confirmed consent page (§1.2) remains the fallback, not the primary mechanism. |
| Invites where user is the recipient (by email) | The Prisma `Invite` table is **retired unused** (data-classification.md §1.6) — no row was ever written against it. Retained here only because the relation still exists in the sibling schema. |

#### 3.3.1 Ride history is a counterparty record

A completed ride has **two** parties. Erasing the rider's copy erases the owner's history of their own car — a second person's data, deleted to satisfy the first person's request. So terminal rows (`completed` / `declined` / `cancelled`) in which the deleted user was the **rider** are **kept, whole and unmodified**: `rider_id` still holds the deleted user's cuid, and nothing is rewritten.

**No column was added, and none was needed.** The requester-name resolution built by MYR-229 and extended by MYR-264 (`requesterIdentitySelect`, `internal/store/ride_request_queries.go`) already distinguishes the two cases that matter, via its `requester_exists` probe across all three identity sources:

| Situation | `requester_exists` | `requesterName` on the wire |
|---|---|---|
| Rider exists, has a name or email | `true` | the resolved first name / email local-part |
| Rider exists, has neither | `true` | the literal `"Rider"` — *"a rider with no name on file"* |
| **Rider's account was deleted** | `false` | **OMITTED** |

An omitted `requesterName` on the live path therefore means precisely *"this account was deleted"*, and the iOS client renders it as **"Former rider"**. The alternative designs — a nullable `requester_display_name` snapshot column, or rewriting `rider_id` to a sentinel — were both rejected: the first duplicates a value that already resolves correctly, and the second destroys the only linkage that makes the retained row auditable.

`TestAccountDeletion_RideHistorySurvivesAsFormerRider` pins it end to end: a real first name before the deletion, an omitted one after, with the row and its status intact.

**The asymmetry, stated rather than hidden.** An OWNER's deletion runs the §1.4 teardown per car, and that teardown **deletes** the car's `go_ride_requests` rows outright — so riders lose their history of rides in that owner's car. That is pre-existing MYR-258 behaviour (those rows carry P1 encrypted pickup/dropoff GPS for a vehicle leaving the platform, and no FK cascade reaches them) and MYR-355 deliberately did not change it. **A rider's deletion preserves the owner's history; an owner's deletion does not preserve the rider's.** Revisiting that is its own decision, not a side effect of this one.

### 3.4 Transactional guarantees

**The guarantee is RE-RUNNABILITY, not whole-sequence atomicity.** The two cannot both be had here, and the reason is structural rather than a matter of effort:

- Step 3 of §3.1 is `store.OwnerTeardown`, which is **already** a transaction — one that takes `SELECT … FOR UPDATE` locks over the owner's whole vehicle set (so the last-vehicle decision is race-safe) and fires the `vehicle_deleted` NOTIFY whose consumers must not observe uncommitted work.
- Step 5 publishes `ride_status_changed`, which sends **push notifications**. A notification cannot be rolled back.

Wrapping N teardowns plus a notifying step in one outer transaction would therefore either deadlock against those locks or tell people about work a later rollback undid. What the contract guarantees instead:

- **Every step is idempotent.** Re-running affects zero rows for work already done (the "Idempotent because" column of §3.1 is normative).
- **The sequence is re-runnable.** A failure answers `500` and leaves a partially-deleted account; calling `DELETE /api/users/me` again resumes from wherever it stopped.
- **The identity delete is LAST**, so a failure never leaves an account that cannot authenticate to finish deleting itself.
- **The audit row and the identity delete are in the SAME transaction** (CG-DL-3), audit first. If the audit insert fails, no identity row is deleted.
- **Exactly one `account_deleted` row is ever written**, however many times the endpoint is called: the transaction probes the identity sources first and writes nothing on the already-gone arm.
- The **Go telemetry server** initiates the deletion. The previous sentence assigning this to the Next.js app is superseded (MYR-355).

**What a mid-sequence failure genuinely leaves.** Between steps, the account is real but degraded — some cars torn down, some grants revoked. This is a deliberate trade: the alternative is refusing the erasure entirely on any transient database error, which serves the user worse and satisfies neither FR-10.1 nor the App Store requirement. Partial state is visible only to the account's own owner, resolves on the next call, and cannot leak another user's data (every statement is caller-scoped in SQL).

### 3.5 WebSocket session cleanup

After the database transaction commits:

1. The Next.js app invalidates the user's HTTP sessions (NextAuth session table is cascade-deleted).
2. The telemetry server detects vehicle deletion on its next DB read cycle and terminates any active WebSocket connections for those vehicles.
3. Active Tesla Fleet Telemetry streams for deleted vehicles are unsubscribed.


### 3.5.1 Asymmetric DB-outage behavior (operational note)

The two new authorization paths added by MYR-73 (2026-05-09) react to transient Postgres errors in opposite directions, and on-call should know about the asymmetry:

| Path | DB-error policy | Outage symptom |
|------|----------------|---------------|
| `JWTAuthenticator.ValidateToken` user-existence check | **Fail-closed** | A Postgres blip rejects every new browser WebSocket handshake with `auth_failed`. Existing connections survive (the check runs only on new handshakes). |
| `Receiver` (Tesla mTLS) authorizer | **Fail-open** | A Postgres blip silently admits every new inbound mTLS upgrade. Real vehicles keep flowing; rejection of post-deletion VINs happens *only* once the cache evicts and the DB is reachable. |

Both choices are individually correct for their context: the WS path is user-facing and a brief auth_failed nag is preferred over silently leaking access; the Tesla path is car-facing and dropping a real vehicle's stream because the DB is briefly unreachable would lose live telemetry that has nowhere to be replayed. The side effect of the combination is that a DB outage produces a one-sided service degradation — the dashboard shows browsers failing while the telemetry rate looks normal. Watch `tesla_inbound_rejected_total{reason="vehicle_not_authorized"}` AND PostgreSQL availability metrics together when triaging.

---

## 4. Audit log table schema

> **Ownership.** The `AuditLog` table is part of the **Next.js app's Prisma schema** (consistent with the §1.4 Prisma-managed-table list and [`rest-api.md`](rest-api.md) §10 DV-23, RESOLVED 2026-05-08, MYR-69). Migrations are authored in the Next.js repo via Prisma; the Go telemetry server does NOT own the migration toolchain for this table. **Since MYR-355 the Go telemetry server writes the `account_deleted` row (FR-10.1)**, inside the same transaction as the identity delete defined in §3.1 step 9, and owns the deletion sequence per §3.4 — it serves `DELETE /api/users/me`, so it owns that audit row, by the same rule that gave it `vehicle_deleted`. The Go telemetry server holds **Insert-only** access via raw pgx for the system-initiated rows (`drives_pruned`, `mask_applied`, `tokens_refreshed`) and — since MYR-258 — the user-initiated `vehicle_deleted` row, which `store.OwnerTeardown` writes inside the same transaction as the owner-scoped per-vehicle delete it backs (`DELETE /api/tesla/vehicles/{vehicleId}`; the Go server owns that endpoint, so it owns that audit row). UPDATE and DELETE are prohibited at both the database level (§4.3 triggers) and the application level (`contract-guard` CG-DL-2). The schema below is the canonical definition that both the Prisma model and the Go pgx writer MUST mirror exactly; drift between them is a contract violation.

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
| `account_deleted` | User account and all associated data deleted (FR-10.1). **Emitted by the Go telemetry server since MYR-355** — `store.AccountDeleter.DeleteIdentity`, in the same transaction as the identity delete (§3.1 step 9, CG-DL-3). `targetType='user'`, `targetId`=the caller's own cuid, `initiator='user'`, `metadata={vehicleCount, driveCount, ridesCancelled, sharesRevoked, pushDevicesDeleted, refreshTokensRevoked, hadPrismaUser}` — P0 counts/flags only (CG-DL-5). Written at most ONCE per account: the transaction probes the three identity sources first and the already-gone arm writes nothing, so the endpoint's re-run path cannot duplicate it | User (FR-10.1) |
| `vehicle_deleted` | Single vehicle and its drives/stops/invites deleted. Since MYR-261 the same-tx write also creates a `go_removed_vehicles` tombstone (§1.4.1); `metadata.tombstoned` records whether one was written | User |
| `vehicle_readd_allowed` | Owner deliberately re-added a previously removed car — the `go_removed_vehicles` tombstone for `(userId, teslaVehicleId)` was cleared so the next Tesla sync may provision the VIN again (MYR-261, §1.4.1). `targetType='vehicle'`, `targetId` is the Tesla vehicle id, `initiator='user'`, `metadata={existed}` (P0 only). Emitted by `store.RemovedVehicleRegistry.ClearTombstone` in the same transaction as the tombstone DELETE (CG-DL-3) | User |
| `drives_pruned` | Batch of drives older than 365 days deleted | System pruning job (NFR-3.27) |
| `drive_deleted` | Single drive record deleted | User |
| `invite_revoked` | Sharing invite revoked | User |
| `tokens_refreshed` | OAuth tokens rotated | System (token refresh) |
| `mask_applied` | Role-based field mask removed at least one field from a REST response or WebSocket broadcast (sampled at 1%) | System (broadcast / handler layer); see [`rest-api.md`](rest-api.md) §5.3 |
| `data_exported` | User-initiated portability export of every Prisma row owned by the caller (GDPR Art. 15 right of access / Art. 20 portability). Emitted by the Next.js `GET /api/users/me/export` handler ([Phase A: myrobotaxi/react-frontend#259](https://github.com/myrobotaxi/react-frontend/pull/259); MYR-75). One row per export — sampling 100% (not high-volume); retained indefinitely per NFR-3.29. `targetType` MUST be `user`, `targetId` MUST be the caller's `userId`, `initiator` MUST be `user`. `metadata` shape is exactly `{vehicleCount, driveCount, inviteCount, auditCount}` — P0 counts only per Rule CG-DL-5; never PII, GPS, addresses, or tokens. See [`rest-api.md`](rest-api.md) §7.7. | User (caller-initiated portability export per GDPR Art. 15 / Art. 20) |

**`targetType` values:**

| Target type | Description |
|-------------|-------------|
| `user` | A User record |
| `vehicle` | A Vehicle record |
| `drive` | A Drive record (or batch of drives) |
| `invite` | An Invite record |
| `account` | An Account (OAuth) record |
| `rest_response` | A REST API response that was mask-projected (paired with `action: mask_applied`) |
| `ws_broadcast` | A WebSocket frame that was mask-projected (paired with `action: mask_applied`) |

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

### Rule CG-DL-8: AuditRepo cross-repo column-list drift

**Trigger:** Any PR that modifies `internal/store/audit_repo.go` in the telemetry repo.

**Check:** The Go `AuditEntry` struct and `queryAuditInsert` SQL in `audit_repo.go` mirror the Prisma `AuditLog` model in the Next.js repo (`../react-frontend/prisma/schema.prisma`). The two MUST stay in lock-step. Specifically:

1. The `CROSS-REPO COUPLING` header comment block in `internal/store/audit_repo.go` MUST be present (it tells future engineers where the schema authority lives).
2. Every column named in §4.1 (and in the Prisma model) MUST appear as a quoted identifier in `audit_repo.go` — `"id"`, `"userId"`, `"timestamp"`, `"action"`, `"targetType"`, `"targetId"`, `"initiator"`, `"metadata"`, `"createdAt"`. A missing column reference is a column-list drift signal: either a column was removed from Prisma (in which case the schema doc must be updated) or the Go side was not updated alongside a Prisma change (in which case both must be updated in the same PR).
3. If a Prisma migration adds, renames, or removes a column on `AuditLog`, the same PR MUST update `internal/store/audit_repo.go` (or, more precisely, the cross-repo coupling MUST be acknowledged by a same-PR Go-side update, even if the Go column list is intentionally narrower in some hypothetical future).

CI enforcement lives in `.github/workflows/contract-guard.yml` under the step "Rule CG-DL-8 — AuditRepo cross-repo coupling intact". The check fires only when `internal/store/audit_repo.go` is in the PR diff.

**Violation examples:**
- Removing the `CROSS-REPO COUPLING` header comment from `audit_repo.go` (loses the pointer to the Prisma authority).
- Renaming a column in Prisma without updating the corresponding column literal in `queryAuditInsert`.
- Adding a new column to Prisma without adding it to `AuditEntry` and `queryAuditInsert`.

**Fix:** Restore the cross-repo coupling header comment and ensure every Prisma `AuditLog` column appears (as a quoted identifier) in `internal/store/audit_repo.go`. If the Prisma side has not been updated yet, hold this PR until the cross-repo PR is merged (or open them as a coordinated pair).

### Rule CG-DL-9: Go migration SQL must not reference Prisma-owned tables

**Trigger:** Any PR that adds or modifies files in `internal/store/migrations/*.sql`.

**Check:** No SQL file in `internal/store/migrations/` may reference a Prisma-owned table name. The prohibited table names are (case-insensitive):

`User`, `Account`, `Session`, `VerificationToken`, `Vehicle`, `Drive`, `TripStop`, `Invite`, `Settings`, `AuditLog`

Referencing a Prisma-owned table in a Go migration file risks schema drift, accidental schema mutation during automated startup, and loss of Prisma ownership over the table's lifecycle. The Go migration toolchain is scoped exclusively to the `_telemetry_*` and `go_*` namespaces.

**Go-owned table naming convention:** All tables created by Go migrations MUST be prefixed `_telemetry_` or `go_`. This makes ownership visible in `psql \dt` output and allows `prisma db pull` filtering.

See `docs/architecture/migrations.md` §4 for the full coexistence rule and table list.

**Violation examples:**
- `ALTER TABLE "Vehicle" ADD COLUMN ...` in a migration file — Prisma owns `Vehicle`
- `INSERT INTO "AuditLog" ...` in a migration file — application runtime queries, not migration SQL, handle AuditLog inserts
- `CREATE INDEX ON "Drive" ...` in a migration file — Prisma owns `Drive`

**Fix:** Replace Prisma table references with Go-owned table names (prefixed `_telemetry_` or `go_`). If the intent is to add an index or constraint to a Prisma-owned table, coordinate with the Next.js repo's Prisma migration instead.

CI enforcement lives in `.github/workflows/contract-guard.yml` under the step "Rule CG-DL-9 — No Prisma table refs in Go migrations".

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
