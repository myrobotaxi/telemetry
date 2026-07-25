# Data Classification Contract

**Status:** Draft — v1
**Target artifact:** Classification reference table
**Owner:** `sdk-architect` agent + `security` agent

## Purpose

Labels every persisted field with a classification tier — **P0**, **P1**, or **P2** — driving logging redaction rules, encryption-at-rest boundaries, access-log requirements, and role-mask visibility. This contract is consulted by `contract-guard` on every PR that adds or modifies a persisted field.

## Classification tiers (per NFR-3.9)

- **P0 (Public)** — may appear in logs, no encryption required. Examples: VIN last-4, vehicle name, vehicle model.
- **P1 (Sensitive, encrypted at rest)** — AES-256-GCM column-level encryption. Never in logs. Examples: GPS coordinates, destination data, OAuth tokens.
- **P2 (Sensitive + access-logged)** — P1 requirements plus every read/write writes an access-log entry. Reserved for future fields (e.g., payment info, health data).

## Anchored requirements

- **NFR-3.9** — tier definitions
- **NFR-3.22** — TLS in transit for all connections
- **NFR-3.23** — AES-256-GCM column-level encryption for P1 fields (OAuth tokens, GPS coordinates, destination coordinates, route points)
- **NFR-3.24** — encryption key stored as Fly.io secret (`ENCRYPTION_KEY`)
- **NFR-3.25** — encryption transparent to SDK (server store layer only)
- **NFR-3.26** — key rotation strategy (separate contract doc)

---

## 1. Per-field classification tables

Every column in every persisted table is listed below. The **Tier** column is the authoritative classification. The **Encrypt** column indicates whether AES-256-GCM column-level encryption is required at rest. The **Log-safe** column indicates whether the value may appear in structured logs, error messages, or crash reports.

### 1.1 User table (Prisma-owned)

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `name` | `String?` | P1 | No | No | PII — user's display name |
| `email` | `String?` | P1 | No | No | PII — user's email address (FR-11.2) |
| `emailVerified` | `DateTime?` | P0 | No | Yes | Timestamp only, no PII |
| `image` | `String?` | P1 | No | No | User avatar URL — links to identity |
| `createdAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |
| `updatedAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |

> **Note:** The User table is Prisma-owned (NextAuth). The telemetry server reads `userId` as a foreign key but does not directly query the User table. Encryption of User columns is the responsibility of the Next.js app layer. Classifications here establish the contract for any future telemetry-server access.

### 1.2 Account table (Prisma-owned — NextAuth)

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `userId` | `String` | P0 | No | Yes | FK to User — opaque identifier |
| `type` | `String` | P0 | No | Yes | OAuth account type descriptor |
| `provider` | `String` | P0 | No | Yes | OAuth provider name (e.g., `tesla`) |
| `providerAccountId` | `String` | P0 | No | Yes | Opaque, provider-scoped ID (Tesla returns non-correlatable ID). Reclassify to P1 if a future provider exposes cross-service correlatable IDs |
| `refresh_token` | `Text?` | P1 | **Yes** | No | OAuth credential — NFR-3.23 |
| `access_token` | `Text?` | P1 | **Yes** | No | OAuth credential — NFR-3.23 |
| `expires_at` | `Int?` | P0 | No | Yes | Token expiry epoch — no secret material |
| `token_type` | `String?` | P0 | No | Yes | OAuth token type descriptor |
| `scope` | `String?` | P0 | No | Yes | OAuth scope string — public metadata |
| `id_token` | `Text?` | P1 | **Yes** | No | Contains user identity claims (JWT) |
| `session_state` | `String?` | P0 | No | Yes | OAuth session state parameter |

> **Note:** The telemetry server reads `access_token` and `refresh_token` via `AccountRepo.GetTeslaToken()` and writes refreshed tokens via `AccountRepo.UpdateTeslaToken()`. AES-256-GCM encryption for these columns is applied in the store layer per NFR-3.23/NFR-3.25.
>
> **Dual-write transition (MYR-62, 2026-05-09):** The Account table now carries `access_token_enc`, `refresh_token_enc`, and `id_token_enc` (`Text?` ciphertext columns) alongside the legacy plaintext columns. The store layer encrypts on write into both columns and prefers `<col>_enc` on read, falling back to plaintext when ciphertext is NULL. The plaintext columns survive the rollout so a roll-back to a pre-encryption binary can still service traffic; they are scheduled for removal in a separate post-rollout migration once `account_token_plaintext_remaining_total` reaches zero across all three columns. See `cmd/backfill-account-tokens/` for the legacy-row migration tool.

### 1.3 Vehicle table

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `userId` | `String` | P0 | No | Yes | FK to User — opaque identifier |
| `teslaVehicleId` | `String?` | P0 | No | Yes | Tesla-assigned vehicle ID — opaque |
| `vin` | `String?` | P0 | No | **Last-4 only** | Publicly visible on vehicle exterior; P1 encryption would be overkill for a value stamped on the car. Risk is mitigated by mandatory `redactVIN()` redaction to `***XXXX` in all logs (see §2.1 VIN redaction rule) |
| `name` | `String` | P0 | No | Yes | User-assigned vehicle name |
| `model` | `String` | P0 | No | Yes | Vehicle model (e.g., "Model 3") |
| `year` | `Int` | P0 | No | Yes | Model year |
| `color` | `String` | P0 | No | Yes | Vehicle color |
| `licensePlate` | `String` | P1 | No | No | Can be used to look up registered owner — PII |
| `chargeLevel` | `Int` | P0 | No | Yes | Battery percentage — not identifying |
| `chargeState` | `String` (enum) | P0 | No | Yes | Charge state enum (`Disconnected`, `Charging`, `Complete`, …) — not identifying. Sourced from Tesla proto field **179** (`DetailedChargeState`) as of [MYR-42](https://linear.app/myrobotaxi/issue/MYR-42) (2026-04-23); MYR-40 initially sourced from proto 2 but empirical capture showed Tesla firmware ≥ 2024.44.25 does not emit proto 2, so the switch to proto 179 was non-behavioral (same 7 enum strings). Added by MYR-11 (v1 charge atomic group); source proto corrected by MYR-42. |
| `estimatedRange` | `Int` | P0 | No | Yes | Range in miles — not identifying |
| `timeToFull` | `Float` | P0 | No | Yes | **Hours (decimal)** to full charge at current rate — not identifying. Tesla proto field 43 (`TimeToFullCharge`, double). Unit per `tesla-fleet-telemetry-sme` skill + legacy Tesla REST API; empirical verification tracked as `websocket-protocol.md` §10 DV-17. Added by MYR-11 (v1 charge atomic group). |
| `status` | `VehicleStatus` | P0 | No | Yes | Enum: driving/parked/charging/offline/in_service |
| `speed` | `Int` | P0 | No | Yes | Speed in mph — not identifying without GPS |
| `gearPosition` | `String?` | P0 | No | Yes | Gear state — not identifying |
| `heading` | `Int` | P0 | No | Yes | Compass heading — not identifying without GPS |
| `locationName` | `String` | P1 | No | No | Reverse-geocoded place name — reveals location |
| `locationAddress` | `String` | P1 | No | No | Street address — reveals location |
| `latitude` | `Float` | P1 | **Yes** | No | GPS coordinate — NFR-3.23 |
| `longitude` | `Float` | P1 | **Yes** | No | GPS coordinate — NFR-3.23 |
| `interiorTemp` | `Int` | P0 | No | Yes | Cabin temperature — not identifying |
| `exteriorTemp` | `Int` | P0 | No | Yes | Ambient temperature — not identifying |
| `odometerMiles` | `Int` | P0 | No | Yes | Odometer reading — not identifying |
| `fsdMilesSinceReset` | `Float` | P0 | No | Yes | FSD miles driven since last reset — not identifying |
| `virtualKeyPaired` | `Boolean` | P0 | No | Yes | Pairing status flag |
| `setupStatus` | `SetupStatus` | P0 | No | Yes | Enum: setup lifecycle state — **Prisma-owned**, not currently accessed by the telemetry server |
| `destinationName` | `String?` | P1 | No | No | Reveals travel intent/plans |
| `destinationAddress` | `String?` | P1 | No | No | Reveals travel intent/plans |
| `destinationLatitude` | `Float?` | P1 | **Yes** | No | GPS coordinate — NFR-3.23 |
| `destinationLongitude` | `Float?` | P1 | **Yes** | No | GPS coordinate — NFR-3.23 |
| `originLatitude` | `Float?` | P1 | **Yes** | No | GPS coordinate — NFR-3.23 |
| `originLongitude` | `Float?` | P1 | **Yes** | No | GPS coordinate — NFR-3.23 |
| `etaMinutes` | `Int?` | P0 | No | Yes | Time estimate — not identifying without destination |
| `tripDistanceMiles` | `Float?` | P0 | No | Yes | Distance value — not identifying |
| `tripDistanceRemaining` | `Float?` | P0 | No | Yes | Distance value — not identifying |
| `navRouteCoordinates` | `Json?` | P1 | **Yes** | No | Full route polyline — reveals travel patterns. NFR-3.23 |
| `lastUpdated` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |
| `createdAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |
| `updatedAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |

> **Dual-write transition (MYR-63, 2026-05-09):** The six Vehicle GPS columns (`latitude`/`longitude`, `destinationLatitude`/`destinationLongitude`, `originLatitude`/`originLongitude`) gain ciphertext shadows: `latitudeEnc`, `longitudeEnc`, `destinationLatitudeEnc`, `destinationLongitudeEnc`, `originLatitudeEnc`, `originLongitudeEnc` (all `Text?`). Both the Go `VehicleRepo` and the TS helpers in `../react-frontend/src/lib/vehicle-gps-encryption.ts` dual-write plaintext + `*Enc` on every UPDATE and **prefer the `*Enc` shadow on read**, falling back to the plaintext Float when the shadow is NULL. **Atomic-pair invariant (`vehicle-state-schema.md` §3.3 GPS predicates):** a half-pair `*Enc` row (one column populated, the other NULL) is corrupt — both readers and writers treat the entire pair as plaintext-only and log a warning rather than emit/consume a half-pair. The plaintext Float columns survive the rollout so a roll-back to a pre-encryption binary can still service traffic; they are scheduled for removal in a separate post-rollout migration once `vehicle_gps_plaintext_remaining_total{column=...}` reaches zero across all six columns. See `cmd/backfill-vehicle-gps/` for the legacy-row migration tool.

> **MYR-252 cabin-control read-back fields (WS-live, not persisted):** The 21 cabin-control wire fields added by [MYR-252](https://linear.app/myrobotaxi/issue/MYR-252) — `locked`, `hvacPower`, `isClimateOn`, `fanSpeed`, `driverTempSetting`, `passengerTempSetting`, `hvacAutoMode`, `hvacAcEnabled`, `seatHeaterLeft`/`Right`, `seatHeaterRearLeft`/`Center`/`Right`, `seatCoolerLeft`/`Right`, `seatVentEnabled`, `chargePortDoorOpen`, `frunkOpen`, `trunkOpen`, `mediaPlaybackStatus`, `mediaVolume` — are **all P0**. Cabin comfort/lock/door/media state is not identifying (same reasoning as `interiorTemp`/`chargeLevel`/`gearPosition`); note that P2 (§0) is the platform's *most*-sensitive tier (payment/health), so P2 would be the wrong label here. These are **not `Vehicle` table columns** — they are delivered live on the WS `vehicle_update` stream only (owner mask allow-list; `rest-api.md` §5.2.1) and are not persisted, so they are exempt from CG-DC-5 (which governs persisted columns). DB persistence, at which point they become P0 `Vehicle` columns in the table above, is tracked in [MYR-253](https://linear.app/myrobotaxi/issue/MYR-253). Per-field definitions: `vehicle-state-schema.md` §1.1.

> **MYR-259 `ServiceMode` signal (internal-only, not persisted, not on the wire):** The `ServiceMode` telemetry field (Tesla proto **159**, `bool`) added by [MYR-259](https://linear.app/myrobotaxi/issue/MYR-259) is **P0** — like the other cabin/vehicle state above, service-mode state is operational, not identifying. Unlike the MYR-252 fields, `ServiceMode` is **not even a wire field**: it is decoded server-side, cached per-VIN, folded into the `status` derivation (`status = in_service`), and then **stripped before broadcast** — only the already-P0 `status` enum reaches clients (and the persisted `Vehicle.status` column). So `ServiceMode` is neither a `Vehicle` column nor a WS field and is exempt from CG-DC-5. Its P0 handling matters only for **log safety**: the raw signal MAY appear in logs (no redaction required), consistent with P0. The companion signal — Tesla's REST `in_service` bool read on connectivity edges — is likewise non-identifying P0 and surfaces only as the persisted `status` enum. Per-field definition + derivation: `vehicle-state-schema.md` §2.4.

### 1.4 Drive table

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `vehicleId` | `String` | P0 | No | Yes | FK to Vehicle — opaque identifier |
| `date` | `String` | P0 | No | Yes | Date string — not identifying on its own |
| `startTime` | `String` | P0 | No | Yes | ISO 8601 timestamp — not identifying on its own |
| `endTime` | `String` | P0 | No | Yes | ISO 8601 timestamp — not identifying on its own |
| `startLocation` | `String` | P1 | No | No | Reverse-geocoded place name — reveals home/work |
| `startAddress` | `String` | P1 | No | No | Street address — reveals home/work |
| `endLocation` | `String` | P1 | No | No | Reverse-geocoded place name — reveals destinations |
| `endAddress` | `String` | P1 | No | No | Street address — reveals destinations |
| `distanceMiles` | `Float` | P0 | No | Yes | Aggregate stat — not identifying |
| `durationMinutes` | `Int` | P0 | No | Yes | Aggregate stat — not identifying |
| `avgSpeedMph` | `Float` | P0 | No | Yes | Aggregate stat — not identifying |
| `maxSpeedMph` | `Float` | P0 | No | Yes | Aggregate stat — not identifying |
| `energyUsedKwh` | `Float` | P0 | No | Yes | Aggregate stat — not identifying |
| `startChargeLevel` | `Int` | P0 | No | Yes | Battery percentage — not identifying |
| `endChargeLevel` | `Int` | P0 | No | Yes | Battery percentage — not identifying |
| `fsdMiles` | `Float` | P0 | No | Yes | FSD distance — not identifying |
| `fsdPercentage` | `Float` | P0 | No | Yes | FSD ratio — not identifying |
| `interventions` | `Int` | P0 | No | Yes | Count — not identifying |
| `routePoints` | `Json` | P1 | **Yes** | No | Full GPS trail (lat/lng/speed/heading/timestamp per point) — reveals travel patterns. NFR-3.23 |
| `createdAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |

> **Dual-write transition (MYR-64, 2026-05-09):** The route-blob polylines on `Vehicle.navRouteCoordinates` (Tesla planned navigation polyline, member of the navigation atomic group) and `Drive.routePoints` (recorded breadcrumb trail) gain ciphertext shadows: `Vehicle.navRouteCoordinatesEnc` and `Drive.routePointsEnc` (both `Text?`). Both the Go store layer and the TS helpers in `../react-frontend/src/lib/route-blob-encryption.ts` dual-write plaintext + `*Enc` on every UPDATE/INSERT and **prefer the `*Enc` shadow on read**, falling back to the plaintext column on decrypt or non-array failures with a `slog.Warn` (TS: `console.warn`). Route blobs can exceed 100KB; corruption MUST NOT 500 the live nav-route view. The plaintext columns survive the rollout so a roll-back to a pre-encryption binary can still service traffic; they are scheduled for removal in a separate post-rollout migration once `route_blob_plaintext_remaining_total{column=navRouteCoordinates|routePoints}` reaches zero. See `cmd/backfill-route-blobs/` for the legacy-row migration tool. Drive append semantics: `DriveRepo.AppendRoutePoints` writes the plaintext concat first via PostgreSQL `jsonb_concat (||)` then re-encrypts the post-append array into the shadow in a second UPDATE — the shadow re-encrypt is fail-open so a key/encryption hiccup never drops Tesla telemetry.

### 1.5 Drive route point (embedded in `Drive.routePoints` JSONB)

Each element in the `routePoints` array is a `RoutePointRecord`:

| JSON key | Type | Tier | Encrypt | Log-safe | Rationale |
|----------|------|------|---------|----------|-----------|
| `lat` | `Float` | P1 | **Yes** (parent column) | No | GPS coordinate |
| `lng` | `Float` | P1 | **Yes** (parent column) | No | GPS coordinate |
| `speed` | `Float` | P0 | No (in P1 column) | No* | Not identifying, but encrypted with parent |
| `heading` | `Float` | P0 | No (in P1 column) | No* | Not identifying, but encrypted with parent |
| `timestamp` | `String` | P0 | No (in P1 column) | No* | Not identifying, but encrypted with parent |

> \*These sub-fields are P0 in isolation but are stored inside the P1 `routePoints` JSONB column, so they are encrypted at rest as a unit. They MUST NOT be logged because extracting them from context would require logging the entire route point including lat/lng.

### 1.6 Invite table (Prisma-owned)

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `vehicleId` | `String` | P0 | No | Yes | FK to Vehicle — opaque identifier |
| `senderId` | `String` | P0 | No | Yes | FK to User — opaque identifier |
| `label` | `String` | P0 | No | Yes | Display label for the invite |
| `email` | `String` | P1 | No | No | PII — invitee's email address |
| `status` | `InviteStatus` | P0 | No | Yes | Enum: pending/accepted |
| `permission` | `InvitePermission` | P0 | No | Yes | Enum: live/live_history |
| `sentDate` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |
| `acceptedDate` | `DateTime?` | P0 | No | Yes | Non-sensitive timestamp |
| `lastSeen` | `DateTime?` | P0 | No | Yes | Non-sensitive timestamp |
| `isOnline` | `Boolean` | P0 | No | Yes | Presence flag |
| `createdAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |
| `updatedAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |

> **Note:** The Invite table is Prisma-owned. The telemetry server does not currently read or write this table, but classifications are established for `contract-guard` enforcement when access is added (FR-5.x sharing features).

### 1.7 TripStop table (Prisma-owned)

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `vehicleId` | `String` | P0 | No | Yes | FK to Vehicle — opaque identifier |
| `name` | `String` | P1 | No | No | Place name — reveals location/travel intent |
| `address` | `String` | P1 | No | No | Street address — reveals location |
| `type` | `StopType` | P0 | No | Yes | Enum: charging/waypoint |

### 1.8 Settings table (Prisma-owned)

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `userId` | `String` | P0 | No | Yes | FK to User — opaque identifier |
| `teslaLinked` | `Boolean` | P0 | No | Yes | Feature flag |
| `teslaVehicleName` | `String?` | P0 | No | Yes | Tesla-reported vehicle name. May differ from the user-assigned `Vehicle.name` if the user renames the vehicle in the MyRoboTaxi app (see MYR-30). |
| `virtualKeyPaired` | `Boolean` | P0 | No | Yes | Feature flag |
| `keyPairingDeferredAt` | `DateTime?` | P0 | No | Yes | Non-sensitive timestamp |
| `keyPairingReminderCount` | `Int` | P0 | No | Yes | Counter |
| `notifyDriveStarted` | `Boolean` | P0 | No | Yes | Preference flag |
| `notifyDriveCompleted` | `Boolean` | P0 | No | Yes | Preference flag |
| `notifyChargingComplete` | `Boolean` | P0 | No | Yes | Preference flag |
| `notifyViewerJoined` | `Boolean` | P0 | No | Yes | Preference flag |
| `createdAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |
| `updatedAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |

### 1.9 go_ride_requests table (Go-owned, MYR-173)

The P10 ride-hailing aggregate (contracts `schemas/ride-request.schema.json`; FR-9.3 list-envelope conventions, NFR-3.9 tiers, NFR-3.23 encryption). First Go-owned feature table — created by `internal/store/migrations/0002_ride_requests.up.sql` under the CG-DL-9 `go_` namespace; the Next.js app's ORM never touches it.

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `TEXT` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `rider_id` | `TEXT` | P0 | No | Yes | User cuid — opaque identifier |
| `owner_id` | `TEXT` | P0 | No | Yes | User cuid — opaque identifier |
| `vehicle_id` | `TEXT` | P0 | No | Yes | Vehicle cuid — opaque identifier |
| `pickup_lat_enc` | `TEXT` (ciphertext) | P1 | **Yes** (encrypt-only) | No | GPS coordinate — NFR-3.23. New table: no plaintext column, no dual-write rollout |
| `pickup_lng_enc` | `TEXT` (ciphertext) | P1 | **Yes** (encrypt-only) | No | GPS coordinate — NFR-3.23 |
| `pickup_label` | `TEXT` | P1 | No | No | Place name — reveals location (same rationale as `Drive.startLocation`) |
| `pickup_address` | `TEXT?` | P1 | No | No | Street address — reveals location |
| `dropoff_lat_enc` | `TEXT` (ciphertext) | P1 | **Yes** (encrypt-only) | No | GPS coordinate — NFR-3.23 |
| `dropoff_lng_enc` | `TEXT` (ciphertext) | P1 | **Yes** (encrypt-only) | No | GPS coordinate — NFR-3.23 |
| `dropoff_label` | `TEXT` | P1 | No | No | Place name — reveals destination |
| `dropoff_address` | `TEXT?` | P1 | No | No | Street address — reveals destination |
| `status` | `TEXT` (enum, CHECK) | P0 | No | Yes | Lifecycle enum: requested/accepted/declined/enroute/arrived/completed/cancelled |
| `passenger_name` | `TEXT?` | P1 | No | No | PII — booked-for passenger's name |
| `passenger_phone` | `TEXT?` | P1 | No | No | PII — booked-for passenger's mobile (tracking-link SMS target). Never logged |
| `scheduled_for` | `TIMESTAMPTZ?` | P0 | No | Yes | Reservation instant — no PII without the (P1) places |
| `reschedule_proposed_for` | `TIMESTAMPTZ?` | P0 | No | Yes | Proposed new pickup instant |
| `reschedule_status` | `TEXT?` (enum, CHECK) | P0 | No | Yes | Sub-state enum: requested/confirmed/declined |
| `accepted_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Non-sensitive timestamp |
| `completed_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Non-sensitive timestamp |
| `created_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |
| `updated_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |
| `dispatch_status` | `TEXT?` (enum, CHECK) | P0 | No | Yes | Nav-dispatch outcome enum: sent/failed/skipped (MYR-176). Opaque, non-sensitive |
| `dispatched_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Nav-dispatch attempt instant + exactly-once claim latch (MYR-176) |
| `dispatch_error` | `TEXT?` | P0 | No | Yes | Opaque nav-dispatch failure CODE — constructed from the typed command error / resolution sentinel only; never a coordinate/address/token/raw VIN (Rule CG-DC-2). Not exposed on the wire. Value set: the typed command codes (`key_not_paired`, `permission_denied`, `vehicle_asleep`, `command_failed`, `invalid_request`, `internal_error`) plus the dispatch-local codes `vehicle_unresolved`, `token_expired` (linked but expired — user must re-link), `token_unavailable` (never linked / could not obtain), `transport_unconfigured` (proxy not configured), `dispatch_canceled`, `dispatch_interrupted` (claimed but the process died pre-outcome; set by the startup reconciler) (MYR-176) |
| `dropoff_dispatch_status` | `TEXT?` (enum, CHECK) | P0 | No | Yes | Leg-2 (dropoff) nav-dispatch outcome enum: sent/failed/skipped (MYR-265). Independent of the leg-1 `dispatch_status` so neither leg clobbers the other. Opaque, non-sensitive |
| `dropoff_dispatched_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Leg-2 nav-dispatch attempt instant + exactly-once claim latch (MYR-265) |
| `dropoff_dispatch_error` | `TEXT?` | P0 | No | Yes | Opaque leg-2 (dropoff) nav-dispatch failure CODE — same construction/value-set/never-a-secret rules as `dispatch_error`; not exposed on the wire (MYR-265) |
| `enroute_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Board (leg-2 start) instant, stamped when the ride enters `enroute` (MYR-265). The drive-end completer correlates the ended drive's start time against it (`enroute_at <= driveStartedAt`) so a delayed leg-1 pickup drive-end cannot false-complete the ride. Non-sensitive; not exposed on the wire |

> **Encrypt-only coordinates (no dual-write).** Unlike the MYR-63/64 rollouts, `go_ride_requests` is a brand-new table: coordinates are written exclusively as AES-256-GCM ciphertext (`internal/store/ride_request_scan.go`, reusing the `vehicle_gps_encryption.go` float codec). There are no plaintext coordinate columns, no backfill tool, and no `*_plaintext_remaining_total` gauge. `RideRequestRepo` therefore requires an `Encryptor` at construction (panics on nil) and treats decrypt failure as a hard read error — there is no fallback column to fall back to.

> **Wire surface (MYR-174).** The rider-facing REST endpoints (`rest-api.md` §7.8) return the full `RideRequest` object — including the decrypted P1 `pickup`/`dropoff` coordinates + labels and the P1 passenger name/phone — but only to a **party** (rider or vehicle owner), over TLS, transported plaintext at the crypto boundary exactly like the vehicle-GPS REST paths (NFR-3.25). The reactive WS frames (`ride_request_created` / `ride_status_changed`, `websocket-protocol.md` §4.7–4.8) are **summary-only** and deliberately carry **none** of the P1 place/passenger data: they emit the P0 ids/status/timestamps plus the P1 `riderId` (an opaque user cuid, same tier + handling as `AuthOkPayload.userId`) and the P1 `requesterName` (see below) and are **per-party unicast** (`Hub.SendToUsers`, never a vehicle-keyed broadcast). So the encrypted coordinates and PII never leave the server on the broadcast path — clients that need them refetch the party-only REST detail. Error-message construction on this surface uses opaque ids only (Rule CG-DC-2); the `409 conflict` / `404 not_found` reasons carry a status string or an id, never a coordinate/label/name.

> **Derived field — `requesterName` (MYR-229).** `RideRequest.requesterName` is a **projected, not persisted** field: `go_ride_requests` stores no requester-name column. The server resolves it per-read from the requester's (`rider_id`) identity across **three READ-ONLY tables in precedence order** (CG-DL-9 — name/email columns only, read **inline via correlated subselects in the same statement** as the ride row on every read/list/mutation return, so no separate lookup and no N+1): the Prisma-owned `"User"` table (legacy/web riders), then `go_identity_apple` (the Apple first-consent name — the real identity for Apple-native riders, MYR-264), then `go_users`. `COALESCE`/`NULLIF` fall through empty sources. The display value then follows the chain **first name → email local-part → `"Rider"`** (rest-api.md §7.8), omitted only when the rider has **no row in any of the three tables**. Widening beyond `"User"` (MYR-264) closed a gap where Apple-native riders — who have no `"User"` row — were omitted, so owners saw a client-side placeholder instead of the real name; it adds no new exposure surface (same P1 party-scope envelope). It is **P1 PII**: a real person's name, log-unsafe (**never logged** on any path), surfaced **only on party-scoped surfaces** — the party-only REST detail + list projections and the per-party-unicast WS summary frames — exactly matching the `riderId` party scope it annotates. No new stored column, so no encrypt/log-safe row above; the classification lives with the wire projection.

### 1.10 go_users table (Go-owned, MYR-193)

Apple-native users minted by the identity module (ADR-001) who have no legacy Prisma `"User"` row. Created by `internal/store/migrations/0003_identity.up.sql` under the CG-DL-9 `go_` namespace.

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `TEXT` (cuid) | P0 | No | Yes | User cuid — opaque internal identifier |
| `email` | `TEXT?` | P1 | No | No | PII — verified email captured at first sign-in (nullable when Apple hides it) |
| `name` | `TEXT?` | P1 | No | No | PII — display name from first sign-in |
| `created_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |
| `updated_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |

### 1.11 go_identity_apple table (Go-owned, MYR-193)

The authoritative `apple_sub -> user_id` binding, written once on first sign-in and reused verbatim thereafter (ADR-001 §4). Same `0003` migration.

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `apple_sub` | `TEXT` | P0 | No | Yes | Apple's stable pseudonymous subject id — opaque, not PII |
| `user_id` | `TEXT` (cuid) | P0 | No | Yes | Resolved user cuid (Prisma `"User".id` or `go_users.id`) — opaque |
| `email` | `TEXT?` | P1 | No | No | PII — email captured at first sign-in |
| `name` | `TEXT?` | P1 | No | No | PII — name captured at first sign-in |
| `created_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |
| `last_login_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |

### 1.12 go_refresh_tokens table (Go-owned, MYR-193)

Hash-only refresh-token store with single-use rotation and family reuse-detection (ADR-001 §5). Same `0003` migration. **The raw refresh token is never persisted** — only its SHA-256 digest — and access tokens are not stored at all.

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `token_hash` | `TEXT` (sha256 hex) | P1 | No | No | Lookup key for a bearer credential — never logged. Storing only the one-way hash IS the protection (like a password hash), so no additional app-level encryption |
| `family_id` | `TEXT` (cuid) | P0 | No | Yes | Rotation-lineage id — opaque |
| `user_id` | `TEXT` (cuid) | P0 | No | Yes | Owning user cuid — opaque |
| `issued_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |
| `expires_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |
| `rotated_from` | `TEXT?` (sha256 hex) | P1 | No | No | Previous token hash in the chain — same tier as `token_hash` |
| `rotated_to` | `TEXT?` (sha256 hex) | P1 | No | No | Successor token hash — same tier as `token_hash` |
| `revoked` | `BOOLEAN` | P0 | No | Yes | Non-sensitive flag |
| `revoked_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Non-sensitive timestamp |
| `reason` | `TEXT?` (enum) | P0 | No | Yes | Lifecycle enum: `rotated`/`revoked`/`reuse_detected` |

> **Token secrecy (MYR-193).** The ES256 access token and the raw refresh token are **P1** (credentials, same tier as `AuthPayload.token`). Neither is stored server-side (access tokens are stateless; refresh tokens are stored only as a SHA-256 hash). The identity module's auth audit trail (`slog`, ADR-001 §6) records the event + opaque user/family ids only — never an email, name, raw token, or token hash. The `/api/auth/*` error envelopes carry a generic `auth_failed` / `invalid_request` message with no PII and no reuse/linkage oracle (Rule CG-DC-2).

### 1.13 go_vehicle_control_state table (Go-owned, MYR-269)

Durable last-known owner-control read-back state — the four owner controls the app renders (Lock, Trunk/Frunk, Climate, Charge-port). These are the MYR-252 cabin read-backs that were stream-only with no persistence, so a `/snapshot` for a non-streaming car (in service / asleep / offline) always showed "Unavailable". Created by `internal/store/migrations/0008_vehicle_control_state.up.sql` under the CG-DL-9 `go_` namespace; written on both the live persist path and the MYR-260 `/vehicle_data` backfill (per-field COALESCE upsert, last-writer-wins) and LEFT-joined into `VehicleRepo.GetByID` for the REST `/snapshot`. Anchored: NFR-3.5 (snapshot completeness), NFR-3.9 (classification tiers). Every control column is nullable — NULL means "never read", surfaced as an honest "unavailable", never a fabricated on/off.

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `vehicle_id` | `TEXT` (cuid) | P0 | No | Yes | Owning vehicle cuid — opaque; no Prisma FK (CG-DL-9) |
| `is_locked` | `BOOLEAN?` | P0 | No | Yes | Lock state — same tier as the MYR-252 `locked` wire field; not identifying, no GPS |
| `frunk_open` | `BOOLEAN?` | P0 | No | Yes | Front-trunk open state (`frunkOpen`) — cabin/door state |
| `trunk_open` | `BOOLEAN?` | P0 | No | Yes | Rear-trunk open state (`trunkOpen`) — cabin/door state |
| `is_climate_on` | `BOOLEAN?` | P0 | No | Yes | Derived climate on/off (`isClimateOn`) — cabin comfort state |
| `charge_port_open` | `BOOLEAN?` | P0 | No | Yes | Charge-port door open state (`chargePortDoorOpen`) — door state |
| `updated_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |

> **No GPS, no PII (MYR-269).** All columns are P0 cabin/lock/door state — the same tier as the MYR-252 fields they persist, consistent with `speed`/`chargeLevel`/`gearPosition`. No coordinates, tokens, addresses, or names are stored, so no encryption or log-redaction is required.

---

## 2. Redaction rules by tier

These rules apply to all structured log output (`slog`), error messages (`fmt.Errorf`), crash reports, and Prometheus metric labels.

### 2.1 P0 (Public) — may appear in logs

P0 values may be included in structured log fields, error messages, and metric labels with the following exceptions:

- **VIN**: Although classified P0 (VINs are publicly visible on vehicle exteriors), VINs MUST be redacted to `***XXXX` (last 4 characters) in all log output and error messages. This is a defense-in-depth measure because VINs link to location data (P1). Use `redactVIN()` — already implemented in `internal/store/errors.go`, `internal/drives/stats.go`, and `internal/telemetry/vin.go`.
- **User IDs, Vehicle IDs, Drive IDs**: These opaque identifiers are log-safe. Log the full value for debugging.

### 2.2 P1 (Sensitive) — never in logs

P1 values MUST NOT appear in:

- **Structured log fields** — never pass a P1 value to `slog.String()`, `slog.Float64()`, `slog.Any()`, or any slog attribute.
- **Error messages** — never include a P1 value in `fmt.Errorf()` format strings. Use opaque identifiers to correlate (e.g., drive ID, vehicle ID).
- **Crash reports / stack traces** — P1 values must not be stored in local variables that appear in panic dumps. Prefer passing by pointer to minimize stack exposure.
- **Prometheus metric labels** — never use GPS coordinates, addresses, tokens, or email addresses as metric label values.
- **HTTP response bodies for errors** — never echo P1 values back in error responses.

**Specific redaction rules for P1 fields:**

| P1 field category | Redaction behavior |
|-------------------|--------------------|
| GPS coordinates (`latitude`, `longitude`, `destinationLatitude`, etc.) | Omit entirely from logs. Never round/truncate as a substitute for redaction. |
| Route data (`navRouteCoordinates`, `routePoints`) | Omit entirely. Log only the point count: `slog.Int("route_points", len(points))` |
| OAuth tokens (`access_token`, `refresh_token`, `id_token`) | Omit entirely. Never log even partial token strings. |
| Email addresses (`User.email`, `Invite.email`) | Omit entirely. Use the associated user ID or invite ID instead. |
| Location names/addresses (`locationName`, `locationAddress`, `startLocation`, `startAddress`, `endLocation`, `endAddress`, `destinationName`, `destinationAddress`) | Omit entirely. Log the associated drive ID or vehicle ID instead. |
| License plate (`licensePlate`) | Omit entirely. |
| User identity (`User.name`, `User.image`) | Omit entirely. Use user ID instead. |

### 2.3 P2 (Access-logged) — P1 rules plus audit trail

No P2 fields exist in v1. When P2 fields are introduced:

- All P1 redaction rules apply.
- Every read or write of a P2 column MUST emit an audit log entry containing: timestamp, actor user ID, operation (read/write), column name, and target record ID.
- Audit log entries themselves are classified P0 (they contain only opaque IDs, not the actual P2 values).

---

## 3. Encryption scope mapping

Per NFR-3.23, AES-256-GCM column-level encryption is applied to the following columns. Encryption/decryption is performed in the server's store layer (NFR-3.25) — the SDK never sees ciphertext.

### 3.1 Encrypted columns

| Table | Column | Data type | Encrypted type | Notes |
|-------|--------|-----------|----------------|-------|
| `Account` | `access_token` | `Text` | `Text` (base64 ciphertext) | Tesla OAuth access token |
| `Account` | `refresh_token` | `Text` | `Text` (base64 ciphertext) | Tesla OAuth refresh token |
| `Account` | `id_token` | `Text` | `Text` (base64 ciphertext) | OpenID Connect ID token |
| `Vehicle` | `latitude` | `Float` | `Text` (base64 ciphertext) | Current GPS latitude |
| `Vehicle` | `longitude` | `Float` | `Text` (base64 ciphertext) | Current GPS longitude |
| `Vehicle` | `destinationLatitude` | `Float` | `Text` (base64 ciphertext) | Nav destination latitude |
| `Vehicle` | `destinationLongitude` | `Float` | `Text` (base64 ciphertext) | Nav destination longitude |
| `Vehicle` | `originLatitude` | `Float` | `Text` (base64 ciphertext) | Nav origin latitude |
| `Vehicle` | `originLongitude` | `Float` | `Text` (base64 ciphertext) | Nav origin longitude |
| `Vehicle` | `navRouteCoordinates` | `Json` | `Text` (base64 ciphertext) | Route polyline coordinate array |
| `Drive` | `routePoints` | `Json` | `Text` (base64 ciphertext) | Full GPS trail for drive playback |
| `go_ride_requests` | `pickup_lat_enc` | `Float` (logical) | `Text` (base64 ciphertext) | Ride pickup latitude — encrypt-only, no plaintext column (MYR-173) |
| `go_ride_requests` | `pickup_lng_enc` | `Float` (logical) | `Text` (base64 ciphertext) | Ride pickup longitude — encrypt-only |
| `go_ride_requests` | `dropoff_lat_enc` | `Float` (logical) | `Text` (base64 ciphertext) | Ride drop-off latitude — encrypt-only |
| `go_ride_requests` | `dropoff_lng_enc` | `Float` (logical) | `Text` (base64 ciphertext) | Ride drop-off longitude — encrypt-only |

### 3.2 P1 columns NOT encrypted (rationale)

These P1 columns are sensitive and must never appear in logs, but are NOT encrypted at rest because they are human-readable strings that do not carry coordinate-precision location data or credential material. They benefit from database-level encryption (Supabase encrypts at the disk level) but do not require application-level AES-256-GCM:

| Table | Column | Rationale for no app-level encryption |
|-------|--------|---------------------------------------|
| `User` | `name` | Prisma-owned; disk encryption sufficient for display names |
| `User` | `email` | Prisma-owned; disk encryption sufficient; not queried by telemetry server |
| `User` | `image` | URL to avatar; disk encryption sufficient |
| `Vehicle` | `licensePlate` | Prisma-owned; disk encryption sufficient; not queried by telemetry server |
| `Vehicle` | `locationName` | Derived from GPS (already encrypted); reverse-geocoded label |
| `Vehicle` | `locationAddress` | Derived from GPS (already encrypted); reverse-geocoded address |
| `Vehicle` | `destinationName` | User-entered or Tesla-provided name; not coordinate data |
| `Vehicle` | `destinationAddress` | User-entered or Tesla-provided address; not coordinate data |
| `Drive` | `startLocation` | Reverse-geocoded from encrypted coordinates |
| `Drive` | `startAddress` | Reverse-geocoded from encrypted coordinates |
| `Drive` | `endLocation` | Reverse-geocoded from encrypted coordinates |
| `Drive` | `endAddress` | Reverse-geocoded from encrypted coordinates |
| `Invite` | `email` | Prisma-owned; disk encryption sufficient |
| `TripStop` | `name` | Prisma-owned; disk encryption sufficient |
| `TripStop` | `address` | Prisma-owned; disk encryption sufficient |
| `go_ride_requests` | `pickup_label` | Place name, not coordinate data (same rationale as `Drive.startLocation`) |
| `go_ride_requests` | `pickup_address` | Street address, not coordinate-precision data |
| `go_ride_requests` | `dropoff_label` | Place name, not coordinate data |
| `go_ride_requests` | `dropoff_address` | Street address, not coordinate-precision data |
| `go_ride_requests` | `passenger_name` | Display name; disk encryption sufficient (same rationale as `User.name`) |
| `go_ride_requests` | `passenger_phone` | PII, never logged; disk encryption sufficient today — promote to app-level encryption if threat modeling changes |

> **Design decision:** Application-level encryption is reserved for columns where a database breach would expose precise geolocation trails or credential material. Human-readable location strings (place names, addresses) are protected by Supabase disk-level encryption and the P1 log-redaction rules. If threat modeling changes (e.g., multi-tenant deployment, regulatory requirements), these columns can be promoted to app-level encryption by adding them to the AES-256-GCM scope.

### 3.3 Encryption implementation contract

- **Algorithm:** AES-256-GCM (authenticated encryption with associated data).
- **Canonical primitive:** [`internal/cryptox.Encryptor`](../../internal/cryptox/encryptor.go) is the only entry point for column-level encryption. The store layer uses the interface; never call `crypto/aes` or `crypto/cipher` directly from outside `internal/cryptox/`.
- **Key source:** `ENCRYPTION_KEY` environment variable (Fly.io secret, NFR-3.24) for single-key deployments. During key rotation, switch to the versioned shape: `ENCRYPTION_KEY_V{N}` plus `ENCRYPTION_WRITE_VERSION`. See [`key-rotation.md`](key-rotation.md) for the full env-var schema and rotation procedure (NFR-3.26).
- **Ciphertext format:** `[1B version][12B nonce][N B ciphertext + 16B GCM tag]`, base64-encoded as `Text` in PostgreSQL. The version byte routes the decrypt path to the matching key in the active `KeySet`, enabling in-place key rotation without re-encrypting old rows up front. Version `0x00` is reserved as invalid; `0x01` is the only emitted version today.
- **Nonce:** 12-byte cryptographically-random nonce per call (NIST SP 800-38D §5.2.1.1). Catastrophic GCM failure mode is nonce reuse; the package guarantees fresh nonces per `Encrypt` call.
- **Transparency:** Encrypt on write, decrypt on read, entirely within the store layer (NFR-3.25). The SDK, WebSocket broadcaster, and REST API handlers operate on plaintext values.
- **Foundation complete (as of MYR-65, 2026-05-09):** every P1 column identified by §3.1 now has an encrypt-on-write path; the unfinished-rollouts list is empty. The `internal/cryptox` package + startup wiring are landed; `ENCRYPTION_KEY` is required at boot. **OAuth tokens (`Account.access_token`, `Account.refresh_token`, `Account.id_token`) are encrypted on disk as of MYR-62** via the dual-write rollout (TS Phase 1 in `myrobotaxi/react-frontend#256`, Go Phase 2 in this repo). **Vehicle GPS coordinates (`Vehicle.latitude`/`longitude`, `destinationLatitude`/`destinationLongitude`, `originLatitude`/`originLongitude`) are encrypted on disk as of MYR-63** via the dual-write rollout (TS Phase 1 in `myrobotaxi/react-frontend#257`, Go Phase 2 in this repo). **Route-blob polylines (`Vehicle.navRouteCoordinates`, `Drive.routePoints`) are encrypted on disk as of MYR-64** via the dual-write rollout (TS Phase 1 in `myrobotaxi/react-frontend#258`, Go Phase 2 in this repo). Operators monitor rollout completion via `account_token_plaintext_remaining_total{column=...}`, `vehicle_gps_plaintext_remaining_total{column=...}`, and `route_blob_plaintext_remaining_total{column=...}` (each gauge → 0 once every legacy row has been backfilled), and rotation observability via `cryptox_decrypt_total{version="N"}` (wired in MYR-65; per-version successful-decrypt counter on the existing `/metrics` endpoint, used to confirm the retiring version's series decays to zero before the key is removed — see [`key-rotation.md`](key-rotation.md) §"Procedure" step 6).

---

## 4. New-field classification checklist

When adding a new persisted field in any table (via Prisma schema change, migration, or new Go struct field), complete the following steps before the PR can merge:

### Step 1: Determine the tier

Answer these questions about the new field:

1. **Does this field contain or derive from GPS coordinates?** If yes → **P1**, encrypt at rest.
2. **Does this field contain credential material (tokens, passwords, API keys)?** If yes → **P1**, encrypt at rest.
3. **Does this field contain PII (name, email, phone, address, photo)?** If yes → **P1**, no app-level encryption required (disk encryption sufficient unless it contains precise coordinates).
4. **Does this field reveal travel patterns, location history, or intent?** If yes → **P1**.
5. **Does this field require audit-logged access for compliance?** If yes → **P2**.
6. **None of the above?** → **P0**.

### Step 2: Add the classification to this document

1. Open `docs/contracts/data-classification.md`.
2. Add a row to the appropriate table in Section 1.
3. Fill in: Column name, Type, Tier, Encrypt (Yes/No), Log-safe (Yes/No), Rationale.

### Step 3: If P1 with encryption — update the encryption scope

1. Add the column to Section 3.1 (Encrypted columns table).
2. Implement encrypt-on-write and decrypt-on-read in the store layer.
3. Verify the column type in PostgreSQL is `Text` (to hold base64 ciphertext).
4. Add a unit test that round-trips a value through encrypt/decrypt.

### Step 4: If P1 without encryption — document the rationale

1. Add the column to Section 3.2 (P1 not encrypted table) with a rationale.

### Step 5: Verify log safety

1. Search the codebase for any `slog.*` or `fmt.Errorf` call that references the new field.
2. If the field is P1, confirm it is never logged. Add a `// P1 — never log` comment at the field declaration.
3. If the field is P0 with the VIN exception, confirm `redactVIN()` is used.

### Step 6: Update related contract docs

1. If the field is in an atomic group, update `vehicle-state-schema.md` with the group membership.
2. If the field changes the data lifecycle, update `data-lifecycle.md`.
3. If the field is exposed over WebSocket or REST, update the corresponding protocol doc.

### Step 7: contract-guard validation

The `contract-guard` CI gate checks:

- Every column in `internal/store/types.go` structs has a corresponding row in this document.
- Every column with `Encrypt: Yes` has encrypt/decrypt calls in the store layer.
- No P1 field name appears in `slog.*()` calls outside of test files.

If `contract-guard` fails, the PR is blocked until classifications are added.

---

## 5. contract-guard rule description

The `contract-guard` agent/CI check enforces the following rules derived from this document:

### Rule CG-DC-1: Classification completeness

**Trigger:** Any PR that adds or modifies a column in `internal/store/types.go` (Go structs), `internal/store/queries.go` (SQL column lists), `internal/store/db_test.go` (test schema), or `prisma/schema.prisma` (in the partner repo).

**Check:** Every column name present in the Go struct or SQL query MUST have a corresponding row in Section 1 of this document. Missing classifications block merge.

**Scope note:** This rule validates against Go structs (the subset of columns the telemetry server uses). Prisma-only tables (User, Account, Invite, TripStop, Settings) are documented in this contract for completeness but are validated against the Prisma schema in the partner frontend repo — not by this rule. Columns in this doc that don't appear in Go structs are annotated "Prisma-owned" and are not enforced by the telemetry server's contract-guard.

**Fix:** Follow the new-field checklist (Section 4).

### Rule CG-DC-2: P1 log safety

**Trigger:** Any PR that modifies Go files in `internal/`.

**Check:** Scan all `slog.String()`, `slog.Float64()`, `slog.Any()`, `slog.Int()`, and `fmt.Errorf()` calls. If any argument references a field name classified P1 in this document (e.g., `latitude`, `longitude`, `access_token`, `email`, `destinationName`, `locationName`, `locationAddress`, `startLocation`, `startAddress`, `endLocation`, `endAddress`, `routePoints`, `navRouteCoordinates`), the PR is blocked.

**Exception:** Test files (`*_test.go`) are exempt from this check.

**Fix:** Remove the P1 value from the log/error statement. Use an opaque identifier (vehicle ID, drive ID, user ID) for correlation instead.

### Rule CG-DC-3: VIN redaction

**Trigger:** Any PR that logs a VIN value.

**Check:** Every `slog.String("vin", ...)` call MUST use `redactVIN(vin)` as the value, not the raw VIN. Raw VIN values in log statements block merge.

**Fix:** Wrap the VIN with `redactVIN()` before passing to the logger.

### Rule CG-DC-4: Encryption coverage

**Trigger:** Any PR that adds a new column to the "Encrypted columns" table in Section 3.1.

**Check:** The store layer MUST contain corresponding encrypt-on-write and decrypt-on-read calls for the new column. The PR must include both the encryption implementation and the classification update.

**Fix:** Implement AES-256-GCM encrypt/decrypt in the store layer and add a round-trip test.

### Rule CG-DC-5: Role-mask coverage for SDK-exposed fields

**Anchored:** NFR-3.19, NFR-3.20, FR-5.4, FR-5.5.

**Trigger:** Any PR that adds a field to a payload schema (`docs/contracts/schemas/vehicle-state.schema.json`, drive-detail / drive-route response shapes in `docs/contracts/rest-api.md` §7), OR any PR that adds a column to a `Vehicle` / `Drive` / `DriveRoutePoint` / `Invite` row that is then exposed over REST or WebSocket.

**Check:** Every persisted column listed in this document's §1 that is exposed over a REST endpoint or WebSocket frame MUST appear in [`rest-api.md`](rest-api.md) §5.2's per-resource mask matrix — under at least one role's "Visible fields" set, OR explicitly enumerated as "not exposed in v1" with rationale. The mask matrix is the single source-of-truth consumed by both the WebSocket per-role projection (`websocket-protocol.md` §4.6) and the REST handler-layer mask (`rest-api.md` §5.1); a field that lands in a payload schema without a §5.2 mask entry would default to "owner-only via fail-closed allow-list" and silently disappear from viewer payloads, hiding the gap from review.

**Why it matters:** without this gate, a field can be added to a wire schema (e.g., a new `Vehicle.someField`) and merged before the §5.2 matrix decides whether viewers should see it. The runtime fail-closed default keeps viewers safe from leaks but creates silent UX regressions ("why is the viewer's app missing the new field?") that surface only at runtime.

**Fix:** Update [`rest-api.md`](rest-api.md) §5.2 in the same PR. Either add the field to the appropriate role's "Visible fields" list, or document explicitly that it is not exposed in v1.

**Rule does NOT apply to:** Prisma-owned columns that are never surfaced over REST or WS (e.g., `User.id`, `Account.refresh_token`). These are documented in §1 for completeness but are out of the SDK contract surface.

---

## 6. Classification summary

### By tier

| Tier | Count | Description |
|------|-------|-------------|
| P0 | 97 | Public — timestamps, opaque IDs, aggregate stats, feature flags, enums |
| P1 | 36 | Sensitive — GPS coordinates, location names/addresses, OAuth tokens, PII, route data |
| P2 | 0 | Access-logged — reserved for future use |

> **Count audit trail.** The P0 count was bumped from 83 → 85 by [MYR-11](https://linear.app/myrobotaxi/issue/MYR-11) when it added `Vehicle.chargeState` (Tesla proto field **179** `DetailedChargeState`, enum — see DV-19 for the 2026-04-23 empirical finding that switched the source from proto 2 to proto 179) and `Vehicle.timeToFull` (Tesla proto field 43, `Float` **hours (decimal)**) to the v1 charge atomic group. Both fields are P0 because they describe charge state, not identity or location. See §1.3 Vehicle table and `vehicle-state-schema.md` §2.2 for the wire contract. The `timeToFull` unit was empirically verified as hours (1.0667h capture) on 2026-04-22 — [DV-17 RESOLVED](https://linear.app/myrobotaxi/issue/MYR-25#comment-4f1dcee9-ab10-4039-acc5-9e7ef25c3762). Future count changes MUST add a one-line entry here so the total is auditable without `git blame`. P0 85 → 97 and P1 26 → 36 by MYR-173: the new Go-owned `go_ride_requests` table (§1.9) adds 12 P0 columns (ids, status enums, timestamps), 4 P1 encrypted coordinate columns (encrypt-only, §3.1), and 6 P1 log-redaction-only columns (place labels/addresses + booked-for passenger name/phone, §3.2).

### P1 fields requiring AES-256-GCM encryption (15 columns)

1. `Account.access_token`
2. `Account.refresh_token`
3. `Account.id_token`
4. `Vehicle.latitude`
5. `Vehicle.longitude`
6. `Vehicle.destinationLatitude`
7. `Vehicle.destinationLongitude`
8. `Vehicle.originLatitude`
9. `Vehicle.originLongitude`
10. `Vehicle.navRouteCoordinates`
11. `Drive.routePoints`
12. `go_ride_requests.pickup_lat_enc`
13. `go_ride_requests.pickup_lng_enc`
14. `go_ride_requests.dropoff_lat_enc`
15. `go_ride_requests.dropoff_lng_enc`

### P1 fields with log-redaction only (no app-level encryption, 21 columns)

1. `User.name`
2. `User.email`
3. `User.image`
4. `Vehicle.licensePlate`
5. `Vehicle.locationName`
6. `Vehicle.locationAddress`
7. `Vehicle.destinationName`
8. `Vehicle.destinationAddress`
9. `Drive.startLocation`
10. `Drive.startAddress`
11. `Drive.endLocation`
12. `Drive.endAddress`
13. `Invite.email`
14. `TripStop.name`
15. `TripStop.address`
16. `go_ride_requests.pickup_label`
17. `go_ride_requests.pickup_address`
18. `go_ride_requests.dropoff_label`
19. `go_ride_requests.dropoff_address`
20. `go_ride_requests.passenger_name`
21. `go_ride_requests.passenger_phone`
