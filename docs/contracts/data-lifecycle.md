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
| `model` | DB | -- | **Prisma (setup) OR Go store (VIN-derived, MYR-507)** | Static vehicle metadata with TWO writers, which is the whole story. Prisma's web-link flow fills it; the Go server's own provisioning INSERT did not — it seeded `''` under a comment promising the web sync would fill it later, and for a car linked through the Go path nothing ever did, so the column stayed empty permanently. MYR-507 derives it from **position 4 of the VIN** (`internal/vin`) at link time and backfills existing rows via the narrow §1.4 carve-out (`store.VehicleRepo.FillVehicleIdentity`) on the same non-waking read that carries the colour write. **FILL-IF-EMPTY, never overwrite** — Prisma stays authoritative wherever it wrote first, because it records a richer value for some cars (`Model 3 Performance`) than a VIN position can encode (`Model 3`). Same column, same type, same masks, **no contract change** — only the provenance. No real-time SoT: v1 pushes no WebSocket delta. |
| `year` | DB | -- | **Prisma (setup) OR Go store (VIN-derived, MYR-507)** | Static vehicle metadata; same two writers, same placeholder gap (seeded `0`) and same fill-if-empty rule as `model` above. Derived from **position 10 of the VIN**, the ISO 3779 model-year code — and unlike `model` there is no alternative source at all: **Tesla's Fleet API carries no model year on any endpoint, field or proto**, so the VIN is the platform's only one. No real-time SoT. |
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

These tables have no WebSocket representation. They are managed primarily by the Next.js app's Prisma layer, with a small set of narrowly-scoped, sanctioned exceptions where the Go telemetry server owns a specific owner-facing flow: `Account`, which the Go server reads/writes for OAuth token management; `AuditLog`, which the Go server writes (Insert-only) via raw pgx; the owner-onboarding **provision** carve-out (`store.OwnerProvisioner`, MYR-257) that upserts `User`/`Settings`/`Account`/`Vehicle` on a completed Tesla link; and its exact inverse, the owner-offboarding **teardown** carve-out (`store.OwnerTeardown`, MYR-258) that owner-scoped **DELETEs** a single `Vehicle` (+ its cascade) plus the Go-owned `go_ride_requests` rows for that vehicle (P1 encrypted pickup/dropoff GPS + passenger PII — no FK cascade reaches them, so a complete removal deletes them explicitly) and, on the owner's last vehicle, clears the `Account` tokens + resets `Settings`, writing the user-initiated `vehicle_deleted` audit row in the same transaction. Since **MYR-261** the teardown also writes a **removed-vehicle tombstone** into the Go-owned `go_removed_vehicles` table in that same transaction (see §1.4.1); since **MYR-286**, the owner **license-plate** carve-out (`store.VehicleRepo.UpdateLicensePlate`) that performs a **single-column owner-scoped UPDATE** of `Vehicle.licensePlate` and nothing else; and, since **MYR-320**, the **exterior-colour** carve-out (`store.VehicleRepo.UpdateVehicleColor`, [`internal/store/vehicle_color.go`](../../internal/store/vehicle_color.go)) that likewise performs a **single-column owner-scoped UPDATE** of `Vehicle.color` and nothing else, from Tesla's `vehicle_data.vehicle_config.exterior_color`; since **MYR-507**, the **vehicle-identity** carve-out (`store.VehicleRepo.FillVehicleIdentity`, [`internal/store/vehicle_identity.go`](../../internal/store/vehicle_identity.go)) that performs a **two-column owner-scoped fill-if-empty UPDATE** of `Vehicle.model` and `Vehicle.year` and nothing else, from the VIN rather than from Tesla; and, since **MYR-355**, the **account-deletion** carve-out (`store.AccountDeleter`, [`internal/store/account_deletion_identity.go`](../../internal/store/account_deletion_identity.go)) — the only one that **DELETEs the `User` row itself**, described in full below. Since **MYR-581**, the **profile-name** carve-out (`store.ProfileNameRepo.UpdateUserName`, [`internal/store/user_profile_name.go`](../../internal/store/user_profile_name.go)) performs a **single-column self-scoped UPDATE** of `User.name` and nothing else — the storage half of `PATCH /api/users/me`, the one profile write the platform permits (client decision reversing MYR-366's no-rename rule). All seven carve-outs are gated by `sdk-architect` + `contract-guard` and a cross-repo schema-verification against the Next.js Prisma source.

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

**Vehicle-identity carve-out (MYR-507) — scope and rationale.** `Vehicle.model` and `Vehicle.year` repeat the colour's story with one instructive difference. Like `color`, both columns have existed for the whole life of the schema, are already on the wire to both roles, and are `required` in [`schemas/vehicle-summary.schema.json`](schemas/vehicle-summary.schema.json). Unlike `color`, they are **not un-populated everywhere** — the Next.js Prisma web-link flow does fill them, which is exactly why the gap survived so long: the fleet looked half-correct. The Go server's own provisioning INSERT seeded `''` and `0` under a comment asserting that "the web sync / streaming pipeline fills real values later", and **no such writer has ever existed on either side**, so every car linked through the Go path carried the placeholders permanently. The field report is what that costs: a shared car sat in production for hours reporting `model: ""` and `year: 0`, so a rider's Live Activity could name it only by its colour (`"UltraRed"`) and the review sheet fell back to the bare make (`"Tesla"`).

The difference from the colour: **the source is the VIN, not Tesla.** Tesla's Fleet API carries no model year at any path, so there is nothing to read for `year` even in principle; and once the year comes from the VIN, the model comes from the same 17 characters for free, so there is ONE derivation over ONE input rather than a year rule here and a `vehicle_config.car_type` rule elsewhere. That has a property no API read has — the VIN needs no token, no awake car and no network call — which is why the primary fix is at **provisioning**: a car is correctly identified from the instant it is linked, rather than waiting on a connectivity edge it may never produce. The refresh-path write below exists to repair the cars already in the table.

- **Two columns.** `SET "model" = CASE …, "year" = CASE …, "updatedAt" = NOW()`. No telemetry column and no other identity column is touched.
- **FILL-IF-EMPTY, NOT OVERWRITE**, with the two arms independent (a car may have a model and no year). This is the one rule that distinguishes it from the colour write, and it is not caution for its own sake: Prisma writes a richer model for some rows (`Model 3 Performance`) than position 4 of a VIN can encode (`Model 3`), so a blind overwrite would **downgrade** those rows — trading a visible bug for a subtler one. Prisma stays authoritative wherever it wrote first.
- **A genuine no-op once filled.** The `WHERE` clause repeats the emptiness test, so a car whose columns are both populated matches **zero rows** and takes no `updatedAt` bump. That is load-bearing rather than tidy: the caller runs on every connectivity edge for every car in the fleet, and without it each pass would touch every `Vehicle` row and stream a pointless change to the Prisma side forever.
- **Owner-scoped in SQL.** `WHERE "vin" = $3 AND "userId" = $4` — identical to the colour and plate writes, so a mismatched user updates zero rows rather than another owner's car.
- **UPDATE only, and an UNRECOGNISED VIN is NEVER written.** `internal/vin` returns `""` / `0` for any position it does not recognise, and both are treated as "nothing to say" rather than as a clear — the same never-fabricate-never-regress discipline as the colour. The server does not guess at a model code it has not seen: a wrong model on a rider's ride card is worse than a blank one.
- **No audit row.** Non-destructive refresh of a factual attribute of the owner's own car; CG-DL-3 does not apply.
- **No migration.** Both columns already exist in the Prisma schema, `NOT NULL` with the placeholders above. **CG-DL-9 does not fire** for the same reason as the colour: it constrains Go *migration SQL*, and this ships none.

Both values are **P0** ([`data-classification.md`](data-classification.md) §1.3) and **log-safe in full** — equipment facts about a car, the same tier as the `color` they sit beside and correlating to no person. **The provenance change is the only change to `model` and `year` themselves** — same type, same nullability, same masks. MYR-507 *does* carry a contract change, but it is a different field: the new optional `VehicleSummary.trimLabel` (contracts **v0.31.0**), which puts the display-safe trim on the catalog because a viewer never fetches a `/snapshot`.

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
| `AuditLog` | DB-only | **Insert-only** (raw pgx) | Prisma-owned schema. **Since MYR-355 the Go server initiates the FR-10.1 account deletion and writes the `account_deleted` row itself**, inside the same transaction as the identity delete (§3.1 step 10). The Go telemetry server holds Insert-only access via raw pgx for system-initiated rows — `drives_pruned` (NFR-3.27 pruning job, §5), `mask_applied` (1% sampling, §4.2 / [`rest-api.md`](rest-api.md) §5.3), `tokens_refreshed` (OAuth refresh) — **and, since MYR-258, the ONE user-initiated row it owns: `vehicle_deleted`**, written by `store.OwnerTeardown` inside the same transaction as the owner-scoped `Vehicle` delete (CG-DL-3 requires the audit BEFORE the delete). `targetType='vehicle'`, `initiator='user'`, `metadata={driveCount, wasLastVehicle, tombstoned}` — P0 counts/flags only (CG-DL-5). Since **MYR-261** the Go server also owns the user-initiated `vehicle_readd_allowed` row (written by `store.RemovedVehicleRegistry.ClearTombstone` when an owner deliberately re-adds a previously removed car; `targetType='vehicle'`, `targetId`=Tesla vehicle id, `initiator='user'`, `metadata={existed}`). UPDATE/DELETE remain prohibited at the database level (§4.3 triggers) and the application level (no `UpdateAuditLog` / `DeleteAuditLog` methods exist; `contract-guard` CG-DL-2 enforces this on every PR). |

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
| `go_ride_requests` | **1 year rolling window on TERMINAL rides** | 365 days from `updated_at`, for rows in `status IN ('completed','declined','cancelled')` only | Background pruning job (Section 5A). On deletion the outcome is party-dependent: an OWNER deletion deletes every ride on each of their cars (§3.1 step 3), a RIDER deletion cancels their open rides and KEEPS their terminal ones as counterparty records (§3.3.1) — so for a deleted rider the sweep is the only thing that ever removes them. **Open rides are never swept, at any age** (§2.2.1). | **MYR-447** |
| `go_ride_requests.passenger_name` / `passenger_phone` | **30 days from terminal**, shorter than the parent row | 30 days from `updated_at`, for rows in `status IN ('completed','declined','cancelled')` only | Columns NULLed in place by the same job (Section 5A); the row survives. Deprecated — the book-for-someone-else feature was removed in [MYR-382](https://linear.app/myrobotaxi/issue/MYR-382) | **MYR-447** |
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

### 2.2.1 go_ride_requests — 1 year rolling window on terminal rides (MYR-447)

- A ride is eligible for pruning when its `status` is one of `completed`, `declined`, `cancelled` **and** its `updated_at` is older than 365 days.
- **Open rides are never pruned, at any age.** `requested`, `accepted`, `enroute` and `arrived` are excluded by the claim predicate, which enumerates the three terminal statuses POSITIVELY rather than as `NOT IN (open)` — so adding a lifecycle state is a deliberate edit rather than a silent widening of what gets destroyed.
- **There is no `expired` status.** A reservation whose dispatch failed carries `dispatch_error = 'reservation_expired'` and remains `accepted` ([`rest-api.md`](rest-api.md) §7.8: the owner and rider may still cancel or proceed manually). Such a ride is OPEN and is never swept.
- **The clock is `updated_at`, not a per-row terminal timestamp, because there isn't one.** `completed_at` is stamped only on entry to `completed`; `declined` and `cancelled` stamp nothing of their own. What every transition does stamp is `updated_at = NOW()` (one shared clause, `rideRequestStatusStamp`). That makes `updated_at` a **lower bound** on age-since-terminal: `updated_at < NOW() - 365d` implies the ride went terminal at least 365 days ago. The error is one-directional — a row edited after going terminal is retained LONGER, never deleted sooner. A `terminated_at` column was rejected (nothing honest to backfill from, and a second authority on "when did this end"), as was `COALESCE(completed_at, updated_at)` (strictly worse — on the completed arm it deletes SOONER, mixes two column semantics in one boundary, and no single index serves it).
- **The passenger columns go earlier than the row: 30 days.** See §2.2.2.
- `go_live_activities` rows for a pruned ride are destroyed with it, via the `ON DELETE CASCADE` on `go_live_activities.ride_request_id` (migration 0025) — the only FK pointing at this table.
- **OPEN rides have no retention ceiling at any age, and that is a real bound rather than an oversight.** The claim predicate selects terminal rows only, so a ride stuck in `requested` / `accepted` / `enroute` / `arrived` is never swept, however old it gets — it keeps its encrypted pickup/dropoff coordinates indefinitely. The same shape as a stuck-open `Drive`, which is why `cmd/cleanup-stuck-drives` exists; there is no equivalent for rides yet. Sweeping open rides on age alone was rejected because "old" and "abandoned" are not the same thing here — a scheduled reservation is legitimately future-dated, and a dispatch-expired one deliberately stays `accepted` so both parties can still act on it (§7.8). Closing this properly needs a staleness rule that can tell an abandoned ride from a live one, not a longer window.
- **On user-initiated deletion (FR-10.1) the outcome depends on WHICH party deleted, and neither arm is a caller-scoped delete of "the caller's rides".** If the deleted user is the vehicle **owner**, every ride booked against each of their cars is deleted outright by the per-vehicle teardown (§1.4, §3.1 step 3: `DELETE FROM go_ride_requests WHERE vehicle_id = $1`). If the deleted user is the **rider**, their OPEN rides are cancelled (§3.1 step 6) and their TERMINAL rides are **kept, whole and unmodified** — they are counterparty records belonging as much to the car's owner, per §3.3.1, and `rider_id` deliberately still holds the deleted user's cuid.
- **For a deleted rider, this 365-day window is therefore the only mechanism that ever removes their ride history.** That is the strongest argument for the window existing: without it, §3.3.1's counterparty carve-out would mean "forever".

### 2.2.2 go_ride_requests passenger columns — 30 days from terminal (MYR-447)

`passenger_name` and `passenger_phone` are the only columns on this table whose retention is shorter than their row's, and the reason is that they have no reader. The book-for-someone-else feature was removed from the app in [MYR-382](https://linear.app/myrobotaxi/issue/MYR-382); no live surface renders a terminal ride's passenger; nothing but the REST projection reads the columns, and both JSON keys are `omitempty` so a NULL simply removes the key. Data with no reader should not wait out the full record window — 30 days is a support window's grace on a feature that is already gone.

- Both columns are NULLed in place at terminal + 30 days. The row survives whole.
- **They are always scrubbed TOGETHER, never phone alone.** The iOS client renders the passenger block as `"\(passenger.phone) · …"` behind a single `if let passenger` whose presence test is the NAME (an absent-or-empty name maps to "no passenger at all"). Clearing the phone but leaving the name does not remove the passenger — it renders the block with a leading blank. Only clearing both makes the branch disappear.
- The scrub does **not** touch `updated_at`. That column is the 365-day boundary above, so bumping it would defer the row's own deletion by another full year.
- **Legacy backlog:** `cmd/scrub-passenger-fields` clears both columns across the whole table, once, with no age window and no status filter — including open rides, which the sweeper would never reach. Run it at deploy. It emits the same `ride_passengers_scrubbed` action as the sweeper, grouped one row per (owner, vehicle), written and confirmed BEFORE the first column is cleared and aborting the run if that write fails — so the mass scrub is provable afterwards rather than resting on somebody's terminal scrollback. `metadata.source` is `one_time_backfill`, which is what distinguishes it from the sweeper's rows. The binary REFUSES a real run with no audit writer; a `-dry-run` changes nothing and needs none.

### 2.3 AuditLog — indefinite retention (NFR-3.29)

- Audit log rows are never deleted, never updated.
- The AuditLog table is append-only (enforced by database-level policy — see Section 4.3).
- Even when the user who triggered the audited action is deleted, the AuditLog entry remains. The `userId` becomes an orphaned reference (no FK constraint to User — by design, so cascading User deletion does not destroy audit history).

#### 2.3.1 Retention after account deletion — decision record ([MYR-447](https://linear.app/myrobotaxi/issue/MYR-447), 2026-08-07)

A privacy cold-read raised that "the permanent audit log survives account deletion" and asked for one of two remedies: **anonymize on deletion** (rewrite the identifier to a tombstone hash) or **delete at deletion + 90 days**. **Neither was adopted.** Both were evaluated against CG-DL-3's purpose and both fail, for different reasons. The stance below is the decision; it is recorded here rather than in a commit message because it is the answer a future reader will look for.

**Delete at deletion + 90d — rejected.** CG-DL-3 exists to produce a durable artifact proving an erasure happened. A 90-day window destroys exactly that artifact, and destroys it precisely when it becomes useful: a person disputing months later whether their deletion was honoured is the case the row is for. It also requires `DELETE` on an append-only table, which the §4.3 triggers refuse at the database level and `contract-guard` CG-DL-2 refuses at PR time.

**Tombstone-hash the identifier — rejected, and the reason is not squeamishness about the invariant.** `userId` is a cuid. An **unkeyed** hash of a known-format identifier buys no anonymity at all: anyone holding the cuid from an external source — an old export, a support ticket, an application log — computes the same digest and looks the row up exactly as before. A **keyed** hash forces a choice with no good arm: retain the key and the linkability is retained with it, or discard the key and the row can no longer be tied to the person whose deletion it proves, which is CG-DL-3's purpose destroyed by a slower route. Separately, applying it in place means `UPDATE` on the append-only table, and the mechanism that can rewrite `userId` on a deleted user's rows can rewrite `initiator` or `action` on anyone's. **Trading tamper-evidence for pseudonymity is a net loss here**, because the thing being pseudonymized is already a bare orphaned cuid.

**Adopted: indefinite retention of a minimized, already-orphaned record — with the pseudonymity made a tested property rather than an assertion.** What actually survives a deletion is a cuid that, by the time it survives, resolves in none of the three identity sources (§4.5) — plus enum values, timestamps and integer counts. No email, no name, no Apple sub, no VIN, no coordinate, no token; §4.4 classifies every column P0 and CG-DL-5 constrains `metadata` to P0. The exposure that remains is not "the audit log holds personal data", it is "someone who already knows a cuid can see that its owner deleted their account" — which is the minimum the deletion proof can cost. MYR-447 adds a conformance test over the audit writers asserting the P0-only metadata property directly, so the claim the privacy page makes is enforced rather than merely documented.

**What would change this.** Adopting in-place anonymization is a coherent position, but it is a policy decision with a security cost attached and it is not free to build: it needs a Prisma-side amendment to the §4.3 triggers carving out a single narrowly-scoped statement, a matching amendment to CG-DL-2, and an anonymizer that is itself audited. It should be commissioned deliberately, not arrived at as a side effect. It is out of scope for MYR-447.

---

## 3. Deletion cascade for FR-10.1

When a user requests deletion of their account (FR-10.1), the system MUST delete all user data and write an immutable audit log entry (FR-10.2).

> **REWRITTEN BY [MYR-355](https://linear.app/myrobotaxi/issue/MYR-355) (2026-07-30).** This section previously specified a single Prisma `$transaction` initiated by the Next.js app, and stated that "the telemetry server does not initiate account deletions". Both are superseded. The **Go telemetry server** owns `DELETE /api/users/me` ([`rest-api.md`](rest-api.md) §7.6) because the native iOS client is the only consumer and never reaches the Next.js app — and because the `User` cascade was never the whole deletion: `go_ride_requests`, `go_vehicle_shares`, `go_push_devices`, `go_refresh_tokens`, `go_users`, `go_identity_apple` and `go_removed_vehicles` carry no FK to `User` (CG-DL-9 forbids one), so no Prisma cascade has ever reached a single one of them.
>
> **The consequence is that the deletion is NOT one transaction, and the contract's guarantee changes shape.** §3.4 below states the new one.

### 3.1 Deletion ordering

The deletion is a SEQUENCE of independently-atomic steps, executed in this order by `telemetry.AccountDeletionHandler` (`internal/telemetry/account_deletion_sequence.go`) over `store.OwnerTeardown` and `store.AccountDeleter`:

**Every step below is keyed on the DELETION SCOPE, not on the caller's JWT subject** ([MYR-452](https://linear.app/myrobotaxi/issue/MYR-452)). Step 0 resolves that scope; steps 1–9 each run once per id in it, and step 10 takes the whole set at once. See §3.1.1 for why the subject alone is not a safe key.

| # | Step | Writer | Idempotent because |
|---|------|--------|--------------------|
| 0 | **Resolve the caller's subject to its identity closure** (MYR-452) | `AccountDeleter.ResolveDeletionScope` | Read-only. **Fatal on error** — unlike step 1, a half-resolved scope is how a deletion silently misses its target and reports success |
| 1 | Count the user's drives (audit metadata only) | `AccountDeleter.CountUserDrives` | Read-only; a failure is logged and ignored — a missing statistic must never block erasure |
| 2 | **Revoke the Tesla OAuth grant AT TESLA** (MYR-366) | `telemetry.TeslaLinkRevoker` → `POST https://auth.tesla.com/oauth2/v3/revoke` | A re-run finds no `Account` row, so there is no token to present and the step skips without calling Tesla |
| 3 | For EACH owned vehicle: the §1.4 owner-teardown transaction | `OwnerTeardown.RemoveVehicle` | An already-removed car returns `AlreadyGone` — a clean no-op success with no duplicate audit row |
| 4 | Revoke every grant the user REDEEMED | `AccountDeleter.RevokeSharesReceived` | `WHERE accepted_by_user_id = $1 AND status <> 'revoked'` matches nothing on a re-run |
| 5 | **Scrub the owner-typed `label` off those same redeemed grants** (MYR-447) | `AccountDeleter.ScrubSharesReceivedLabel` | `WHERE accepted_by_user_id = $1 AND label <> ''` matches nothing on a re-run |
| 6 | Cancel every OPEN ride the user holds as RIDER | the guarded §7.8 transition + `ride_status_changed` publish | The guarded `UPDATE … WHERE status = ANY(from)` cannot re-fire; a lost race is not an error |
| 7 | Delete the user's push devices | `AccountDeleter.DeletePushDevices` | `DELETE … WHERE user_id = $1` affects zero rows on a re-run |
| 8 | Delete the user's saved places (MYR-321) | `AccountDeleter.DeleteSavedPlaces` | `DELETE … WHERE user_id = $1` affects zero rows on a re-run |
| 9 | Revoke the user's refresh tokens | `AccountDeleter.RevokeRefreshTokens` | `WHERE user_id = $1 AND revoked = FALSE` matches nothing on a re-run |
| 10 | Identity + audit, ONE transaction | `AccountDeleter.DeleteIdentity` | The transaction probes the three identity sources first; finding none it commits empty and writes NO audit row |
| 11 | Invalidate the auth caches | `auth.JWTAuthenticator` | Pure cache eviction |

#### 3.1.1 The deletion scope (step 0) — normative

`DELETE /api/users/me` authenticates a JWT and gets back a subject. **That subject is not reliably the id the account is filed under**, and keying the teardown on it was [MYR-452](https://linear.app/myrobotaxi/issue/MYR-452).

`store.OwnerProvisioner.rebindApple` re-points `go_identity_apple.user_id` onto a canonical Prisma `"User"` id whenever a Tesla link proves the caller and an existing owner are the same person (§ self-serve-onboarding.md, resolution paths (a) and (b)). Nothing re-issues the caller's tokens when that happens, so a converged owner keeps presenting the **pre-convergence** id for the life of their refresh family. Keyed on it, every step targeted an id that owned nothing: the binding survived, the `"User"` row and its cascades survived, and the endpoint still wrote its audit row and answered 204. The next Sign in with Apple found the surviving binding and returned the account.

The scope is therefore resolved FIRST and every subsequent step runs over it:

- **`go_identity_convergence(from_user_id, to_user_id)`** records each re-point, written in the SAME transaction as the re-point itself. It is deliberately NOT a column on `go_users`: the re-pointed id is whatever the caller's token names, and `identity.linkage` mints bindings directly onto Prisma `"User"` ids (verified-email match) and onto configured cuids (bootstrap override) without creating a `go_users` row at all. A `go_users` column would record nothing for exactly those callers.
- **The walk is transitive, undirected and cycle-safe.** Chains form when a person converges twice; 2-cycles form because `Account.userId` is never rewritten, so re-linking the same Tesla under the new canonical id converges back the other way.
- **`CanonicalID` is the closure member holding the Apple binding**, not the target of the edges — the edge direction is not well defined inside a cycle. The `account_deleted` audit row is filed against it.
- **The closure is capped at 8 ids and exceeding it FAILS the deletion.** An edge is an unconditional grant of DELETE authority over another id, so the closure size is the blast radius of one Delete tap. A 500 is recoverable; deleting a stranger's account is not.
- **An edge is only written when a binding actually moved.** `rebindApple` checks the re-point's affected-row count first. The evidence that two ids are the same human IS the binding moving; without it the caller held no binding — which happens whenever a caller presenting an already-converged subject links a SECOND Tesla owned by somebody else — and writing an edge would aim their next deletion at that stranger's account.
- **A recorded edge is never silently re-targeted.** The upsert refreshes the timestamp only when the target is identical.
- **The closure is re-walked inside step 10's transaction.** If it GREW, the transaction aborts with a retryable error and the whole sequence re-runs from step 0 — merging the newcomer in at step 10 would delete its identity rows while its push devices, saved places, grants and tokens were never touched, since no table carries an FK to `go_users` (CG-DL-9) and nothing cascades. A closure that SHRANK is not an error. `CanonicalID` is recomputed inside the transaction, because the binding can move within an unchanged closure.
- **Two live binding owners in one closure, with neither being the caller, is refused** on the same reasoning as the cap: it is a stronger "whose account is this?" signal, at a smaller number.

**Operability.** A refusal means a person cannot delete their own account until the graph is repaired, which is a privacy-commitment failure, not a routine 500. It emits the dedicated event `account_deletion_scope_unresolved` at ERROR with the caller id. Repair is manual against `go_identity_convergence` today; an `ops` subcommand and a runbook entry are outstanding.

**Step 2 must precede step 3, and this ordering is normative too** ([MYR-366](https://linear.app/myrobotaxi/issue/MYR-366)). The revoke call presents the stored `refresh_token`; step 3's last-vehicle arm DELETEs the `Account` row that holds it, and step 10 deletes any row that survived. After either, the credential the revocation needs no longer exists and only the owner can withdraw the grant by hand from the consent page. Revoking first is the only ordering in which the server can do it at all.

**Step 2 is BEST-EFFORT and its failure is NOT an error.** Every failure mode — no `Account` row, a database read error, a network error, a Tesla 5xx, an already-invalid token — is logged at WARN and the sequence continues. Tesla's availability MUST NOT be able to block a person's erasure of their own account, so the step has no error path a caller could propagate. It is skipped entirely when no Tesla OAuth `client_id` is configured. The step writes **no `AuditLog` row**: it records a P0 structured log line `event=tesla_tokens_revoked` carrying the `user_id` and nothing else — never the token, its prefix, its length, or a VIN.

**Step 5 is separate from step 4 deliberately, and the row sets differ.** Step 4 tombstones the ACCESS; step 5 erases the NAME. `go_vehicle_shares.label` is free text the CAR OWNER typed for their own list — "Mira Chen", "Mom", "Roommate" (data-classification.md §1.15) — so on a grant the deleted user REDEEMED, the label is that person's name held in somebody else's row. Before [MYR-447](https://linear.app/myrobotaxi/issue/MYR-447) it survived the deletion indefinitely, keyed by a cuid that resolves to nothing. Folding the scrub into step 4 would not have been equivalent: step 4 is guarded by `status <> 'revoked'` for idempotency and therefore skips grants that were ALREADY revoked — by the owner, or by an earlier partial run of this very sequence — whose labels are exactly as stale and exactly as much this person's PII. Step 5 is keyed on the person alone. It scrubs to `''` rather than NULL because the column is `TEXT NOT NULL` and `store.CreateInvite` rejects a blank at the door, which makes the empty string an unambiguous scrubbed-here sentinel no live row can hold. **No wire effect:** a revoked grant is never serialized to any client (`status` has no `revoked` wire member) and the label was owner-facing only — it was never delivered to the invited party.

**What step 5 does NOT reach.** Grants where the deleted user is the OWNER rather than the redeemer carry OTHER people's names, and those are counterparty records that stay (§3.3). Separately, the owner-side revocation in step 3 is keyed on `vehicle_id`, so a grant whose vehicle was removed in an earlier session is reached by neither step 3 nor step 5 and keeps both its label and, if still `pending`, a redeemable `code`. That orphan gap predates MYR-447 and is not closed by it.

**Step 10 is the only transaction that deletes identity, and it runs LAST. That ordering is normative**, because the caller authenticates with a token that resolves through exactly those rows: deleting them earlier would leave a half-deleted account that nobody — not even its owner — could finish deleting.

Step 10, in full (CG-DL-3 requires the audit BEFORE the destructive delete):

```
BEGIN TRANSACTION;

-- Step 1: Write audit log FIRST (before the destructive deletes in this tx)
INSERT INTO "AuditLog" ("id", "userId", "timestamp", "action", "targetType", "targetId", "initiator", "metadata")
VALUES (
  cuid(),
  '<canonical-id>',
  NOW(),
  'account_deleted',
  'user',
  '<canonical-id>',
  'user',
  '{"vehicleCount": N, "driveCount": M, "inviteCount": K}'
);

-- Step 2: Delete the identity rows. Every statement is keyed on the SCOPE
-- (§3.1.1), re-walked inside this transaction, because the caller's subject is
-- not reliably the id the rows are filed under.
--
-- An Apple-native user has no "User" row and a legacy web user has no go_users
-- row; ALL statements run unconditionally and simply affect zero rows in the
-- case that does not apply (dual-source identity — neither case is
-- special-cased, neither is an error).
DELETE FROM go_identity_apple WHERE user_id = ANY('<scope>');

-- The stored OAuth grants and Settings go EXPLICITLY, not via the Prisma
-- cascade (MYR-452). The cascade exists and works; the point is that it is
-- defined in the Next.js app's schema, in another repository, and a deletion
-- guarantee about live fleet-control credentials must not rest on a constraint
-- this server neither owns, migrates, nor tests. These statements are redundant
-- today and deliberately so: they make the guarantee local and testable.
DELETE FROM "Account"  WHERE "userId" = ANY('<scope>');
DELETE FROM "Settings" WHERE "userId" = ANY('<scope>');

-- The convergence edges themselves: once the ids they connect are gone, an
-- edge is a dangling grant of delete authority over a cuid resolving to nothing.
DELETE FROM go_identity_convergence
  WHERE from_user_id = ANY('<scope>') OR to_user_id = ANY('<scope>');

DELETE FROM go_users WHERE id = ANY('<scope>');

-- Step 3: Delete the Prisma User row IF one exists — its remaining cascades are
-- the BACKSTOP, not the mechanism: by now step 3 of §3.1 has already torn down
-- every owned vehicle one transaction at a time, so the cascade normally has
-- nothing left to take.
DELETE FROM "User" WHERE "id" = ANY('<scope>');

-- Prisma onDelete: Cascade propagation (automatic) — Account and Settings are
-- no longer left to it, see above:
--   User delete  -> Vehicle[]      (all vehicles owned by this user)
--   User delete  -> Invite[]       (all invites SENT by this user)
--
--   Vehicle delete -> Drive[]      (all drive history for this vehicle)
--   Vehicle delete -> TripStop[]   (all trip stops for this vehicle)
--   Vehicle delete -> Invite[]     (all invites TO this vehicle)

COMMIT;

-- Sessions are already gone before this transaction opens: step 9 of §3.1
-- revoked every go_refresh_tokens row, and step 11 evicts the user-existence
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
- Step 6 publishes `ride_status_changed`, which sends **push notifications**. A notification cannot be rolled back.

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

> **Ownership.** The `AuditLog` table is part of the **Next.js app's Prisma schema** (consistent with the §1.4 Prisma-managed-table list and [`rest-api.md`](rest-api.md) §10 DV-23, RESOLVED 2026-05-08, MYR-69). Migrations are authored in the Next.js repo via Prisma; the Go telemetry server does NOT own the migration toolchain for this table. **Since MYR-355 the Go telemetry server writes the `account_deleted` row (FR-10.1)**, inside the same transaction as the identity delete defined in §3.1 step 10, and owns the deletion sequence per §3.4 — it serves `DELETE /api/users/me`, so it owns that audit row, by the same rule that gave it `vehicle_deleted`. The Go telemetry server holds **Insert-only** access via raw pgx for the system-initiated rows (`drives_pruned`, `mask_applied`, `tokens_refreshed`) and — since MYR-258 — the user-initiated `vehicle_deleted` row, which `store.OwnerTeardown` writes inside the same transaction as the owner-scoped per-vehicle delete it backs (`DELETE /api/tesla/vehicles/{vehicleId}`; the Go server owns that endpoint, so it owns that audit row). UPDATE and DELETE are prohibited at both the database level (§4.3 triggers) and the application level (`contract-guard` CG-DL-2). The schema below is the canonical definition that both the Prisma model and the Go pgx writer MUST mirror exactly; drift between them is a contract violation.

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
| `account_deleted` | User account and all associated data deleted (FR-10.1). **Emitted by the Go telemetry server since MYR-355** — `store.AccountDeleter.DeleteIdentity`, in the same transaction as the identity delete (§3.1 step 10, CG-DL-3). `targetType='user'`, `targetId`=the caller's own cuid, `initiator='user'`, `metadata={vehicleCount, driveCount, ridesCancelled, sharesRevoked, shareLabelsScrubbed, pushDevicesDeleted, savedPlacesDeleted, refreshTokensRevoked, hadPrismaUser}` — P0 counts/flags only (CG-DL-5). Written at most ONCE per account: the transaction probes the three identity sources first and the already-gone arm writes nothing, so the endpoint's re-run path cannot duplicate it | User (FR-10.1) |
| `vehicle_deleted` | Single vehicle and its drives/stops/invites deleted. Since MYR-261 the same-tx write also creates a `go_removed_vehicles` tombstone (§1.4.1); `metadata.tombstoned` records whether one was written | User |
| `vehicle_readd_allowed` | Owner deliberately re-added a previously removed car — the `go_removed_vehicles` tombstone for `(userId, teslaVehicleId)` was cleared so the next Tesla sync may provision the VIN again (MYR-261, §1.4.1). `targetType='vehicle'`, `targetId` is the Tesla vehicle id, `initiator='user'`, `metadata={existed}` (P0 only). Emitted by `store.RemovedVehicleRegistry.ClearTombstone` in the same transaction as the tombstone DELETE (CG-DL-3) | User |
| `drives_pruned` | Batch of drives older than 365 days deleted | System pruning job (NFR-3.27) |
| `rides_pruned` | Batch of TERMINAL ride requests (`completed` / `declined` / `cancelled`) older than 365 days deleted ([MYR-447](https://linear.app/myrobotaxi/issue/MYR-447), §5A). Written by `store.RidePruner.PruneBatch` **inside the same transaction as the DELETE and before it** (CG-DL-3). `targetType='ride_request'`, `targetId`=the **vehicle** cuid the rides were booked against — not a ride id, which no longer exists by the time anyone reads the row — `initiator='system_pruner'`, `metadata={rideCount, oldestRideDate, newestRideDate}`, P0 counts and an opaque window only (CG-DL-5). One row per (owner, vehicle) group per batch. **`userId` is the vehicle OWNER; the rider gets no rider-keyed row** — see §5A.4 for why that is a decision rather than an inheritance | System pruning job (MYR-447) |
| `ride_passengers_scrubbed` | Batch of SURVIVING terminal rides whose deprecated `passenger_name` and `passenger_phone` columns were NULLed at terminal + 30 days ([MYR-447](https://linear.app/myrobotaxi/issue/MYR-447), §2.2.2). A separate action from `rides_pruned` because it is a separate promise: one says a record was destroyed, the other says a record survives with two fields removed, and collapsing them would make the log unable to answer which happened. Same emitter, same transaction-before-write ordering, same grouping and same `metadata` shape as `rides_pruned` | System pruning job (MYR-447) |
| `drive_deleted` | Single drive record deleted | User |
| `invite_revoked` | Sharing invite revoked | User |
| `tokens_refreshed` | OAuth tokens rotated | System (token refresh) |
| `mask_applied` | Role-based field mask removed at least one field from a REST response or WebSocket broadcast (sampled at 1%) | System (broadcast / handler layer); see [`rest-api.md`](rest-api.md) §5.3 |
| `operator_decrypt` | An internal operator tool DECRYPTED user data (MYR-447). Emitted by `store.OperatorAuditor.RecordDecrypt` from the `ops` CLI, one row per invocation of a decrypting subcommand: `ops auth token` (Tesla access + refresh token), `ops fields snapshot` (the whole encrypted location/nav surface of a Vehicle row), `ops fleet-config push` (both of the above), and `ops geocode backfill` (every drive trail in the fleet that is missing an address, plus the sealed start/end labels). **The geocode backfill is the one emitter whose rows are GROUP-SCOPED rather than one-per-invocation** — it writes one row per (owner, vehicle) over the set it decrypted, because a fleet-wide backfill would otherwise flood an append-only table with a row per drive. **This is the only action in the enum that records a READ rather than a mutation** — nothing about the subject's data changed, only who saw it. `initiator` MUST be `operator`; `targetType` is `user` (targetId = the subject's cuid) when the material hangs off the account, or `vehicle` (targetId = the **Vehicle cuid, never the VIN** — §4.4 columns are P0 and a full VIN is restricted outside the owner's own snapshot per [`data-classification.md`](data-classification.md) §1.3/§2.1). `metadata` shape is exactly `{command, operator, fields, fieldCount}` — field NAMES and counts only, never a decrypted value (CG-DL-5). **Fail-closed ordering:** the row is written BEFORE the plaintext is printed or transmitted, and a failed insert aborts the command — the same posture CG-DL-3 requires of deletions | Operator (internal tooling; attributed by the required `OPS_OPERATOR` handle) |
| `data_exported` | User-initiated portability export of every Prisma row owned by the caller (GDPR Art. 15 right of access / Art. 20 portability). Emitted by the Next.js `GET /api/users/me/export` handler ([Phase A: myrobotaxi/react-frontend#259](https://github.com/myrobotaxi/react-frontend/pull/259); MYR-75). One row per export — sampling 100% (not high-volume); retained indefinitely per NFR-3.29. `targetType` MUST be `user`, `targetId` MUST be the caller's `userId`, `initiator` MUST be `user`. `metadata` shape is exactly `{vehicleCount, driveCount, inviteCount, auditCount}` — P0 counts only per Rule CG-DL-5; never PII, GPS, addresses, or tokens. See [`rest-api.md`](rest-api.md) §7.7. | User (caller-initiated portability export per GDPR Art. 15 / Art. 20) |

**`targetType` values:**

| Target type | Description |
|-------------|-------------|
| `user` | A User record |
| `vehicle` | A Vehicle record |
| `drive` | A Drive record (or batch of drives) |
| `ride_request` | A `go_ride_requests` record (or batch of them) — paired with `action: rides_pruned` or `ride_passengers_scrubbed` (MYR-447). Like `drive`, the `targetId` beside it is the **vehicle** cuid rather than a ride id: the row records a batch grouped by the car, and for `rides_pruned` the ride ids it covers no longer exist. First `go_*`-owned target type in this enum |
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
| `operator` | Action initiated by a human operator running internal tooling (MYR-447). Distinct from `user` (the data subject acting on their own data) and from the `system_*` initiators (unattended jobs). The operator is NOT the `userId` column — `userId` is the DATA SUBJECT throughout this table, which is what makes "everything that touched this person's data" one indexed lookup and what lets the row survive the subject's deletion (§4.5). The actor's handle rides in `metadata.operator` |

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

**Operator handle (MYR-447).** `metadata.operator` on an `operator_decrypt` row carries the invoking operator's handle from the required `OPS_OPERATOR` environment variable. It is **P0**, and the write path enforces the shape that makes that true: `store.ValidateOperatorHandle` accepts `^[A-Za-z0-9][A-Za-z0-9._-]*$` up to 64 characters and therefore **rejects an email address** (no `@`). A directory handle like `jdoe` is an internal lookup key that resolves to a person only inside the company — the same character as the opaque cuids in the neighbouring columns. A work email is a routable personal identifier and is P1 by the same reasoning that makes `User.email` P1 in [`data-classification.md`](data-classification.md) §1.1; admitting one would quietly demote the whole table. Rejecting the email form at the write site, with an error that says why, is cheaper than discovering P1 in an append-only table that cannot be corrected by UPDATE or DELETE (§4.3).

The same guard covers `metadata.fields`: entries must match a bare field identifier (`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)*$`, ≤ 64 chars), which structurally excludes the value shapes CG-DL-5 forbids — a coordinate (`37.7749`) and an address (`1600 Amphitheatre Pkwy`) fail the leading-letter rule, a Tesla JWT fails the length bound. CG-DL-5 stops being a convention reviewers must remember and becomes a precondition callers cannot violate.

### 4.5 No FK to User (intentional design decision)

The `AuditLog.userId` column is **not** a foreign key to the User table. This is intentional:

- When a user is deleted (FR-10.1), the audit log entry recording that deletion must survive. A cascading FK would destroy the audit trail.
- The `userId` value becomes an orphaned reference after account deletion. This is acceptable because the audit log's purpose is to prove that data was deleted, not to reconstruct the user's profile.
- Queries against the audit log use `userId` as a filter, not a join target.

---

## 5. Pruning job spec (NFR-3.27)

> **IMPLEMENTED by [MYR-439](https://linear.app/myrobotaxi/issue/MYR-439) (2026-08-02).** This section was a specification with no implementation behind it from Phase 1 until now — the policy existed and nothing enforced it, which is the [MYR-427](https://linear.app/myrobotaxi/issue/MYR-427) privacy-audit finding that prompted the work. It now describes shipped behaviour. Where the as-built differs from the original spec, the difference is called out inline and marked **AS-BUILT**.
>
> **No data was ever over-retained.** The gap was an unenforced policy, not a violated one: production holds 315 `Drive` rows as of 2026-08-02, the oldest created **2026-03-08**, so nothing has yet reached 365 days. The platform is younger than its own retention window.
>
> **⚠️ THE SWEEP DELETES NOTHING UNTIL 2027-03-08.** Every pass before that date claims zero rows and exits. This matters to whoever is reading in March 2027: the audit grouping, the `SKIP LOCKED` contention path, and Postgres TOAST reclamation of the deleted route blobs will have run **only against the test suite** up to that point, never against production data. The first pass that actually deletes something is the real first run — watch `telemetry_pruner_drives_deleted_total` and the batch-error counter that night.
>
> Code: `store.DrivePruner` (`internal/store/drive_pruner.go`) is the batch; `retention.Pruner` (`internal/retention/`) is the cadence, budget and retries; `startDrivePruner` (`cmd/telemetry-server/wiring_retention.go`) is the composition root.
>
> **The 365-day window is `store.DriveRetentionDays`, and that is the only place it is written.** Compile-time constant, unreachable from configuration per CG-DL-4. The `DRIVE_RETENTION_PRUNER_ENABLED` kill-switch stops the sweep; it cannot change the window.

### 5.1 Purpose

A background job that enforces the 1-year rolling retention window for Drive records. Drives with `createdAt` older than 365 days are deleted in batches.

**Scope of one deletion.** `DELETE FROM "Drive"` is the whole job. No table in either the Prisma schema or the `go_*` namespace is keyed by a drive id — there is no drive-keyed sidecar, derivative, summary or blob table, and a Go migration could not declare an FK to `Drive` even if one were wanted (CG-DL-9). The route/GPS trail is two columns ON the drive row (`routePoints` plaintext, `routePointsEnc` ciphertext), so the row delete destroys ciphertext and plaintext together and leaves no orphan. The pruner's statements therefore **name no column**, which is also what makes them invariant under [MYR-433](https://linear.app/myrobotaxi/issue/MYR-433)'s retirement of the plaintext copy.

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

> **AS-BUILT — the index is NOT shipped with MYR-439, and cannot be.** `"Drive"` is Prisma-owned and CG-DL-9 names `CREATE INDEX ON "Drive"` in Go migration SQL as a violation, so this index has to land as a Prisma migration in `myrobotaxi/react-frontend`. Tracked as **[MYR-442](https://linear.app/myrobotaxi/issue/MYR-442)**.
>
> **Hard deadline: 2027-03-08** — the date the first drive becomes eligible and the claim query starts doing real work. This is not a "before the table grows" nice-to-have with an open end. Until then the claim scans `createdAt` and returns zero rows, which is affordable at any size; from that date forward every 03:00 UTC pass runs the range scan for real, and the audit grouping resolves an owner per batch. The index must be in place before that pass, not after it.

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

> **AS-BUILT — open drives are IN SCOPE, deliberately.** The claim filters on `createdAt` alone and does **not** exclude rows with a null/empty `endTime`. This is a decision, not an oversight: a drive still "open" 365 days after it was created is corrupt state, not an in-progress trip (the longest real drive is bounded by `MaxDriveDuration`, and `cmd/cleanup-stuck-drives` exists precisely because stuck-open rows happen). Deleting it is the correct outcome. If the detector somehow still held a reference, the failure is loud rather than silent — `Complete` / `AppendRoutePoints` return `ErrDriveNotFound`, which the writer logs as a warning and steps over.

> **AS-BUILT — `Exhausted` requires an EMPTY claim, not a short one.** §5.4's step 2 says "IF no rows returned → job complete", and that is exactly what shipped, but it is worth stating why the obvious optimisation is wrong. A batch that returns fewer than `LIMIT` rows looks done, and under `SKIP LOCKED` it is not: a short claim means "no more rows *I* can take", which a peer holding the remainder also satisfies. Treating that as completion would let one replica declare the backlog drained over rows a dying peer is about to roll back — and advance the freshness gauge on work nobody did. The loop therefore always pays one extra empty claim to confirm.
>
> **It narrows the ambiguity rather than removing it, and the earlier wording overstated this.** An empty claim still means only "no rows *I* can take": a peer holding the entire remainder satisfies that too, so a replica can report a complete pass over rows it never saw. The residual is bounded and accepted — the pass is idempotent, so the next night re-claims anything a peer rolled back, which makes the worst case a one-day delay on a 365-day window. Closing it properly would need cross-replica coordination that costs more than it buys.

### 5.5 Batch configuration

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| Batch size | 100 drives | Balances transaction size with throughput. Large enough for efficiency, small enough to avoid long-held locks. |
| Audit granularity | One audit entry per batch per vehicle owner | Groups pruned drives by owner for readable audit history |
| Iteration limit | **AS-BUILT: 500 batches per pass** (was: none) | 50,000 drives a night. **This is deliberate headroom, not a response to an existing backlog — there is no backlog** (315 rows, nothing eligible until 2027-03-08). An unbounded "runs until no eligible drives remain" loop is a ceiling nobody set; adding one now costs a constant, whereas adding it after a pathological pass has pinned a connection until morning costs an incident. The budget is a ceiling, not a throttle, and a pass that hits it logs a warning naming the cap. Nothing is lost: the claim is always oldest-first, so the next pass resumes exactly where this one stopped. |
| **AS-BUILT: concurrency** | `FOR UPDATE OF d SKIP LOCKED` on the claim | Replaces §5.8's leader election with something that needs no coordinator. Two replicas waking at 03:00 claim disjoint batches; a replica that dies mid-batch releases its claim on rollback. Correct here because the work set is unordered with respect to correctness — any expired drive may be deleted by anyone. |
| **AS-BUILT: transaction boundary** | Claim, audit and delete are ONE transaction | §5.4's pseudocode SELECTs before `BEGIN`. Folding the claim inside the transaction is what makes `SKIP LOCKED` meaningful and removes the window in which a claimed id could be deleted by another path before this one's audit row was written. |

### 5.6 Failure handling

| Scenario | Behavior |
|----------|----------|
| Batch transaction fails | Retry with exponential backoff, **3 attempts max**. **AS-BUILT: the waits are 1s then 2s**, not "1s, 2s, 4s" — three attempts have only two gaps between them, so a 4s wait would imply a fourth attempt the same row forbids. Cancellation during a backoff aborts immediately rather than burning the remaining attempts. |
| All 3 retries fail for a batch | **AS-BUILT: log at `slog.Error`, increment `telemetry_pruner_batch_errors_total`, and END THE PASS** (was: skip to next batch). "Skip to next batch" is not expressible: the claim is deterministic — always the oldest expired rows — so the "next" batch after a failure is the SAME batch, and continuing would spin. Ending the pass is the honest form of the same intent, and costs nothing because the sweep is idempotent and the next daily run retries from the same position. |
| Database connection lost | Abort the job. Next daily run will pick up where this one left off (idempotent — only deletes drives older than 365 days). |
| Audit log insert fails | The entire batch transaction rolls back. No drives are deleted without an audit trail. |
| Job takes longer than expected | **AS-BUILT: bounded three ways** — a 30s timeout on each batch transaction, a 500-batch ceiling per pass, and cancellation honoured at every batch boundary. There is still no timeout on the pass as a whole; the budget is what bounds it. |
| **AS-BUILT: process shutdown mid-pass** | The pass stops at the next batch boundary. Each batch is its own transaction, so the one in flight rolls back whole — there is no partially-pruned state and nothing to drain. The sweep is context-only on shutdown, matching every other timer-driven worker in the process. |

### 5.7 Observability

The pruning job emits the following metrics:

**AS-BUILT:** all five ship, carrying the `telemetry_` prefix this service uses for every metric, and the gauge carries the `_seconds` unit suffix Prometheus convention requires.

| Metric | Type | Description |
|--------|------|-------------|
| `telemetry_pruner_drives_deleted_total` | Counter | Total drives deleted across all batches, cumulative. **Expect a flat zero until 2027-03-08** — that is the correct reading, not a stalled sweep. Distinguish "nothing to do" from "not running" with the freshness gauge below, never with this counter. |
| `telemetry_pruner_batches_processed_total` | Counter | Number of batches committed |
| `telemetry_pruner_batch_errors_total` | Counter | Number of batch failures (after retries). **Alert on any increase.** |
| `telemetry_pruner_run_duration_seconds` | Histogram | Wall-clock time for one pass |
| `telemetry_pruner_last_success_timestamp_seconds` | Gauge | Unix timestamp of the last pass that completed without a batch error. **Alert when `now() - this > ~48h`:** the window is a privacy commitment, so a silently stalled sweep is a compliance problem, not a missed cron. A budget-capped pass still advances this gauge — it made progress; only a failed pass leaves it stale. **This is the sweep's ONLY liveness signal** for as long as the deleted-drives counter is legitimately zero (i.e. until 2027-03-08), so it carries the whole alert. **Seeding:** the gauge is published as `0` at process start rather than left absent, so the staleness expression evaluates from boot instead of silently matching no series — meaning it reads as *firing* from startup until the first 03:00 UTC pass. That is intended (a process that never reaches 03:00 is exactly the failure being caught); damp the startup window with an alert `for:` longer than a day, **not** by special-casing zero. |

### 5.8 Deployment

The pruning job runs as a scheduled task within the telemetry server process (not a separate service). On Fly.io, this is implemented as a goroutine with a `time.Ticker` that fires daily at 03:00 UTC. The job is leader-elected if multiple instances are running (only one instance executes the prune).

> **AS-BUILT — no leader election.** The goroutine ships as described (a `time.Timer` re-armed to the next 03:00 UTC slot, plus up to five minutes of jitter so replicas do not stampede the pool), but there is **no leader**. `SKIP LOCKED` on the claim makes concurrent replicas safe by construction: they take disjoint batches and the work simply finishes sooner. A leader election would have added a coordinator, a lease, and a failure mode where the leader dies and nothing prunes until the lease expires — all to serialise work that is already safe to parallelise.
>
> **Kill-switch:** `DRIVE_RETENTION_PRUNER_ENABLED` (defaults ON, fail-fast on a non-boolean value). Turning it off is not a neutral degraded mode — the 365-day window is a promise made to owners in the privacy policy, so the sweep being off means that promise is unmet for as long as it stays off. It exists for stopping a misbehaving sweep, not for deferring one.

### 5.9 Read-path behaviour after a prune (AS-BUILT)

A drive id that a client still holds — a cached `drive_ended` WebSocket payload, a deep link, a stale page — may refer to a drive that has since been pruned. Every path degrades cleanly; **none errors**:

| Path | Behaviour on a pruned id |
|------|--------------------------|
| `GET /api/drives/{driveId}` (§7.3) | `404 not_found` via the ordinary `store.ErrDriveNotFound` → `sdk.ErrNotFound` chain. Returned *before* the ownership check, so a pruned drive 404s rather than 403s. |
| `GET /api/drives/{driveId}/route` (§7.4) | Same `404 not_found`. |
| `GET /api/vehicles/{vehicleId}/drives` (§7.2) | The row is simply absent from the page. `items` is built as an empty slice, so a fully-pruned history serialises as `[]`, never `null`. The 404 on this endpoint means a missing *vehicle*, never a missing drive. |
| Cursor pagination (§7.2) | Safe. The keyset predicate `("startTime","id") < ($2,$3)` is a pure value comparison and never looks the anchor row up, so a cursor pointing at a pruned drive still returns the correct next page — it cannot 400 or 500. |
| Ride → drive joins | **None exist.** The MYR-265 drive/ride correlation was removed by MYR-270; `go_ride_requests` has no drive-id column. The only join involving `"Drive"` anywhere is `Drive → Vehicle`. |
| `AuditLog.targetId` | Never a drive id — every emitter passes the vehicle id, including the `drives_pruned` row itself. No audit row can dangle. |

**One caveat on §7.2's "disappearing from the tail".** The prune orders by `createdAt` (a `timestamptz`) while the list orders by `startTime` (a Prisma `String`, compared lexicographically). For rows with clock skew or a malformed `startTime` the two orders can diverge, so a pruned row may vanish from the **middle** of a cursor scan rather than its tail. The effect is still only a gap in a page — no error, no cursor corruption — but the guarantee is "items may disappear from a scan", not specifically from its tail.

---

## 5A. Ride pruning job spec (MYR-447)

> **IMPLEMENTED by [MYR-447](https://linear.app/myrobotaxi/issue/MYR-447).** Unlike §5,
> this section was never a specification first — `go_ride_requests` had no retention
> policy at all, and terminal rides accumulated indefinitely. It describes shipped
> behaviour from the start.
>
> Code: `store.RidePruner` (`internal/store/ride_pruner.go`,
> `ride_pruner_queries.go`, `ride_pruner_audit.go`) is the batch;
> `retention.RidePruner` (`internal/retention/ride_pruner.go`) is the cadence, budget
> and retries — **the same engine as §5's drive sweep**, generalized over its batch
> outcome in `internal/retention/sweeper.go`; `startRidePruner`
> (`cmd/telemetry-server/wiring_retention_rides.go`) is the composition root.
>
> **The 365-day window is `store.RideRetentionDays` and the 30-day passenger window is
> `store.RidePassengerScrubDays`, and those are the only places they are written.**
> Compile-time constants, unreachable from configuration per CG-DL-4. The
> `RIDE_RETENTION_PRUNER_ENABLED` kill-switch stops the sweep; it cannot change either
> window.

### 5A.1 Purpose

Enforce two windows on `go_ride_requests` (§2.2.1, §2.2.2): delete terminal rides past
365 days, and NULL the deprecated booked-for-passenger columns on terminal rides past
30 days.

**Scope of one deletion.** `DELETE FROM go_ride_requests` takes the row whole — both
encrypted coordinate pairs, both place labels, both street addresses, and the two
passenger columns. `go_live_activities.ride_request_id` is the only FK pointing at this
table (`ON DELETE CASCADE`, migration 0025), so the delete also destroys any Live
Activity sidecar rows for the pruned rides and takes row locks in that table. That is
correct — an Activity for a ride deleted a year after it ended is dead weight — but it
is why the batch stays small.

**Scope of one scrub.** Exactly two columns, always both, and never `updated_at` (see
§2.2.2).

### 5A.2 Schedule

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| Schedule | Daily at **03:00 UTC**, ±5 min jitter | Same slot and same jitter as §5.2 — the two sweeps share the engine, and a second slot would be a second thing to know |
| Frequency | Once per day | Terminal-ride accrual does not justify more |
| Timezone | UTC | Server operates in UTC |

### 5A.3 Index

**Shipped with the job** — unlike §5.3, which cannot be, because `"Drive"` is
Prisma-owned. `go_ride_requests` is Go-owned, so migration
`0030_ride_retention_index.up.sql` lands with the sweeper:

```sql
CREATE INDEX IF NOT EXISTS idx_go_ride_requests_retention
    ON go_ride_requests (updated_at)
    WHERE status IN ('completed', 'declined', 'cancelled');
```

One index serves both claims: the predicate is exactly their terminal-status conjunct,
and the `updated_at` key serves both the age range (as an Index Cond) and the
`ORDER BY updated_at ASC` (as a forward ordered scan the LIMIT short-circuits). The
scrub's extra `passenger_name IS NOT NULL OR passenger_phone IS NOT NULL` arm stays a
cheap recheck — those columns are NULL on essentially every row.

**Every existing partial index on this table is partial on the OPEN statuses** — 0004,
0013, 0016 and 0028 — i.e. the exact complement of this predicate, so not one of them
contains a single row the sweep wants.

> **NOT SELF-DRAINING, unlike migration 0028.** 0028's index is partial on the open
> statuses, so a row LEAVES it when its ride ends and the indexed set tracks concurrent
> live rides. This one is the mirror image: a row ENTERS when the ride ends and stays
> until deletion, so the indexed set tracks **lifetime** terminal ride volume and grows
> monotonically. **What bounds it is the sweeper it exists to serve.** The index is only
> affordable because the job runs; if the sweep is disabled long-term, this index grows
> with the table and should be reconsidered along with everything else that decision
> breaks.

### 5A.4 Execution

```
FOR each batch, in ONE transaction:
  1. Claim: SELECT id, vehicle_id, owner_id, updated_at
            FROM go_ride_requests
            WHERE updated_at < NOW() - make_interval(days => 365)
              AND status IN ('completed','declined','cancelled')
            ORDER BY updated_at ASC LIMIT 100
            FOR UPDATE SKIP LOCKED

  2. IF the claim is non-empty:
       INSERT one 'rides_pruned' AuditLog row per (owner, vehicle) group  -- BEFORE the delete
       DELETE FROM go_ride_requests
        WHERE id = ANY($1)
          AND updated_at < NOW() - make_interval(days => 365)
          AND status IN ('completed','declined','cancelled')

  3. Claim: the same shape at 30 days, plus
              AND (passenger_name IS NOT NULL OR passenger_phone IS NOT NULL)

  4. IF that claim is non-empty:
       INSERT one 'ride_passengers_scrubbed' AuditLog row per (owner, vehicle) group
       UPDATE go_ride_requests
          SET passenger_name = NULL, passenger_phone = NULL
        WHERE id = ANY($1)
          AND updated_at < NOW() - make_interval(days => 30)
          AND status IN ('completed','declined','cancelled')

  5. COMMIT. Exhausted = (both claims were empty).
```

> **BOTH GUARDS ARE REPEATED in the DELETE and the UPDATE**, not merely trusted from the
> claim. §5.4's drive equivalent repeats its age predicate for the same reason — the
> boundary is asserted at the point of destruction — but the stakes are higher here,
> because `go_ride_requests` also holds LIVE rides. A mis-scoped delete would destroy a
> ride someone is currently taking, not merely over-delete history.

> **THE DELETE RUNS FIRST, and the ordering is not incidental.** The scrub's target set
> is a superset of the delete's by age, so scrubbing first would rewrite rows about to
> be destroyed and emit a `ride_passengers_scrubbed` audit row about data that no
> longer exists.

> **`Exhausted` requires BOTH claims to be EMPTY, not short.** Same reasoning as §5.4's
> AS-BUILT note: under `SKIP LOCKED` a short claim means "no more rows *I* can take",
> which a peer holding the remainder also satisfies. The loop pays one extra empty pair
> of claims, and that **narrows the ambiguity without removing it**. An empty claim is
> the same statement in a stronger form — "there are no rows *I* can take" — which a
> peer holding the ENTIRE remainder also satisfies. This replica can therefore still end
> a pass reporting completion over rows it never saw, and if that peer's transaction
> then rolls back, the rows come back unpruned after the freshness signal already moved.
> Closing that last gap needs cross-replica coordination the sweep deliberately does not
> have: the cost of the residual case is bounded — the pass is idempotent and runs again
> the next night, so a premature "exhausted" delays rows by a day rather than losing
> them — and a lock or a leader election to buy that day back would be a far larger
> standing liability than the thing it fixes.

> **AUDIT GROUPING IS BY VEHICLE, `userId` = the vehicle OWNER — a choice with a named
> cost.** A ride has two parties. The row deleted is as much the RIDER's record as the
> owner's, and the rider gets **no rider-keyed audit row** for its destruction: an
> owner's subject-access request over `AuditLog` surfaces the deletion, a rider's does
> not. Two rows per group was rejected because doubling audit volume to record the same
> batch twice makes the log harder to read for the case it is actually used for
> (reconstructing what the sweep did), and because rider-side accountability is already
> served by the deletion being unconditional and contract-documented rather than
> discretionary — there is no per-rider decision here for an audit row to hold to
> account. If a rider-keyed view is ever required, the honest fix is a second row keyed
> by `rider_id`, not a re-reading of this one.

### 5A.5 Batch configuration

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| Batch size | 100 rides, per job | Small enough that the locks one transaction holds — on `go_ride_requests` AND, via the 0025 cascade, on `go_live_activities` — are never interesting to a concurrent reader |
| Audit granularity | One entry per batch per (owner, vehicle), per action | Same as §5.5; up to two audit rows per group per batch, one per job |
| Iteration limit | 500 batches per pass | Shared default with §5.5 |
| Concurrency | `FOR UPDATE SKIP LOCKED` on both claims | No leader election, same as §5.8's AS-BUILT note |
| Transaction boundary | Both claims, both audits and both writes are ONE transaction | A scrub failure rolls back the delete; both are retried together |

### 5A.6 Failure handling

Identical to §5.6 — same engine, same three attempts at 1s/2s, same
end-the-pass-on-exhausted-retries, same 30s per-batch timeout, same
stop-at-the-next-boundary on shutdown.

### 5A.7 Observability

**Distinct metric names from §5.7.** Both collector sets register into the same
registry in the same process, and a duplicate metric name is a panic at
`MustRegister` — a crash loop on boot, not a bad dashboard.

| Metric | Type | Description |
|--------|------|-------------|
| `telemetry_ride_pruner_rides_deleted_total` | Counter | Terminal rides deleted, cumulative. Expect zero for the first year of the table's life; a flat line before then is correct, not a stalled sweep |
| `telemetry_ride_pruner_passengers_scrubbed_total` | Counter | Rows whose two passenger columns were NULLed, cumulative. **In steady state this should sit at zero** — the feature was removed in MYR-382, so a sustained non-zero rate means something is still WRITING those columns and wants finding |
| `telemetry_ride_pruner_batches_processed_total` | Counter | Batches committed |
| `telemetry_ride_pruner_batch_errors_total` | Counter | Batches that failed every attempt. **Alert on any increase.** |
| `telemetry_ride_pruner_run_duration_seconds` | Histogram | Wall-clock time for one pass |
| `telemetry_ride_pruner_last_success_timestamp_seconds` | Gauge | Unix timestamp of the last pass without a batch error. **Alert when `now() - this > ~48h`.** Seeded to `0` at startup rather than left absent, for the same reason as §5.7's gauge, and damped the same way (an alert `for:` longer than a day, never by special-casing zero) |

### 5A.8 Deployment

A goroutine in the telemetry server process, no separate service and **no leader** —
`SKIP LOCKED` makes concurrent replicas safe by construction.

> **Kill-switch:** `RIDE_RETENTION_PRUNER_ENABLED` (defaults ON, fail-fast on a
> non-boolean value). **Deliberately separate from `DRIVE_RETENTION_PRUNER_ENABLED`:**
> the two sweeps share an engine but touch different tables, and stopping one because
> it is misbehaving must not suspend the other's promise. Turning this one off suspends
> two commitments at once — the 365-day record window and the removal of a deleted
> feature's PII.

### 5A.9 Read-path behaviour after a prune

| Path | Behaviour on a pruned ride id |
|------|-------------------------------|
| `GET /api/ride-requests/{id}` (§7.8) | `404 not_found` via the ordinary `store.ErrRideRequestNotFound` chain |
| Rider / owner list endpoints | The row is simply absent from the page; `items` is an empty slice, never `null` |
| Cursor pagination | Safe — the keyset predicate is a pure value comparison and never looks the anchor row up |
| `go_live_activities` | Cascaded away with the ride (migration 0025). No dangling Activity row can survive its ride |
| `AuditLog.targetId` | Never a ride id — every emitter passes the vehicle id, including the `rides_pruned` row itself |

**On a SCRUBBED (not pruned) ride:** `passengerName` and `passengerPhone` are both
`omitempty` on the wire, so a NULL simply removes the keys and the client sees a ride
with no booked-for passenger — which is the same shape every ride created since
MYR-382 already has.

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
