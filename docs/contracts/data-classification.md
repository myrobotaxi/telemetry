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
> **Ciphertext-only as of MYR-433 (2026-08-02).** The Account table carries `access_token_enc`, `refresh_token_enc`, and `id_token_enc` (`Text?` ciphertext columns). **The Go store layer reads and writes ONLY those columns.** It does not select the legacy plaintext `access_token` / `refresh_token` / `id_token`, does not set them, and has no plaintext fallback: a row whose ciphertext is absent reads as "no token".
>
> This closed the MYR-62 dual-write, which wrote both copies and preferred ciphertext on read. The dual-write's justification was roll-back safety — an older binary could still service traffic from the plaintext column — and for this column family that trade was wrong. These are Tesla fleet-control credentials: a leak of the plaintext columns lets an attacker drive the user's car. Roll-back safety now comes from the ciphertext being readable by both this server and the Next.js app (byte-compatible AES-256-GCM under a shared `ENCRYPTION_KEY`), not from a second readable copy.
>
> `OwnerProvisioner` additionally NULLs the plaintext columns on its conflict-update, so re-linking a Tesla account scrubs pre-MYR-433 residue rather than preserving it.
>
> Migration tooling: `cmd/backfill-account-tokens/` seals legacy rows into the `_enc` columns; `cmd/purge-plaintext-columns/` then removes the plaintext values (verify-before-destroy — see §3.3). **The plaintext columns still EXIST on the table**; dropping them is a react-frontend Prisma migration, because Go migrations may not touch Prisma-owned tables (CG-DL-9).
>
> **Every provider, not just Tesla (MYR-433 follow-up).** The backfill originally scanned `WHERE "provider" = 'tesla'`, matching MYR-62's scope — the tokens *this server* reads. The first production purge run exposed the gap that left: **7 Apple Sign-in rows** holding a plaintext `access_token` and identity `id_token`, which the purge correctly refused to scrub because nothing had sealed them. The `Account` classification above is provider-agnostic and always was: an Apple `id_token` is an identity JWT, tiered P1 for exactly the reason the table states ("contains user identity claims"). "The telemetry server does not read it" was never the criterion. The scan and the `account_token_plaintext_remaining_total` gauge are now provider-agnostic; a gauge reading zero while unsealed rows exist is worse than no gauge, since it is the signal used to call the rollout finished.
>
> Nothing reads these columns for non-Tesla rows, verified in both repos before the change: every Go `Account` query is `provider = 'tesla'`-scoped, and in the Next.js app the sole token consumer (`src/lib/tesla.ts`) is Tesla-scoped while the Prisma adapter's `getUserByAccount` hydrates the row and returns only `account.user`, discarding the tokens.
>
> **If Sign in with Apple revocation is ever implemented**, it MUST read `id_token_enc` / `refresh_token_enc` through the decrypting path, never the plaintext columns. Apple requires token revocation on account deletion and neither repo implements it today — the Go deletion sequence revokes its own `go_refresh_tokens` and cascades the `Account` row away.

### 1.3 Vehicle table

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `userId` | `String` | P0 | No | Yes | FK to User — opaque identifier |
| `teslaVehicleId` | `String?` | P0 | No | Yes | Tesla-assigned vehicle ID — opaque |
| `vin` | `String?` | P0 | No | **Last-4 in logs; role-masked on the wire** | Publicly visible on vehicle exterior; P1 encryption would be overkill for a value stamped on the car. Risk is mitigated by mandatory `redactVIN()` redaction to `***XXXX` in all logs (see §2.1 VIN redaction rule). **Wire exposure (MYR-279):** the FULL vin is returned on the REST `/snapshot` to the vehicle's OWNER only — gated to the owner role mask (party-scoped), removed from the viewer allow-list, and NEVER emitted on the WebSocket `vehicle_update` broadcast. The vehicles-list catalog surfaces `vinLast4` only. See `vehicle-state.schema.json` `vin` and `internal/mask/tables.go` `vehicleStateOwnerFields`/`vehicleStateViewerFields` (FR-4.2, NFR-3.9). |
| `name` | `String` | P0 | No | Yes | User-assigned vehicle name |
| `model` | `String` | P0 | No | Yes | Vehicle model (e.g., "Model 3") — an equipment fact, the same tier as `year`/`color` and not identifying. **Provenance changed by MYR-507 (tier, encryption, log-safety and wire exposure all unchanged):** the column and the wire field long predate it, but were populated only for cars linked through the Prisma web flow — the MYR-257 provisioning INSERT seeds `''` and no Go or Prisma writer ever filled it, so a Go-linked car reported an empty model forever. Now derived from **VIN position 4** (`internal/vin`) at link time and backfilled by `store.VehicleRepo.FillVehicleIdentity` — the **fifth** sanctioned §1.4 write carve-out in `data-lifecycle.md`, and the first sourced from the VIN rather than from Tesla. **Fill-if-empty, never overwrite:** Prisma stays authoritative where it wrote first, because it records a richer value for some cars ("Model 3 Performance") than a VIN position can encode. An unrecognised VIN code is never written. Log-safe in full, like `color` and unlike the neighbouring P1 `licensePlate` |
| `year` | `Int` | P0 | No | Yes | Model year — same tier, same provenance change and same fill-if-empty rule as `model` above (MYR-507). Derived from **VIN position 10**, the ISO 3779 model-year code; **Tesla's Fleet API exposes no model year at any path**, so the VIN is the platform's only source of one. `0` means "not determined", never a real year |
| `color` | `String` | P0 | No | Yes | Vehicle color — an equipment/appearance fact, the same tier as `model`/`year` and not identifying. **Provenance changed by MYR-320 (tier, encryption, log-safety and wire exposure all unchanged):** the column and the wire field both predate MYR-320 but were never populated (the MYR-257 provisioning INSERT seeds `''`); they are now filled from Tesla REST `vehicle_data.vehicle_config.exterior_color` by a single-column owner-scoped UPDATE (`store.VehicleRepo.UpdateVehicleColor` — the fourth sanctioned §1.4 write carve-out in `data-lifecycle.md`), where an empty Tesla value never overwrites a good one. Log-safe in full, unlike the neighbouring P1 `licensePlate` |
| `licensePlate` | `String` | P1 | No | **No — redact in logs; role-masked on the wire (BOTH roles)** | Externally correlatable to a person via DMV / third-party registry lookups, so it is identifying data — hence P1 rather than the P0 of its neighbours `model`/`color`. **Never log the value** (§2.2); log the P0 `vehicle_id` / `user_id` instead. **Wire exposure (MYR-286):** as of MYR-286 the plate IS on the wire — on the REST `/snapshot` (`vehicle-state.schema.json`) and on the `GET /api/vehicles` catalog row (`vehicle-summary.schema.json`) — and is in **BOTH** role allow-lists, owner AND viewer. That is a deliberate product decision, not an oversight: the entire purpose of the plate is that a rider can identify the correct car at pickup, which fails if only the owner can see it. Contrast the sibling `vin` above, which MYR-279 gated to the owner mask. Party-scope still binds: never emit it outside the vehicle's party (owner + invited viewers/riders of that vehicle), and it is NEVER on the WebSocket `vehicle_update` broadcast. **Write path (MYR-286):** not from Tesla and not from Prisma — the Fleet API exposes no plate anywhere, so the ONLY writer is the owner's `PUT /api/tesla/vehicles/{vehicleId}/plate` (`rest-api.md` §7.14) through the `data-lifecycle.md` §1.4 carve-out. Server-normalized on write (trim, uppercase, ≤ 10 chars, `^[A-Z0-9 -]*$`); empty string means "no plate set". See `internal/mask/tables.go` `vehicleStateOwnerFields` / `vehicleSummaryOwnerFields`. |
| `chargeLevel` | `Int` | P0 | No | Yes | Battery percentage — not identifying |
| `chargeState` | `String` (enum) | P0 | No | Yes | Charge state enum (`Disconnected`, `Charging`, `Complete`, …) — not identifying. Sourced from Tesla proto field **179** (`DetailedChargeState`) as of [MYR-42](https://linear.app/myrobotaxi/issue/MYR-42) (2026-04-23); MYR-40 initially sourced from proto 2 but empirical capture showed Tesla firmware ≥ 2024.44.25 does not emit proto 2, so the switch to proto 179 was non-behavioral (same 7 enum strings). Added by MYR-11 (v1 charge atomic group); source proto corrected by MYR-42. |
| `estimatedRange` | `Int` | P0 | No | Yes | Range in miles — not identifying |
| `timeToFull` | `Float` | P0 | No | Yes | **Hours (decimal)** to full charge at current rate — not identifying. Tesla proto field 43 (`TimeToFullCharge`, double). Unit per `tesla-fleet-telemetry-sme` skill + legacy Tesla REST API; empirical verification tracked as `websocket-protocol.md` §10 DV-17. Added by MYR-11 (v1 charge atomic group). |
| `status` | `VehicleStatus` | P0 | No | Yes | Enum: driving/parked/charging/offline/in_service |
| `speed` | `Int` | P0 | No | Yes | Speed in mph — not identifying without GPS |
| `gearPosition` | `String?` | P0 | No | Yes | Gear state — not identifying |
| `heading` | `Int` | P0 | No | Yes | Compass heading — not identifying without GPS |
| `locationName` | `String` | P1 | **Yes** | No | Reverse-geocoded place name — reveals location. Encrypted by MYR-447; `locationNameEnc` is the sole store |
| `locationAddress` | `String` | P1 | **Yes** | No | Street address — reveals location. Encrypted by MYR-447; `locationAddressEnc` is the sole store |
| `latitude` | `Float` | P1 | **Yes** | No | GPS coordinate — NFR-3.23. **Wire exposure widened by MYR-515 (tier, encryption and log-handling all unchanged):** as of contracts v0.31.0 this pair is ALSO emitted on the `GET /api/vehicles` catalog row, as the nullable `VehicleSummary.location` object, in **BOTH** role allow-lists — the first P1 field on that resource other than `licensePlate`. Same column, same resolver, same full precision as the `/snapshot` and the live `vehicle_update`. **Not a new disclosure:** a viewer already receives this pair at full precision on the STREAMING path for exactly these vehicles (`vehicleStateViewerFields` retains the whole Speed/GPS atomic group, and MYR-435 deliberately left it), so the catalog is the same value over a different transport to an already-entitled party. **The caveat, stated:** the catalog aggregates N positions into one response where the socket serves one per subscription — an aggregation difference, not an entitlement one, since the grant is per-vehicle. Coarsening was considered and rejected (see `rest-api.md` §7.0). The `(0,0)` no-fix sentinel is collapsed to `null` on this surface |
| `longitude` | `Float` | P1 | **Yes** | No | GPS coordinate — NFR-3.23. Same MYR-515 wire-exposure widening as `latitude` above — the two travel as one indivisible object and are never emitted apart |
| `interiorTemp` | `Int` | P0 | No | Yes | Cabin temperature — not identifying |
| `exteriorTemp` | `Int` | P0 | No | Yes | Ambient temperature — not identifying |
| `odometerMiles` | `Int` | P0 | No | Yes | Odometer reading — not identifying |
| `fsdMilesSinceReset` | `Float` | P0 | No | Yes | FSD miles driven since last reset — not identifying |
| `virtualKeyPaired` | `Boolean` | P0 | No | Yes | Pairing status flag |
| `setupStatus` | `SetupStatus` | P0 | No | Yes | Enum: setup lifecycle state — **Prisma-owned**, not currently accessed by the telemetry server |
| `destinationName` | `String?` | P1 | **Yes** | No | Reveals travel intent/plans. Encrypted by MYR-447; `destinationNameEnc` is the sole store |
| `destinationAddress` | `String?` | P1 | **Yes** | No | Reveals travel intent/plans. Encrypted by MYR-447; `destinationAddressEnc` is the sole store |
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

> **Ciphertext-only as of MYR-433 (2026-08-02).** The six Vehicle GPS columns (`latitude`/`longitude`, `destinationLatitude`/`destinationLongitude`, `originLatitude`/`originLongitude`) have ciphertext shadows: `latitudeEnc`, `longitudeEnc`, `destinationLatitudeEnc`, `destinationLongitudeEnc`, `originLatitudeEnc`, `originLongitudeEnc` (all `Text?`). **The Go `VehicleRepo` reads and writes ONLY the `*Enc` columns.** The plaintext Float columns are absent from `vehicleSelectColumns` and from `updateColumns`; there is no plaintext fallback on any read path.
>
> **Atomic-pair invariant (`vehicle-state-schema.md` §3.3 GPS predicates):** a half-pair `*Enc` row (one column populated, the other NULL) is corrupt. Writers refuse to emit one; readers now surface **no location** for the whole pair and log a warning. Pre-MYR-433 they fell back to the plaintext pair — that fallback was itself the exposure this issue closed, since it required the plaintext column to be readable.
>
> A `VehicleRepo` built without an `Encryptor` (the legacy `NewVehicleRepo` constructor) therefore reads **no coordinates at all**. Callers needing GPS must use `NewVehicleRepoWithEncryption`; the ops CLI now does.
>
> Migration tooling: `cmd/backfill-vehicle-gps/` seals legacy rows; `cmd/purge-plaintext-columns/` then scrubs the plaintext values. Note that `latitude`/`longitude` are **NOT NULL** on the Prisma schema, so their scrubbed value is the zero coordinate rather than NULL; the four nullable columns scrub to NULL. **The plaintext columns still EXIST**; dropping them is a react-frontend Prisma migration (CG-DL-9).

> **MYR-252 cabin-control read-back fields (WS-live, not persisted):** The 21 cabin-control wire fields added by [MYR-252](https://linear.app/myrobotaxi/issue/MYR-252) — `locked`, `hvacPower`, `isClimateOn`, `fanSpeed`, `driverTempSetting`, `passengerTempSetting`, `hvacAutoMode`, `hvacAcEnabled`, `seatHeaterLeft`/`Right`, `seatHeaterRearLeft`/`Center`/`Right`, `seatCoolerLeft`/`Right`, `seatVentEnabled`, `chargePortDoorOpen`, `frunkOpen`, `trunkOpen`, `mediaPlaybackStatus`, `mediaVolume` — are **all P0**. Cabin comfort/lock/door/media state is not identifying (same reasoning as `interiorTemp`/`chargeLevel`/`gearPosition`); note that P2 (§0) is the platform's *most*-sensitive tier (payment/health), so P2 would be the wrong label here. These are **not `Vehicle` table columns** — they are delivered live on the WS `vehicle_update` stream (owner mask allow-list; `rest-api.md` §5.2.1). **Persistence status (updated by [MYR-298](https://linear.app/myrobotaxi/issue/MYR-298)):** the [MYR-253](https://linear.app/myrobotaxi/issue/MYR-253) hydration landed **not** as Prisma `Vehicle` columns but as the Go-owned `go_vehicle_control_state` side table — **20 of the 21** are now persisted there and classified per-column in **§1.13** (MYR-269, MYR-273, MYR-279, MYR-274, MYR-298), so CG-DC-5 is satisfied by those §1.13 rows. Only `hvacPower` remains unpersisted and thus CG-DC-5-exempt; its server-derived `isClimateOn` boolean IS a §1.13 column. Per-field wire definitions: `vehicle-state-schema.md` §1.1.

> **MYR-259 `ServiceMode` signal (internal-only, not persisted, not on the wire):** The `ServiceMode` telemetry field (Tesla proto **159**, `bool`) added by [MYR-259](https://linear.app/myrobotaxi/issue/MYR-259) is **P0** — like the other cabin/vehicle state above, service-mode state is operational, not identifying. Unlike the MYR-252 fields, `ServiceMode` is **not even a wire field**: it is decoded server-side, cached per-VIN, folded into the `status` derivation (`status = in_service`), and then **stripped before broadcast** — only the already-P0 `status` enum reaches clients (and the persisted `Vehicle.status` column). So `ServiceMode` is neither a `Vehicle` column nor a WS field and is exempt from CG-DC-5. Its P0 handling matters only for **log safety**: the raw signal MAY appear in logs (no redaction required), consistent with P0. The companion signal — Tesla's REST `in_service` bool read on connectivity edges — is likewise non-identifying P0 and surfaces only as the persisted `status` enum. Per-field definition + derivation: `vehicle-state-schema.md` §2.4.

### 1.4 Drive table

> **Retention: 365 days, enforced (MYR-439, 2026-08-02).** Every row in this table is deleted once its `createdAt` passes **365 days** — the P1 GPS trail and the P1 location/address strings along with it. **No row has reached that age yet:** production holds 315 drives, the oldest created 2026-03-08, so the first deletion happens on **2027-03-08**. The enforcement is in place ahead of the window, not catching up to it — nothing was ever over-retained. The window is NFR-3.27, the job that enforces it is specified in [`data-lifecycle.md`](data-lifecycle.md) §5, and it is implemented as `store.DrivePruner` driven by `internal/retention.Pruner` (daily, 03:00 UTC, batched, audited).
>
> **The window lives in exactly one place:** `store.DriveRetentionDays` in `internal/store/drive_pruner.go`. It is a compile-time constant and is deliberately unreachable from configuration (CG-DL-4) — the `DRIVE_RETENTION_PRUNER_ENABLED` kill-switch can stop the sweep but cannot shorten or lengthen the window. Changing it means changing `data-lifecycle.md` §2.2 first, then that constant, and nothing else.
>
> **There is nothing else to delete.** No table anywhere is keyed by a drive id — no sidecar, no derivative, no separate blob store. The GPS trail is two columns ON this row (`routePoints` plaintext + `routePointsEnc` ciphertext), so deleting the row is the complete deletion of both copies. This is why the pruner's statements name no column, and why [MYR-433](https://linear.app/myrobotaxi/issue/MYR-433) retiring the plaintext copy changes nothing about them.

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `vehicleId` | `String` | P0 | No | Yes | FK to Vehicle — opaque identifier |
| `date` | `String` | P0 | No | Yes | Date string — not identifying on its own |
| `startTime` | `String` | P0 | No | Yes | ISO 8601 timestamp — not identifying on its own |
| `endTime` | `String` | P0 | No | Yes | ISO 8601 timestamp — not identifying on its own |
| `startLocation` | `String` | P1 | **Yes** | No | Reverse-geocoded place name — reveals home/work. Encrypted by MYR-447; `startLocationEnc` is the sole store |
| `startAddress` | `String` | P1 | **Yes** | No | Street address — reveals home/work. Encrypted by MYR-447; `startAddressEnc` is the sole store |
| `endLocation` | `String` | P1 | **Yes** | No | Reverse-geocoded place name — reveals destinations. Encrypted by MYR-447; `endLocationEnc` is the sole store |
| `endAddress` | `String` | P1 | **Yes** | No | Street address — reveals destinations. Encrypted by MYR-447; `endAddressEnc` is the sole store |
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

> **Ciphertext-only as of MYR-433 (2026-08-02).** The route-blob polylines on `Vehicle.navRouteCoordinates` (Tesla planned navigation polyline, member of the navigation atomic group) and `Drive.routePoints` (recorded breadcrumb trail) have ciphertext shadows: `Vehicle.navRouteCoordinatesEnc` and `Drive.routePointsEnc` (both `Text?`). **The Go store layer reads and writes ONLY the shadows.** Decrypt or non-array failures now surface an **empty** route/trail with a `slog.Warn` instead of falling back to plaintext. Route blobs can exceed 100KB; corruption still MUST NOT 500 the live nav-route view, which is why the failure mode is empty-and-logged rather than an error.
>
> `Drive.routePoints` is the drive GPS trail the client named explicitly in the MYR-433 acceptance bar: *"I should not be able to go into the db and see users drive."*
>
> **Drive append semantics changed.** Ciphertext cannot be concatenated in SQL, so `DriveRepo.AppendRoutePoints` no longer does a `jsonb_concat (||)` plaintext append. It runs a decrypt-append-reseal cycle inside a transaction holding `SELECT … FOR UPDATE` on the Drive row. Two consequences worth knowing:
>
> - The row lock supplies the atomicity the SQL concat had for free. Without it, concurrent flushes for one drive would each decrypt the same trail and the later commit would silently drop the earlier one's points.
> - The path is **fail-closed**, reversing the MYR-64 fail-open rule. Fail-open was right when a plaintext copy was still landing; now, committing after a decrypt failure would overwrite an intact-but-unreadable trail with a fragment. An error instead lets the caller retry with its buffer intact.
>
> `Drive.routePoints` is NOT NULL, so the INSERT writes the literal `[]` and every real coordinate goes to the shadow. A `DriveRepo` without an `Encryptor` returns `ErrEncryptionRequired` from `AppendRoutePoints` rather than silently discarding points.
>
> Migration tooling: `cmd/backfill-route-blobs/` seals legacy rows; `cmd/purge-plaintext-columns/` then scrubs plaintext (`navRouteCoordinates` → NULL, `routePoints` → `[]`). **The plaintext columns still EXIST**; dropping them is a react-frontend Prisma migration (CG-DL-9).

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

> **RETIRED UNUSED ([MYR-184](https://linear.app/myrobotaxi/issue/MYR-184), 2026-07-29).** This table is Prisma-owned, the telemetry server never read or wrote it, and **no invite row was ever written against it by anything**. The classifications above stayed here waiting for the FR-5.x sharing features to arrive; when they did, they arrived somewhere else. Sharing shipped as **§1.15 `go_vehicle_shares`** — Go-owned, code-based, and shaped nothing like this: there is no email infrastructure and riders are Apple-native, so the `email` column that anchors this design has no producer, and `senderId` / `sentDate` / `lastSeen` / `isOnline` have no consumer. `rest-api.md` §10 DV-23, which assigned the invite endpoints to the now-deprecated Next.js app against this table, is **SUPERSEDED** for that half. The rows above are kept, not deleted, because the table itself still exists in the sibling schema and an un-classified column is a `contract-guard` violation whether or not anybody writes to it. Treat this section as a description of a dead relation: **do not add columns here, and do not model new sharing work on it** — §1.15 is the live one.

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
| `passenger_name` | `TEXT?` | P1 | No | No | **DEPRECATED (MYR-447).** PII — booked-for passenger's name. The book-for-someone-else feature was removed from the app in [MYR-382](https://linear.app/myrobotaxi/issue/MYR-382); the column and its `omitempty` wire key remain for legacy rows. **Retention is 30 days from terminal, shorter than the row's own 365** ([`data-lifecycle.md`](data-lifecycle.md) §2.2.2) — and it was cleared across the whole table once at MYR-447 deploy by `cmd/scrub-passenger-fields`. Tier is unchanged: while a value exists it is still P1 |
| `passenger_phone` | `TEXT?` | P1 | No | No | **DEPRECATED (MYR-447).** PII — booked-for passenger's mobile (tracking-link SMS target). Never logged. Same removal, same 30-day window, same one-time scrub as `passenger_name` — and **the two are always scrubbed TOGETHER, never phone alone**: the iOS passenger block is gated on the NAME and formats the phone first, so a name-without-phone row renders a leading blank instead of disappearing |
| `scheduled_for` | `TIMESTAMPTZ?` | P0 | No | Yes | Reservation instant — no PII without the (P1) places |
| `reschedule_proposed_for` | `TIMESTAMPTZ?` | P0 | No | Yes | Proposed new pickup instant |
| `reschedule_status` | `TEXT?` (enum, CHECK) | P0 | No | Yes | Sub-state enum: requested/confirmed/declined |
| `accepted_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Non-sensitive timestamp |
| `completed_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Non-sensitive timestamp |
| `created_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |
| `updated_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |
| `dispatch_status` | `TEXT?` (enum, CHECK) | P0 | No | Yes | Nav-dispatch outcome enum: sent/failed/skipped (MYR-176). Opaque, non-sensitive |
| `dispatched_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Nav-dispatch attempt instant + exactly-once claim latch (MYR-176) |
| `dispatch_error` | `TEXT?` | P0 | No | Yes | Opaque nav-dispatch failure CODE — constructed from the typed command error / resolution sentinel only; never a coordinate/address/token/raw VIN (Rule CG-DC-2). Not exposed on the wire. Value set: the typed command codes (`key_not_paired`, `permission_denied`, `vehicle_asleep`, `command_failed`, `invalid_request`, `internal_error`) plus the dispatch-local codes `vehicle_unresolved`, `token_expired` (linked but expired — user must re-link), `token_unavailable` (never linked / could not obtain), `transport_unconfigured` (proxy not configured), `dispatch_canceled`, `dispatch_interrupted` (claimed but the process died pre-outcome; set by the startup reconciler) (MYR-176), and `reservation_expired` (a SCHEDULED ride was still undispatched past its `scheduledFor + 30 min` lateness ceiling — its vehicle stayed on another active ride, or dispatch itself was unavailable — so the reservation was failed without a push; reservation-only, MYR-179) |
| `dropoff_dispatch_status` | `TEXT?` (enum, CHECK) | P0 | No | Yes | Leg-2 (dropoff) nav-dispatch outcome enum: sent/failed/skipped (MYR-265). Independent of the leg-1 `dispatch_status` so neither leg clobbers the other. Opaque, non-sensitive |
| `dropoff_dispatched_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Leg-2 nav-dispatch attempt instant + exactly-once claim latch (MYR-265) |
| `dropoff_dispatch_error` | `TEXT?` | P0 | No | Yes | Opaque leg-2 (dropoff) nav-dispatch failure CODE — same construction/value-set/never-a-secret rules as `dispatch_error`; not exposed on the wire (MYR-265) |
| `enroute_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Leg-2 start instant, stamped when the ride enters `enroute` (rider **start**, MYR-270; was the `board` instant in MYR-265). Retained as a lifecycle timestamp; MYR-270 removed the drive-end completer, so it no longer feeds any drive-end correlation. Non-sensitive; not exposed on the wire |
| `picked_up_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Owner-confirmed pickup instant, stamped when the ride enters `arrived` (owner **picked-up**, MYR-270; migration 0009). Audit/operability timestamp; non-sensitive; not exposed on the wire |

> **Encrypt-only coordinates (no dual-write).** Unlike the MYR-63/64 rollouts, `go_ride_requests` is a brand-new table: coordinates are written exclusively as AES-256-GCM ciphertext (`internal/store/ride_request_scan.go`, reusing the `vehicle_gps_encryption.go` float codec). There are no plaintext coordinate columns, no backfill tool, and no `*_plaintext_remaining_total` gauge. `RideRequestRepo` therefore requires an `Encryptor` at construction (panics on nil) and treats decrypt failure as a hard read error — there is no fallback column to fall back to.

> **Wire surface (MYR-174).** The rider-facing REST endpoints (`rest-api.md` §7.8) return the full `RideRequest` object — including the decrypted P1 `pickup`/`dropoff` coordinates + labels and the P1 passenger name/phone — but only to a **party** (rider or vehicle owner), over TLS, transported plaintext at the crypto boundary exactly like the vehicle-GPS REST paths (NFR-3.25). The reactive WS frames (`ride_request_created` / `ride_status_changed`, `websocket-protocol.md` §4.7–4.8) are **summary-only** and deliberately carry **none** of the P1 place/passenger data: they emit the P0 ids/status/timestamps plus the P1 `riderId` (an opaque user cuid, same tier + handling as `AuthOkPayload.userId`) and the P1 `requesterName` (see below) and are **per-party unicast** (`Hub.SendToUsers`, never a vehicle-keyed broadcast). So the encrypted coordinates and PII never leave the server on the broadcast path — clients that need them refetch the party-only REST detail. Error-message construction on this surface uses opaque ids only (Rule CG-DC-2); the `409 conflict` / `404 not_found` reasons carry a status string or an id, never a coordinate/label/name.

> **Derived field — `requesterName` (MYR-229).** `RideRequest.requesterName` is a **projected, not persisted** field: `go_ride_requests` stores no requester-name column. The server resolves it per-read from the requester's (`rider_id`) identity across **three READ-ONLY tables in precedence order** (CG-DL-9 — name/email columns only, read **inline via correlated subselects in the same statement** as the ride row on every read/list/mutation return, so no separate lookup and no N+1): the Prisma-owned `"User"` table (legacy/web riders), then `go_identity_apple` (the Apple first-consent name — the real identity for Apple-native riders, MYR-264), then `go_users`. `COALESCE`/`NULLIF` fall through empty sources. The display value then follows the chain **first name → email local-part → `"Rider"`** (rest-api.md §7.8), omitted only when the rider has **no row in any of the three tables**. Widening beyond `"User"` (MYR-264) closed a gap where Apple-native riders — who have no `"User"` row — were omitted, so owners saw a client-side placeholder instead of the real name; it adds no new exposure surface (same P1 party-scope envelope). It is **P1 PII**: a real person's name, log-unsafe (**never logged** on any path), surfaced **only on party-scoped surfaces** — the party-only REST detail + list projections and the per-party-unicast WS summary frames — exactly matching the `riderId` party scope it annotates. No new stored column, so no encrypt/log-safe row above; the classification lives with the wire projection.

> **Derived field — `VehicleSummary.hasActiveRide` (MYR-233).** The `GET /api/vehicles` catalog flag (`rest-api.md` §7.0) is a **projected, not persisted** boolean: neither `go_ride_requests` nor the Prisma-owned `Vehicle` table stores it, so there is no encrypt / log-safe row above. It is computed read-time as a correlated `EXISTS` folded into the existing lean list query — `scheduled_for IS NULL AND status IN ('accepted','arrived','enroute')` on `vehicle_id`, character-for-character the predicate of the `uq_go_ride_requests_active_instant_vehicle` partial unique index (migration 0013, MYR-266) — so it costs one index-only probe per row (no N+1) and can never disagree with the accept guard it mirrors. **P0**, same tier as the sibling `Vehicle.status` (§1.3): it reveals only whether a car is in service right now. It is deliberately **not** derived from any P1 column — no pickup/dropoff coordinate, label, address, passenger name/phone, or `rider_id` is read, projected, or inferable from a single boolean, so the flag carries none of the party-scoped exposure that `requesterName` above does and is safe on the catalog surface that a non-party viewer may see. Log-safe (a bare boolean about a car, no PII). Present in BOTH `VehicleSummary` role allow-lists (`rest-api.md` §5.2.0 / `internal/mask/tables.go`) per Rule CG-DC-5.

> **Deprecated passenger fields (MYR-447).** `passenger_name` / `passenger_phone` are the only columns in this table with a retention window shorter than the row's, and the reason is that they have no reader. The book-for-someone-else feature was removed in [MYR-382](https://linear.app/myrobotaxi/issue/MYR-382); no live surface renders a terminal ride's passenger; nothing but the party-only REST projection reads them, and both JSON keys are `omitempty` so a scrubbed NULL simply removes the key rather than emitting an empty string. MYR-447 also **removed them from the internal event bus**: `events.RideAcceptedEvent` carried both on every accept and no subscriber ever read either — not `internal/dispatch`, not `internal/push`, not `internal/ws` — which made that struct the one place the in-process fan-out carried P1 PII for no purpose. Wire and DB surfaces are unchanged; only the internal event dropped the fields.

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

### 1.13 go_vehicle_control_state table (Go-owned, MYR-269; extended MYR-273, MYR-279, MYR-274, MYR-298, MYR-303, MYR-308, MYR-316, MYR-320, MYR-342)

Durable last-known owner-control read-back state — the four owner controls the app renders (Lock, Trunk/Frunk, Climate, Charge-port). These are the MYR-252 cabin read-backs that were stream-only with no persistence, so a `/snapshot` for a non-streaming car (in service / asleep / offline) always showed "Unavailable". Created by `internal/store/migrations/0008_vehicle_control_state.up.sql` under the CG-DL-9 `go_` namespace; written on the live persist path and — for the columns Tesla's cached `vehicle_data` subset actually carries — the MYR-260 `/vehicle_data` backfill (per-field COALESCE upsert, last-writer-wins), then LEFT-joined into `VehicleRepo.GetByID` for the REST `/snapshot`. The MYR-274, MYR-298 and MYR-303 columns are **stream-fed only**: `vehicle_data` carries no equivalent, so nothing backfills them. The MYR-308 `seat_cooling_capable` column is the exact inverse — **REST-fed only** (`vehicle_config.has_seat_cooling`), like `trim`; Tesla does not stream it, so it has no `fieldMap` entry and the MYR-300 gate below never drops it. The MYR-320 columns `trim_label` and `fsd_version` (migration **0018**) are REST-fed only for the same reason — `trim_label` from `vehicle_config.performance_package`, `fsd_version` from the TITLE of the newest `GET /api/1/vehicles/{vin}/release_notes` entry, which is the only place Tesla exposes it at all (no `vehicle_data` field, no proto) — and both are read on the same non-waking connectivity-edge path, now joined by a periodic in-service re-poll (`SERVICE_REPOLL_ENABLED` / `SERVICE_REPOLL_INTERVAL`, default 15m) so a car that never flips a connectivity edge still acquires them. Anchored: NFR-3.5 (snapshot completeness), NFR-3.9 (classification tiers). Every control column is nullable — NULL means "never read", surfaced as an honest "unavailable", never a fabricated on/off. **MYR-300:** the backfill is additionally gated on **stream recency** — it writes no stream-sourceable column while a live frame for that VIN arrived within the last 120s — so Tesla's possibly-cached `vehicle_data` can no longer overwrite fresher streamed state through the COALESCE upsert. No classification change: same columns, same tier, strictly fewer writes.

**MYR-435 — WIRE EXPOSURE IS NOW OWNER-ONLY FOR THIS ENTIRE TABLE.** No column's tier changed and no row below is edited: the P0/P1 labels still describe how each value must be handled at rest and in logs. What changed is *who receives it*. Per the client decision of 2026-08-02 ("Remove media/cabin and any vehicle controls"), every cabin, media and control read-back in this table is withheld from the `viewer` role on both the REST `/snapshot` and the WebSocket `vehicle_update` — see §2.4 and [`rest-api.md`](rest-api.md) §5.2.1.1. Most of these columns are P0, which is exactly the point: P0 means "not sensitive to leak", not "everyone with a share link should receive it". A **new column added to this table inherits owner-only exposure by construction**, because the viewer mask is an allow-list — but CG-DC-5 still requires the choice to be stated explicitly rather than defaulted.

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `vehicle_id` | `TEXT` (cuid) | P0 | No | Yes | Owning vehicle cuid — opaque; no Prisma FK (CG-DL-9) |
| `is_locked` | `BOOLEAN?` | P0 | No | Yes | Lock state — same tier as the MYR-252 `locked` wire field; not identifying, no GPS |
| `frunk_open` | `BOOLEAN?` | P0 | No | Yes | Front-trunk open state (`frunkOpen`) — cabin/door state |
| `trunk_open` | `BOOLEAN?` | P0 | No | Yes | Rear-trunk open state (`trunkOpen`) — cabin/door state |
| `is_climate_on` | `BOOLEAN?` | P0 | No | Yes | Derived climate on/off (`isClimateOn`) — cabin comfort state |
| `charge_port_open` | `BOOLEAN?` | P0 | No | Yes | Charge-port door open state (`chargePortDoorOpen`) — door state |
| `driver_temp_setting` | `INT?` | P0 | No | Yes | Driver temperature setpoint °F (`driverTempSetting`) — cabin comfort setting. MYR-273 (migration 0010) |
| `passenger_temp_setting` | `INT?` | P0 | No | Yes | Passenger temperature setpoint °F (`passengerTempSetting`) — cabin comfort setting. MYR-273 (migration 0010) |
| `fan_speed` | `INT?` | P0 | No | Yes | HVAC fan speed level (`fanSpeed`) — cabin comfort setting. MYR-273 (migration 0010) |
| `seat_heater_left` | `INT?` | P0 | No | Yes | Front-left seat heater level (`seatHeaterLeft`) — cabin comfort setting. MYR-273 (migration 0010) |
| `seat_heater_right` | `INT?` | P0 | No | Yes | Front-right seat heater level (`seatHeaterRight`) — cabin comfort setting. MYR-273 (migration 0010) |
| `seat_heater_rear_left` | `INT?` | P0 | No | Yes | Rear-left seat heater level (`seatHeaterRearLeft`) — cabin comfort setting. MYR-273 (migration 0010) |
| `seat_heater_rear_center` | `INT?` | P0 | No | Yes | Rear-center seat heater level (`seatHeaterRearCenter`) — cabin comfort setting. MYR-273 (migration 0010) |
| `seat_heater_rear_right` | `INT?` | P0 | No | Yes | Rear-right seat heater level (`seatHeaterRearRight`) — cabin comfort setting. MYR-273 (migration 0010) |
| `seat_cooler_left` | `INT?` | P0 | No | Yes | Front-left seat cooler level (`seatCoolerLeft`) — cabin comfort setting. MYR-273 (migration 0010). MYR-299: its NON-NULLness (including a stored `0`) additionally carries the ventilated-seat capability — same column, same P0 tier, no shape change |
| `seat_cooler_right` | `INT?` | P0 | No | Yes | Front-right seat cooler level (`seatCoolerRight`) — cabin comfort setting. MYR-273 (migration 0010). MYR-299: its NON-NULLness (including a stored `0`) additionally carries the ventilated-seat capability — same column, same P0 tier, no shape change |
| `media_volume` | `DOUBLE PRECISION?` | P0 | No | Yes | Media volume level (`mediaVolume`, fractional, typically 0-11) — cabin media setting, carries no content metadata. MYR-273 (migration 0010) |
| `software_version` | `TEXT?` | P0 | No | Yes | Installed Tesla firmware string (`softwareVersion`) — a publicly-legible attribute, not identifying, no GPS. MYR-279 (migration 0011). Streamed (proto Version) OR `/vehicle_data` `car_version` |
| `trim` | `TEXT?` | P0 | No | Yes | Trim badge (`trim`, e.g. "Performance") — a publicly-legible attribute, not identifying. MYR-279 (migration 0011). `/vehicle_data` `vehicle_config.trim_badging` only (not streamed) |
| `hvac_auto_mode` | `TEXT?` | P0 | No | Yes | HVAC auto mode (`hvacAutoMode`, enum string "On"/"Override") — cabin comfort state. MYR-274 (migration 0012). A streamed "Unknown"/empty persists NULL |
| `hvac_ac_enabled` | `BOOLEAN?` | P0 | No | Yes | A/C compressor enabled (`hvacAcEnabled`) — cabin comfort state. MYR-274 (migration 0012). A real `false` overwrites |
| `seat_vent_enabled` | `BOOLEAN?` | P0 | No | Yes | Seat ventilation enabled (`seatVentEnabled`) — cabin comfort state, same tier as the seat heater/cooler levels. MYR-298 (migration 0014). A real `false` overwrites. Stream-fed only (not in the `/vehicle_data` subset) |
| `media_playback_status` | `TEXT?` | P0 | No | Yes | Media playback status (`mediaPlaybackStatus`, enum string "Stopped"/"Playing"/"Paused") — cabin media state; carries no track title, artist, or account identifier, so it is not identifying. MYR-298 (migration 0014). A streamed "Unknown"/empty persists NULL. Stream-fed only (not in the `/vehicle_data` subset) |
| `media_now_playing_title` | `TEXT?` | **P1** | No | **No — redact** | Now-playing track title (`mediaNowPlayingTitle`) — free-text **user content**. MYR-303 (migration 0015). Empty string means "nothing playing" and OVERWRITES; NULL means never observed |
| `media_now_playing_artist` | `TEXT?` | **P1** | No | **No — redact** | Now-playing artist (`mediaNowPlayingArtist`) — free-text user content. MYR-303 (migration 0015). Same empty-vs-NULL semantics |
| `media_now_playing_album` | `TEXT?` | **P1** | No | **No — redact** | Now-playing album (`mediaNowPlayingAlbum`) — free-text user content. MYR-303 (migration 0015). Same empty-vs-NULL semantics |
| `media_now_playing_station` | `TEXT?` | **P1** | No | **No — redact** | Station/channel label (`mediaNowPlayingStation`) — free-text user content; a station is a direct taste signal and often a language/politics/religion signal. MYR-303 (migration 0015) |
| `media_playback_source` | `TEXT?` | **P1** | No | **No — redact** | Playback source/input (`mediaPlaybackSource`, e.g. an app name, `Bluetooth`, `USB`) — free text, deliberately not an enum. MYR-303 (migration 0015) |
| `media_now_playing_duration_ms` | `BIGINT?` | P0 | No | Yes | Track length in ms (`mediaNowPlayingDurationMs`) — a bare number, identifies nothing. MYR-303 (migration 0015). Tesla's `18000000` radio sentinel is stored verbatim |
| `media_now_playing_elapsed_ms` | `BIGINT?` | P0 | No | Yes | Playback offset in ms (`mediaNowPlayingElapsedMs`) — a bare number, identifies nothing. MYR-303 (migration 0015). Stale by construction once persisted |
| `media_volume_max` | `DOUBLE PRECISION?` | P0 | No | Yes | Per-vehicle volume ceiling (`mediaVolumeMax`) — a device capability, not user content. MYR-303 (migration 0015). Fractional like `media_volume`, which it bounds |
| `seat_cooling_capable` | `BOOLEAN?` | P0 | No | Yes | Ventilated-seat capability (`seatCoolingCapable`) — an equipment/spec fact, same tier as `trim`/`model`/`year`. MYR-308 (migration 0015). REST-only (`vehicle_config.has_seat_cooling`); a real `false` is authoritative, NULL means never read |
| `service_etc` | `TIMESTAMPTZ?` | P0 | No | Yes | Tesla's own estimated end of the current service visit — `service_data.service_etc` from the Fleet API `GET /api/1/vehicles/{vin}/service_data`. MYR-316 (migration 0017). Operational timing about the car, the same tier as `Vehicle.status`; not identifying, no GPS, no user content. OUTRANKS `service_expected_end_at` in the emitted `serviceEstimatedEndAt`. NULL means Tesla has no appointment record for this visit — common and normal, never a fetch failure |
| `service_expected_end_at` | `TIMESTAMPTZ?` | P0 | No | Yes | The owner's "expected back" answer, written ONLY by `PUT /api/tesla/vehicles/{vehicleId}/service-window` (rest-api.md §7.16). MYR-316 (migration 0017). Same tier and reasoning as `service_etc`; it is the FALLBACK, used only while `service_etc` is NULL. NULL means the owner has not entered one (or cleared it) |
| `trim_label` | `TEXT?` | P0 | No | Yes | Display-ready trim / performance designation (`trimLabel`, e.g. "Performance") — an equipment/spec fact, the same tier as its sibling `trim` and as `model`/`year`; not identifying, no GPS, no user content. MYR-320 (migration 0018). REST-only (`vehicle_config.performance_package`); Tesla does not stream it, so it has no `fieldMap` entry and the MYR-300 gate never drops it. NULL means never read, NOT "this car has no trim designation". **Wire exposure widened by MYR-507 (tier unchanged):** as of contracts v0.31.0 this column is ALSO emitted on the `GET /api/vehicles` catalog row as `VehicleSummary.trimLabel`, in **BOTH** role allow-lists, not only on the `/snapshot`. Same column, same read, no derivation on either surface. The reason is a VIEWER: a rider never fetches a `/snapshot`, so the catalog row is the only surface on which a shared car can say what it is, and with the trim withheld a rider's descriptor degraded to the raw colour token. It is an equipment fact of exactly the tier of the identity siblings `model`/`year`/`color` it now sits beside — all legible through a windshield — so there is nothing here for masking to protect. Its sibling `fsd_version` was deliberately NOT brought along |
| `fsd_version` | `TEXT?` | P0 | No | Yes | FSD software designation (`fsdVersion`, e.g. "FSD (Supervised) v14.3.5") — a publicly-legible software attribute, the same tier as its sibling `software_version`. MYR-320 (migration 0018). Sourced ONLY from the TITLE of the newest `GET /api/1/vehicles/{vin}/release_notes` entry — no `vehicle_data` field and no proto carries it — and stored VERBATIM, never parsed. NULL means never read, NOT "this car lacks FSD" |
| `nav_reading_at` | `TIMESTAMPTZ?` | P0 | No | Yes | When the car last reported NAVIGATION — the flush instant of the most recent frame carrying a navigation-group member (`destinationName`, `minutesToArrival`, `milesToArrival`, destination/origin location, `routeLine`). [MYR-409](https://linear.app/myrobotaxi/issue/MYR-409) (migration **0034**). A clock reading about one car: no coordinate, no place name, no distance, no duration, and it names nobody — the same tier as the sibling `updated_at`, unencrypted and log-safe. **Stream-fed only**, by a dedicated MONOTONE writer (`GREATEST(new, stored)`) rather than the shared COALESCE upsert, so two replicas and a coalescing flush cannot move it backwards. A motion-, charge- or climate-only frame does NOT move it, and neither does the MYR-454 derived-status fold — that is the entire point of the column. NULL means no navigation frame has ever been seen, which every freshness gate reads as NOT FRESH; there is deliberately no backfill and no fallback to the row stamp. **Not on the wire** — it is a server-side gate input, not a `VehicleState` field |
| `ride_share_enabled` | `BOOLEAN NOT NULL DEFAULT true` | P0 | No | Yes | The OWNER's ride-sharing switch (`rideShareEnabled`) — whether this car currently accepts ride requests at all. MYR-342 (migration 0021). Operational availability of the car, the same tier as `Vehicle.status`; not identifying, no GPS, no user content, and **log-safe in full**. **The only NOT NULL column in this table**, and the only one with a DEFAULT: every other column is honest-unknown-nullable, but this one has no unknown state — a car nobody has paused IS accepting rides. `false` means the owner has PAUSED requests; `true` is the ordinary state. Written ONLY by `PUT /api/tesla/vehicles/{vehicleId}/ride-share` (rest-api.md §7.18), never by Tesla and never by the stream |
| `updated_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |

> **No GPS, no PII (MYR-269).** All columns are P0 cabin/lock/door state — the same tier as the MYR-252 fields they persist, consistent with `speed`/`chargeLevel`/`gearPosition`. No coordinates, tokens, addresses, or names are stored, so no encryption or log-redaction is required. **Superseded in part by MYR-303:** the five now-playing text columns added by migration 0015 are the first **P1** columns in this table, and the blanket "log-safe" claim above no longer covers them — see the MYR-303 note below.

> **MYR-303 now-playing columns — the first P1 rows in this table.** The MYR-298 note below anticipated this exactly: "a now-playing *title* would be a different analysis". It is. `media_now_playing_title`, `media_now_playing_artist`, `media_now_playing_album`, `media_now_playing_station` and `media_playback_source` are **free-text user content**, and an accumulated stream of them reveals listening habits — taste, and by inference language, mood, politics, religion. They are therefore **P1**, deliberately stricter than the P0 `media_playback_status` and `media_volume` they sit beside.
>
> **Handling.** Redact in logs: never log the value, log only presence/length. Never emit outside the vehicle's party, never aggregate, and never retain as a listening history. No encryption at rest: the P1 encryption scope (§3) covers GPS coordinates specifically, and these are not location data; the control is redaction plus party-scoping, consistent with how the P1 `licensePlate` (MYR-286) is handled.
>
> **Logging-layer status (verified, MYR-303).** No redaction-list entry was required, because this server has no code path that logs a telemetry field's VALUE. `logFieldVerification` (`internal/telemetry/decoder.go`) is gated to a single observation-only proto (190, `EstimatedHoursToChargeTermination`) and never fires for a media field; the decoder's field-error path logs the field NAME and proto key only. The redaction obligation above is therefore a standing constraint on future code, not a change to existing code — the same conclusion MYR-286 reached for `licensePlate`. Any new value-logging path MUST exclude these five columns.
>
> **Visibility is BOTH ROLES, deliberately.** Owner AND viewer/rider of the vehicle receive all five on the `/snapshot`. That is the feature, not an oversight: a rider sitting in the car can already hear what is playing, and a now-playing panel that blanks for the passenger is the feature failing. Same reasoning as `licensePlate` (MYR-286) — what matters is who NEEDS the value, not the tier. `vin` remains the one owner-only `VehicleState` field. The FR-5.5 `limited_viewer` seam is the exception: those five MUST be excluded from that tier when it is implemented (see `rest-api.md` §5.2.1), because it is the deliberately-degraded tier for someone NOT in the car, where the "they can already hear it" justification does not hold.
>
> The three numeric MYR-303 columns (`media_now_playing_duration_ms`, `media_now_playing_elapsed_ms`, `media_volume_max`) stay **P0** — a bare track length, playback offset, or volume ceiling identifies nothing on its own, consistent with `media_volume`. They nonetheless travel alongside the P1 columns and MUST NOT be used to reconstruct or fingerprint a listening history.

> **MYR-308 `seat_cooling_capable`.** P0, and not a runtime state at all: it is an equipment/spec fact about the car — whether it is EQUIPPED WITH ventilated front seats — the same tier and the same reasoning as `trim`, `model` and `year`. Contrast the sibling `seat_vent_enabled`, which is the runtime on/off of that equipment. Sourced ONLY from REST `vehicle_data.vehicle_config.has_seat_cooling`; Tesla does not stream it, so it has no proto and no `fieldMap` entry — which is precisely what carries it past the MYR-300 stream-recency gate (that gate drops only fields derived from `fieldMap`), letting an in-service car acquire the capability with no stream at all. Nullable: NULL means "never read", NOT "no seat cooling" — clients fall back to the `seatCooler*`-presence heuristic on NULL, while an explicit `false` is authoritative. Visible to both roles.

> **MYR-316 service-window columns.** `service_etc` and `service_expected_end_at` (migration 0017) are both **P0** for the same reason the sibling `Vehicle.status` is: they are operational timing about the CAR — when a service visit is expected to end — not identifying data, not location, and not user content. A timestamp about a car in a shop correlates to no person, so they need no encryption and are **log-safe** in full (contrast the P1 free-text media columns above, which are redacted). They are **two columns on purpose**: the emitted `serviceEstimatedEndAt` is `COALESCE(service_etc, service_expected_end_at)` — Tesla's estimate outranks the owner's — and merging them would let a late Tesla estimate ERASE what the owner typed, while a WITHDRAWN Tesla estimate would fall back to NULL instead of back to the owner's answer. Sources are disjoint and neither is streamed: `service_etc` comes from the Fleet API `GET /api/1/vehicles/{vin}/service_data` on the ServiceStatusMonitor's connectivity-edge path for an in-service car (sharing the existing 45s per-VIN read debounce and the token that read already resolved), and `service_expected_end_at` only from the owner's §7.16 write. Tesla has no proto for a service ETC, so like `trim` and `seat_cooling_capable` these have no `fieldMap` entry and the MYR-300 stream-recency gate never touches them — and unlike every other column here they bypass the shared per-field COALESCE upsert entirely, because that upsert cannot express a NULL write and CLEARING is a first-class operation for both. Two lifecycle rules bound the exposure: the field is emitted **only** while the vehicle's `status` is `in_service`, and the monitor **physically clears both columns** when the car leaves service — so a service window is scoped to the visit it describes and cannot outlive it. Visible to BOTH roles on `/snapshot` and `GET /api/vehicles`: the value floors the rider's scheduling picker, so a rider who cannot see it cannot book correctly.

> **MYR-320 vehicle-details columns.** `trim_label` and `fsd_version` (migration **0018**) are both **P0** for the same reason their siblings `trim` and `software_version` are: they are equipment/software FACTS ABOUT THE CAR — what it is badged and what it runs — not identifying data, not location, and not user content, so they need no encryption and are **log-safe in full** (contrast the P1 free-text media columns above, which are redacted, and the P1 `Vehicle.licensePlate`, which is externally correlatable to a person). `trim_label` is the DISPLAY-READY twin of `trim`: `trim` keeps the raw `vehicle_config.trim_badging` badge code (e.g. `p74d`) for downstream classification and is not display-safe, `trim_label` carries `vehicle_config.performance_package` (e.g. "Performance") — two columns because the badge code and the human label are different values with different consumers, and collapsing them would lose one of them. `fsd_version` is likewise NOT a duplicate of `software_version`: that column is the installed FIRMWARE BUILD, this one is the FSD software DESIGNATION, and the two strings move independently with neither derivable from the other. **Neither is streamed** — Tesla has no proto for either, so like `trim`, `seat_cooling_capable` and the MYR-316 columns they have no `fieldMap` entry and the MYR-300 stream-recency gate never touches them, which is precisely what lets a busily-streaming or an in-service car acquire them anyway. `fsd_version` is additionally the only column here with **no `vehicle_data` source at all**: the TITLE of the newest `GET /api/1/vehicles/{vin}/release_notes` entry is the only thing Tesla exposes, and it is stored **VERBATIM** — the server never parses, normalizes or version-compares it, because the string's shape is Tesla's to change. Both are nullable: NULL means "never read", surfaced as an honest absent row, never a fabricated value — and specifically NOT "this car has no trim designation" or "this car lacks FSD". Visible to BOTH roles on `/snapshot`, matching `trim` and `software_version` exactly. **MYR-507 SPLIT the last clause of this note,** which used to end "deliberately absent from the vehicles-list row, which stays a thin catalog." That held for `fsd_version` and does not hold for `trim_label`. The thin-catalog argument was a byte-cost argument, and it was made without weighing the one consumer it hurt: a **viewer never fetches a `/snapshot` at all**, so for a rider the catalog row is the ONLY surface on which a car can identify itself. With `trim_label` withheld, a shared car's descriptor had nothing left but the colour, and a lock-screen Live Activity read `UltraRed` while a review-sheet row rendered the bare make `Tesla`. `trim_label` is therefore now on the catalog as `VehicleSummary.trimLabel` (contracts **v0.31.0**), in both role allow-lists, classified with the identity siblings `model`/`year`/`color` it belongs to. `fsd_version` **stays off**: it is a software designation, no catalog row renders it, and no descriptor degrades without it — the split is pinned by `TestMYR320DetailFieldsSplitOnTheVehiclesList` so a later change cannot carry it along by analogy. The raw badge code `trim` stays off both catalog roles too, since it is not display-safe at all.

> **MYR-342 ride-sharing switch.** `ride_share_enabled` (migration **0021**) is **P0** for the same reason the sibling `Vehicle.status` is: it is OPERATIONAL AVAILABILITY of the car — whether its owner is currently lending it out — not identifying data, not location, and not user content. A boolean about a car correlates to no person, so it needs no encryption and is **log-safe in full** (the §7.18 success line logs it beside the P0 `vehicle_id` / `user_id`, exactly as §7.16 logs its `cleared` flag). It is nonetheless the most *consequential* column in this table, because three access-control gates read it: ride-request create, owner accept, and the reservation sweeper (rest-api.md §7.8). Three properties are load-bearing and none is stylistic. **(1) NOT NULL DEFAULT true** — alone in this table, which is otherwise uniformly honest-unknown-nullable. There is no unknown state to be honest about: a car whose owner has never touched the toggle IS accepting rides, and a nullable column would have invented a third state every reader collapsed back to `true` anyway. The DEFAULT also backfills every pre-MYR-342 row to the correct prior behaviour, and the read path spells the same default again as `COALESCE(gcs.ride_share_enabled, TRUE)` so a car with no side-table row at all is indistinguishable from an explicitly-enabled one. **(2) It bypasses the shared per-field COALESCE upsert**, like the MYR-316 pair above but for a sharper reason: that form treats NULL as "leave alone" and so cannot express a write of `false`, which is the entire point of a toggle. **(3) It is deliberately ABSENT from `ControlStateUpdate`**, the struct the TELEMETRY writer fills — and this one is a security property, not a style choice. A slot there would let any routine frame from the car silently re-enable ride sharing on a vehicle its owner had paused. Keeping the write on its own statement, reachable only from the owner-authenticated §7.18 handler, means the pause can be lifted by exactly one actor: the owner. Asserted by `TestVehicleRepo_RideShareIsNotReachableFromTelemetry`. Not streamed and never will be — Tesla has no proto and no `vehicle_data` field for "this owner is lending their car out", because it is a MyRoboTaxi product fact — so like `trim`, `seat_cooling_capable` and the MYR-316/320 columns it has no `fieldMap` entry and the MYR-300 stream-recency gate never touches it. Visible to BOTH roles on `/snapshot` AND on `GET /api/vehicles`: the viewer is the party the value is about, and a rider who cannot see it learns the car is paused only from a `409 vehicle_unavailable` after composing a whole request.

> **MYR-298 seat-vent + media columns.** `seat_vent_enabled` and `media_playback_status` (migration 0014) are the last two MYR-252 cabin read-backs to become durable. Both are P0 cabin comfort/media state — the same tier as the seat heater/cooler levels and `media_volume` they sit beside. `media_playback_status` is an enum string only (`Stopped`/`Playing`/`Paused`); it carries **no track title, artist, album, or streaming-account identifier**, so it reveals nothing about the occupant and needs no encryption or log-redaction (contrast: a now-playing *title* would be a different analysis). Unlike the MYR-269/279 columns, **neither is written by the MYR-260 `/vehicle_data` backfill** — Tesla's cached `vehicle_data` climate subset carries neither value, so the live stream is their only source. Both surface on the owner `/snapshot` (owner mask allow-list since MYR-252) and are nullable: NULL means never read, rendered as an explicit `null`, never a fabricated value.

> **Table completeness (MYR-298).** The MYR-273 cabin-setting columns (migration 0010) and the MYR-274 climate-mode columns (migration 0012) were shipped without rows in this table; they are enumerated above as of MYR-298 so §1.13 lists every column the side table actually has. All are P0, unencrypted, log-safe — no tier changes, documentation only.

> **MYR-279 vehicle-detail columns.** `software_version` and `trim` (migration 0011) are the two owner-facing vehicle-DETAIL read-backs the app renders on the details sheet that the Prisma-owned `Vehicle` table does not carry. Both are P0 publicly-legible attributes (a firmware string / a trim badge) — not identifying, no GPS, no PII — so they need no encryption or log-redaction. `software_version` populates from the live stream (Tesla proto Version) OR the `/vehicle_data` `car_version`; `trim` populates ONLY from `/vehicle_data` `vehicle_config.trim_badging` (Tesla does not stream it). Both surface on the owner `/snapshot` (owner mask allow-list) alongside the existing side-table read-backs.

### 1.14 go_push_devices table (Go-owned, MYR-186)

The APNs device-token registry behind ride-lifecycle push notifications — one row per installed app instance. Created by `internal/store/migrations/0019_push_devices.up.sql` under the CG-DL-9 `go_` namespace, written by the §7.17 endpoints and read by the `internal/push` notifier to resolve a ride party into the set of phones to alert.

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `TEXT` (cuid) | P0 | No | Yes | Go-generated row id — opaque |
| `user_id` | `TEXT` (cuid) | P0 | No | Yes | Owning user cuid — opaque; no Prisma FK (CG-DL-9) |
| `device_token` | `TEXT` (UNIQUE) | **P1** | No | **No** | APNs device token — a device identifier AND a capability: whoever holds it plus the team's APNs key can push to that phone. Never logged in full; only an 8-character prefix (`push.tokenPrefix`), and never echoed into a response or error envelope. See §3.2 for why it is not app-level encrypted |
| `platform` | `TEXT` (enum, CHECK) | P0 | No | Yes | Push platform enum, `ios` only in v1; adding one is a schema change, not a write |
| `sandbox` | `BOOLEAN` | P0 | No | Yes | Whether the token was minted by a development/TestFlight build — selects the APNs gateway. A build-flavour flag, not identifying |
| `created_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |
| `last_seen_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp — refreshed on every re-registration |

> **Device-token secrecy (MYR-186).** The token is P1 for the same reason `go_refresh_tokens.token_hash` is: it is credential-adjacent. It differs in one decisive way, and that difference drives the classification. A refresh token can be stored as a one-way hash because the server only ever needs to *compare* it; an APNs token must be replayed **verbatim** to Apple on every send, so there is no hash to store and no derived form that works. App-level AES-256-GCM would therefore buy an encrypt/decrypt round-trip per notification against a threat it does not stop: a database-read attacker cannot push anything without `APNS_KEY_P8`, which lives in the environment and not in this table, and an attacker who holds *both* is already past any column-level control. **Log redaction is the control** (§3.2), and it is enforced in three places: the store never puts a token in an error string, the handler never echoes a rejected value, and every log line carries at most an 8-character prefix. There is **no Prisma FK** (CG-DL-9), so deleting a person leaves rows behind; the registry self-heals instead — APNs answers `410 Unregistered` for a dead installation and the sender deletes that row. **Not projected onto any wire field** (Rule CG-DC-5): no REST or WebSocket response carries a device token, so `internal/mask` needs no entry — the §7.17 responses deliberately return only booleans.

### 1.15 go_vehicle_shares table (Go-owned, MYR-184)

The vehicle-sharing grant table — one row per (owner → recipient → vehicle) grant, in either of its two live shapes: an unredeemed invite carrying a redeemable `code`, or an accepted viewer grant naming the person who redeemed it. Created by `internal/store/migrations/0020_vehicle_shares.up.sql` under the CG-DL-9 `go_` namespace, written by the `rest-api.md` §7.5 endpoints, and read by **every viewer access check on the server** — the `GetUserVehicles` access set, `ResolveVehicleAccess`, the §7.0 viewer merge, and the capability gates on §7.1 and §7.8. (§7.2–§7.4 are **owner-only again as of [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369)** and no longer consult this table at all.) Extended by `0024_vehicle_share_grant_flags.up.sql`, which replaced the fixed cumulative tier with the per-grant `allow_rides` / `suspended_at` flags, and by `0032_vehicle_share_grant_updated_at.up.sql` ([MYR-451](https://linear.app/myrobotaxi/issue/MYR-451)), which added the `updated_at` mutation stamp so a capability change can be dated after the fact.

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `TEXT` (cuid, PK) | P0 | No | Yes | Go-generated row id — opaque. **This is what log lines and error messages identify a row by**, precisely because two of its siblings are P1 |
| `vehicle_id` | `TEXT` (cuid) | P0 | No | Yes | Target vehicle cuid — opaque; no FK to the sibling schema (CG-DL-9) |
| `owner_user_id` | `TEXT` (cuid) | P0 | No | Yes | Granting owner's cuid — opaque; no FK (CG-DL-9). Carried in the `WHERE` clause of every owner-scoped mutation, so it is an access-control value as well as an identifier |
| `label` | `TEXT` | **P1** | No | **No** | **A person's name**, typed by the owner for their own list ("Mira Chen", "Mom", "Roommate"). NOT an email and NOT an identity — never matched against, resolved to, or validated as any account. Same tier and same treatment as `RideRequest.requesterName`. Owner-facing only: it is returned to the vehicle's owner and is **never** delivered to the invited party |
| `permission` | `TEXT` (enum, CHECK) | P0 | No | Yes | The invite-time **preset**: `live` \| `live_history` \| `rides`. **No longer a cumulative tier and no longer read by any gate** ([MYR-369](https://linear.app/myrobotaxi/issue/MYR-369)) — redemption maps it onto `allow_rides`, and an accepted row's wire `permission` is derived back from that flag. `live_history` is retired: still accepted at create and still admitted by the CHECK (existing rows carry it), but normalized to `live` before it is written. An authorization value describing a relationship, not identifying data — same classification as the sibling `role` on the wire |
| `allow_rides` | `BOOLEAN NOT NULL DEFAULT false` | P0 | No | Yes | **The per-grant, owner-editable ride capability** ([MYR-369](https://linear.app/myrobotaxi/issue/MYR-369), migration 0024). Whether this viewer may create a §7.8 ride request against the car as a non-owner. Read by three gates — ride create, owner accept, and reservation dispatch — and edited in place by §7.5.7 `PATCH`. NOT NULL because there is no honest-unknown state for a capability; `DEFAULT false` is the fail-closed direction, so a row written by a path that forgets the column grants the smaller thing. An authorization capability, the same tier as `permission` beside it |
| `suspended_at` | `TIMESTAMPTZ?` | P0 | No | Yes | **The owner's reversible pause on one grant** ([MYR-369](https://linear.app/myrobotaxi/issue/MYR-369), migration 0024). Non-NULL means SUSPENDED, and suspension **gates everything**: the row is excluded from the viewer-merge access set, so the catalog, snapshot, WebSocket, drives and rides all deny together and no capability flag survives it. NULL is the ordinary active state, which is why every pre-existing row is correct untouched. A **timestamp rather than a boolean** deliberately: it records WHEN, an access-control decision an owner may later need to reconstruct, in the same way `revoked_at` and `accepted_at` already do — and one column encoding one fact cannot disagree with itself the way a bool-plus-timestamp pair can. An authorization state change, not identifying data |
| `code` | `TEXT` | **P1** | No | **No** | **A live BEARER CREDENTIAL for vehicle access.** 6 characters, `[A-Z0-9]`, crypto-random, server-minted, valid 7 days. Anyone holding it can redeem it. See the note below |
| `status` | `TEXT` (enum, CHECK) | P0 | No | Yes | Lifecycle: `pending` \| `accepted` \| `revoked`. `revoked` is a server-side tombstone and has **no wire member** — a revoked row is never serialized |
| `created_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp — NOT reset by a resend |
| `expires_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp. Written and compared by the DATABASE clock (`NOW() + INTERVAL '7 days'`), so "expired" never depends on two clocks agreeing |
| `accepted_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Non-sensitive timestamp — null until redeemed |
| `accepted_by_user_id` | `TEXT?` (cuid) | P0 | No | Yes | The redeeming person's cuid — opaque; no FK (CG-DL-9). **Server-side only:** deliberately absent from the owner-facing `ShareInvite` wire shape, which identifies the person by the `label` the owner typed |
| `revoked_at` | `TIMESTAMPTZ?` | P0 | No | Yes | Non-sensitive timestamp — the tombstone stamp. Never serialized (see `status`) |
| `updated_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | P0 | No | Yes | **When this grant's capability set was last decided** ([MYR-451](https://linear.app/myrobotaxi/issue/MYR-451), migration 0032). Stamped by the two statements that move a capability — `queryPatchShare` (the owner's toggle) and `queryAcceptSharesByID` (redemption, which derives `allow_rides` from the preset) — in the same statement that moves the flags, so the timestamp cannot disagree with the values it dates. Revoke and resend deliberately do NOT stamp: each has a dedicated timestamp that says more, and folding them in would make a revocation look like a capability edit. No trigger competes for this column. Added because `allow_rides` was a bare boolean beside a `suspended_at` that recorded WHEN — so a withdrawn ride capability could not be placed in time, and an urgent beta report ("this viewer rode after I turned Rides off") could not be resolved either way from the row. A clock reading about a row, not about a person: it names no party and describes no location, exactly like `created_at` beside it |

> **Code secrecy (MYR-184).** `code` is P1 for a reason none of the other P1 values in this document share: it is not data *about* someone, it is a **capability**. Presenting it to `POST /api/invites/redeem` with any valid account attached grants live location access to somebody else's car. That makes it closest in kind to `go_refresh_tokens.token_hash`, and it differs in the same decisive way `go_push_devices.device_token` does (§1.14): the redeem lookup matches on the exact bytes, so there is no hash to store and no derived form that works. App-level AES-256-GCM would buy an encrypt/decrypt round-trip per redemption against a database-read attacker who, at that point, can read `accepted_by_user_id` and simply insert their own grant. **The controls are elsewhere, and there are four:**
>
> 1. **Never logged.** No log line in `internal/store/vehicle_share_*.go` or `internal/telemetry/share_*.go` carries a code — not on mint, not on redeem, not on failure. Rows are identified by `id`. The mint-collision path reports only that it could not find a free code, never the values it drew.
> 2. **Never echoed.** A rejected redemption's error envelope does not repeat the submitted code. An error message naming it would be a confirmation oracle for an enumerating caller — which is also why unknown, expired, and already-consumed codes all answer 404 with an **identical body**.
> 3. **Suppressed on read, in SQL.** The projection is `CASE WHEN status = 'pending' THEN code ELSE '' END`, so no read path can carry an accepted grant's code out of the database even by mistake; the handler applies the same rule again independently.
> 4. **Short-lived and rate-limited.** 7 days, and 10 redemption attempts per user per minute — the 36^6 space is only safe with a cap on guesses (`rest-api.md` §7.5.5).
>
> **Wire projection (Rule CG-DC-5):** `label`, `permission`, `status`, `allow_rides`, `suspended_at`, `code`, `created_at`, `expires_at` and `accepted_at` ARE projected, onto the owner-only `ShareInvite` object — see the `ResourceInvite` allow-list in `internal/mask/tables.go` and `rest-api.md` §5.2.5. `allow_rides` and `suspended_at` reach the wire as `allowRides` / `suspended` ([MYR-369](https://linear.app/myrobotaxi/issue/MYR-369)) and only on **accepted** rows — a pending invite has no grant for them to describe — and `suspended_at` projects as the BOOLEAN `suspended` rather than the timestamp: the instant is kept for the owner's audit trail, not handed to a consumer. The `viewer` role has **no entry** for that resource, so the fail-closed lookup yields deny-all. `owner_user_id`, `accepted_by_user_id`, `revoked_at` and `updated_at` are projected nowhere — `updated_at` ([MYR-451](https://linear.app/myrobotaxi/issue/MYR-451)) deliberately so: it is a forensic record for the operator, and the owner's client already knows when it issued the patch. `permission` additionally reaches the invited party as `VehicleSummary.sharePermission` (P0, §5.2.0 viewer list); no other column in this table is ever visible to a non-owner.
>
> **No FKs (CG-DL-9).** Deleting a person or a car leaves rows behind. They are inert — the read paths resolve the target through the sibling schema and find nothing — and the owner-offboarding path revokes a vehicle's grants explicitly (`RevokeSharesForVehicle`). **Since MYR-355 the mirror direction is covered too**: account deletion revokes every grant the departing person *redeemed* (`RevokeSharesReceived`), so neither end of a grant can outlive its parties as a live row.

---

### 1.16 Account deletion — classification note (MYR-355)

`DELETE /api/users/me` ([`rest-api.md`](rest-api.md) §7.6, sequence in [`data-lifecycle.md`](data-lifecycle.md) §3) **adds no column and no table**, so it changes no tier and does not move the P0/P1 counts. It is recorded here because it is the one operation that reads across *every* classified table at once, and three classification rules bind it.

**1. The audit metadata is P0 only (CG-DL-5), and the shape is closed.** The `account_deleted` row's `metadata` is exactly `{vehicleCount, driveCount, ridesCancelled, sharesRevoked, shareLabelsScrubbed, pushDevicesDeleted, savedPlacesDeleted, refreshTokensRevoked, hadPrismaUser}` (`store.AccountDeletionCounts`) — eight counts and one bool. The temptation this endpoint creates is real and specific: an operator triaging a failed deletion wants to know *which* account, and the P1 values (name, email, VIN, ride labels) are all sitting in the same function. None of them may enter the row. `hadPrismaUser` is the closest call and is P0 on the same reasoning as `sandbox` in §1.14 — it describes which *shape* of account this was, not who it belonged to. Two tests hold the line, and it is worth being precise about which half each one holds: `TestAccountDeleter_DeleteIdentity_WritesTheAuditRow` checks every key present against an allow-list, so an ADDED field fails the test and is therefore a deliberate decision rather than a drift — but a field silently REMOVED still passes, and the allow-list is not a full key-set equality assertion. `TestAccountDeletionAuditMetadataIsStructurallyP0` (MYR-447) covers the other axis: it asserts the value SHAPE, requiring every metadata value to be a number or a bool, so a future field carrying a name, an address, a label or a coordinate fails on its type alone before anyone has to guess what it might be called.

**2. The server log lines carry the user cuid and counts, and nothing else.** `account_deleted` (`store.AccountDeleter`) and `account_deletion_complete` (`telemetry.AccountDeletionHandler`) log the opaque cuid, the audit row id, and the same counts. The **P1 values the sequence necessarily handles in memory are never logged**: the `requesterName` resolved onto each cancelled ride (§1.9), the `label` on each revoked grant (§1.15), the `device_token` on each deleted registration (§1.14), the VIN of each torn-down car, and the deleted person's own name and email. The per-vehicle teardown's own log line is unchanged and already obeys this (MYR-258).

**3. Deletion does not lower any surviving row's tier.** Two categories of P1 data survive the deletion by design (data-lifecycle.md §3.3) and keep their classification:

- **Terminal ride rows where the deleted user was the rider.** The P1 encrypted pickup/dropoff coordinates (§3.1) and the P1 log-redaction-only labels, addresses and passenger fields (§3.2) are unchanged and stay P1 — they are the OWNER's data now, and the owner is still present to be protected. What changes is only the *resolution* of `requesterName`: with no identity row in any of the three sources, `requester_exists` is false and the field is **omitted from the wire** rather than degraded to the `"Rider"` literal. An omitted `requesterName` on the live path means precisely "this account was deleted", which the iOS client renders as "Former rider". **This is not an anonymization step and must not be described as one** — nothing was scrubbed, and the row remains keyed by the deleted person's cuid.
- **Revoked share tombstones on the cars the deleted person OWNED**, whose P1 `label` (another person's name) is retained. A revoked row is never serialized to any client (`status` has no `revoked` wire member), so the retained name reaches nobody; it stays P1 in the database and in logs. The mirror-image rows are no longer survivors: on a grant the deleted person REDEEMED the `label` is *their* name sitting in somebody else's list, and since MYR-447 step 5 of the §7.6 sequence scrubs that column to `''` ([`data-lifecycle.md`](data-lifecycle.md) §3.1).

The one P1 value the deletion genuinely destroys everywhere is the departing person's own `name` / `email`, across all three identity sources (§1.10, §1.11, and the sibling `User` row).

### 1.16 go_push_prefs table (Go-owned, MYR-349)

The per-person notification preferences behind the app's Settings switches — one row per signed-in person, five booleans. Created by `internal/store/migrations/0022_push_prefs.up.sql` under the CG-DL-9 `go_` namespace, written by the `rest-api.md` §7.19 endpoints, and read by the `internal/push` notifier before every fan-out.

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `user_id` | `TEXT` (cuid, **PK**) | P0 | No | Yes | Owning user cuid — opaque; no Prisma FK (CG-DL-9). The natural key: a person has exactly ONE preference set |
| `ride_lifecycle` | `BOOLEAN NOT NULL DEFAULT TRUE` | P0 | No | Yes | May the whole ride status class wake this person's phones — a request reaching an owner, and accepted/declined/arrived/enroute/completed/expired reaching a rider |
| `drive_started` | `BOOLEAN NOT NULL DEFAULT TRUE` | P0 | No | Yes | May a drive-start notification be delivered |
| `drive_completed` | `BOOLEAN NOT NULL DEFAULT TRUE` | P0 | No | Yes | May a drive-completed notification be delivered |
| `charging_complete` | `BOOLEAN NOT NULL DEFAULT TRUE` | P0 | No | Yes | May a charging-complete notification be delivered |
| `viewer_joined` | `BOOLEAN NOT NULL DEFAULT TRUE` | P0 | No | Yes | May a "somebody redeemed your invite" notification be delivered |
| `created_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp |
| `updated_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp — stamped on every write |

> **Why P0, next to a P1 sibling (MYR-349).** This table sits directly beside `go_push_devices` (§1.14) in the same feature and lands two tiers apart, which is worth stating rather than leaving to be inferred. `device_token` is P1 because it is a **capability**: whoever holds it plus the team's APNs key can push to that phone. These five booleans are a capability for nothing. They are not identifying, carry no location, name no ride, no car and no other person, and reveal only that somebody would rather their phone stayed quiet about one category — the same tier as the sibling `Settings` table's own preference flags (§1.8) and as `go_vehicle_control_state.ride_share_enabled` (§1.13), which is likewise an owner's own switch about their own thing. There is consequently **nothing to redact**: unlike every §7.17 log line, the §7.19 success line carries all five values in full.
>
> **No FK (CG-DL-9)**, so deleting a person leaves a row behind. Unlike `go_push_devices` there is no self-healing signal to lean on — APNs has a verdict about a dead installation but none about a preference — and none is needed: the row is inert, because nothing reads it except a fan-out that has already resolved that person into a ride party, and a deleted person is never a ride party again. An orphan here is five booleans nobody asks about.
>
> **NOT NULL DEFAULT TRUE is a classification-adjacent decision, not just schema.** Every column is non-nullable with an all-on default because there is no honest-unknown state to preserve: a person who has never opened Settings **is** receiving notifications. "No row", "row whose column was defaulted" and "explicitly enabled" are therefore indistinguishable to the notifier by design — which is what stops the migration silencing every account on the day it lands, since on that day nobody has a row at all.
>
> **Wire projection (Rule CG-DC-5):** all five booleans ARE projected, onto the §7.19 `GET` and `PUT` responses, to the **owning person only** (`/api/users/me` scope — the JWT subject IS the resource, so there is no mask entry to write and no other party can ever read them). `user_id` is deliberately **NOT** projected: it appears in neither request nor response, and a body carrying one is rejected by the strict decode rather than ignored. Every projected key is emitted **unconditionally, with no `omitempty`** — dropping `false` would erase the interesting half of every value.
>
> **These are DELIVERY preferences, not authorization** (§7.19). A category switched off is a silence, not a denial: it stops this service waking a phone and changes nothing about what the account may read. Nothing in the RBAC layer may consult this table.

---

### 1.17 go_saved_places table (Go-owned, MYR-321)

The account's saved Home and Work places — at most two rows per person, holding where they live and where they work. Created by `internal/store/migrations/0023_saved_places.up.sql` under the CG-DL-9 `go_` namespace, written and read exclusively by the `rest-api.md` §7.20 endpoints. Contract shape: `schemas/saved-places.schema.json` (contracts v0.21.0).

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `user_id` | `TEXT` (cuid, **PK part 1**) | P0 | No | Yes | Owning user cuid — opaque; no Prisma FK (CG-DL-9) |
| `kind` | `TEXT` (**PK part 2**, CHECK `home`/`work`) | P0 | No | Yes | Which of the two fixed slots — a closed enum, not a name the person chose |
| `lat_enc` | `TEXT` (ciphertext) | **P1** | **Yes** (encrypt-only) | No | GPS coordinate — NFR-3.23. New table: no plaintext column, no dual-write rollout |
| `lng_enc` | `TEXT` (ciphertext) | **P1** | **Yes** (encrypt-only) | No | GPS coordinate — NFR-3.23 |
| `label` | `TEXT` (CHECK 1–200 chars) | **P1** | No | No | Display address/name the person typed or picked — reveals location; log-redacted only (same rationale as `go_ride_requests.pickup_label`, §3.2) |
| `created_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp — preserved across a replace |
| `updated_at` | `TIMESTAMPTZ` | P0 | No | Yes | Non-sensitive timestamp — stamped on every write |

> **This is the most sensitive location the platform stores, and it is worth saying plainly.** §1.9 encrypts a ride's pickup and drop-off because they are P1 GPS. These columns are the same tier by the letter of NFR-3.23 but strictly worse by consequence: a ride coordinate is **where somebody went once**, whereas a saved home coordinate is **where they sleep**, it is durable rather than transactional, and it is re-read on every app launch. A leak here is not a leaked trip, it is a leaked address. The encrypt-only posture is therefore followed to the letter rather than argued down, and no exception is available for convenience.
>
> **Encrypt-only coordinates (no dual-write),** exactly as §1.9 describes for `go_ride_requests`. The table is new, so coordinates are written exclusively as AES-256-GCM ciphertext (`internal/store/saved_places_scan.go`, reusing the `vehicle_gps_encryption.go` float codec — `strconv` prec=-1, the shortest exactly-round-tripping decimal, byte-compatible with the TS helpers). There are no plaintext coordinate columns, no backfill tool and no `*_plaintext_remaining_total` gauge. `SavedPlacesRepo` requires an `Encryptor` at construction (**panics on nil**, so a deployment without a key fails at wiring rather than writing an address in the clear at the first request) and treats decrypt failure as a **hard read error** — there is no fallback column to fall back to, and surfacing a zeroed coordinate would route somebody to Null Island rather than home.
>
> **The ciphertext is not sortable, comparable or indexable, and nothing here wanted it to be.** Every access is a point lookup on `(user_id)` or `(user_id, kind)`, which the primary key already serves. There is no "places near me" query and no geospatial index, so encryption costs this surface nothing it was using — the reason the encrypt-only choice is free here and would not be on a search surface.
>
> **`label` is P1 but NOT app-level encrypted**, the same tier split §1.9 makes between `pickup_lat_enc` and `pickup_label`, and the same rationale as the reverse-geocoded strings in §3.2: a formatted address string is meaningfully coarser than the coordinate pair it came from, and the pair beside it already carries the precision. It is log-redacted either way. The 200-character `CHECK` is a size bound, not a classification decision.
>
> **No FK (CG-DL-9)**, so nothing cascades — and unlike §1.16 the orphan is NOT inert. A `go_push_prefs` row left behind is five booleans nobody asks about; a `go_saved_places` row left behind is **ciphertext of where a deleted person lives**, keyed by a cuid that no longer resolves to anybody and reachable by nothing but a table scan. It would never be read, never be reported, and never go away. Account deletion therefore deletes these rows **explicitly**, as step 8 of the `rest-api.md` §7.6 sequence and before the identity transaction. A **real delete, not a tombstone**: unlike a revoked `go_vehicle_shares` grant, which is evidence in the CAR OWNER's audit trail, a saved place is a person's own note to themselves, and no counterparty is owed a record that this account once knew its owner's address.
>
> **The absent state is the absent ROW.** Both coordinate columns are `NOT NULL`: a place without coordinates is not a place, so a kind never set and a kind deleted are indistinguishable and both read as absent. This is what lets the wire envelope be a sparse 0–2 array rather than a fixed pair with nullable members — which matters for classification too, since a nullable-coordinate row would put a permanently-empty P1 column in front of every read for the life of the feature.
>
> **Wire projection (Rule CG-DC-5):** `kind`, `label`, `latitude` and `longitude` ARE projected, onto the §7.20 `GET` list and the `PUT` echo, to the **owning person only** (`/api/users/me` scope — the JWT subject IS the resource, so there is no §5.2 mask entry to write and no other party can ever read them). **Sharing a car grants access to the CAR, never to the other person's address book:** a viewer, a co-owner and a ride counterparty all see nothing here, which is why the resource has no viewer-masked projection at all. `user_id` is deliberately **NOT** projected — it appears in neither request nor response, and a body carrying one is rejected by the strict decode rather than ignored. `created_at` / `updated_at` are not projected either; the client has no use for them and they would leak a behavioural signal (when somebody last moved house).
>
> **Nothing in this surface is ever logged.** No log line in the handler or the store carries a coordinate or a label — the §7.20 write and delete lines carry the P0 `user_id` and `kind` only. Validation failures name the offending FIELD and never echo its value, because an out-of-range coordinate is still a location and error envelopes end up in crash reporters and log aggregators. The account-deletion audit row records `savedPlacesDeleted` as a **count** (CG-DL-5), never the places themselves.

---

### 1.18 go_live_activities table (Go-owned, MYR-172, MYR-398)

The ActivityKit push-token registry behind the rider's Live Activity — one row per `(ride, party)`, holding the token that addresses **one running Activity** for the length of a single ride. Created by `internal/store/migrations/0025_live_activities.up.sql` under the CG-DL-9 `go_` namespace and extended by `0027_live_activity_progress.up.sql` and `0029_live_activity_alerted_phase.up.sql`, written by the `rest-api.md` §7.21 endpoints, and read by the `internal/push` lifecycle notifier and ETA ticker before every send. Contract shape: `schemas/live-activity.schema.json` (contracts **v0.28.0** — `LiveActivityContentState.progress` and `asOf` added additively by MYR-398; every other field byte-identical to v0.24.0). Anchored: NFR-3.9 (data tiers), NFR-3.21 (self-scoped surfaces).

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `TEXT` (cuid, PK) | P0 | No | Yes | Go-generated row id — opaque. **This is what log lines identify a row by**, because the only other candidate is P1 |
| `ride_request_id` | `TEXT` (**FK** → `go_ride_requests(id)` ON DELETE CASCADE) | P0 | No | Yes | The ride whose Activity this is — an opaque cuid, and **the first genuine foreign key in the `go_` namespace**. Half of the `(ride, party)` natural key and the predicate every send path selects on. See the FK note below |
| `user_id` | `TEXT` (cuid) | P0 | No | Yes | The party whose phone is showing the Activity — opaque; **no Prisma FK (CG-DL-9)**, so this stays an unenforced pointer even though its sibling is enforced. The other half of the natural key |
| `activity_push_token` | `TEXT` | **P1** | No | **No** | The ActivityKit update token — a capability: whoever holds it plus the team's APNs key can write to that phone's lock screen. Addresses ONE Activity rather than an installation, and **rotates mid-ride**, so it is not a stable identity the way `go_push_devices.device_token` is. Never logged in full; only an 8-character prefix (`push.tokenPrefix`), and never echoed into a response or error envelope. See §3.2 for why it is not app-level encrypted |
| `sandbox` | `BOOLEAN NOT NULL DEFAULT FALSE` | P0 | No | Yes | Whether the token was minted by a development/TestFlight build — selects the APNs gateway. A build-flavour flag, not identifying. Carried per-Activity rather than joined from `go_push_devices`, because a rider who declined the notification permission still gets a Live Activity and may have no device row at all |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT NOW()` | P0 | No | Yes | Non-sensitive timestamp — preserved across a token rotation |
| `updated_at` | `TIMESTAMPTZ NOT NULL DEFAULT NOW()` | P0 | No | Yes | Non-sensitive timestamp meaning **last touched** — stamped on every registration, every rotation, every end, and **every successful push**. Both the 24-hour sweep's predicate and the ETA ticker's ordering key: the ticker serves least-recently-pushed first, so without the push stamp a capped pass would shed the same Activities on every tick forever |
| `ended_at` | `TIMESTAMPTZ?` | P0 | No | Yes | The **tombstone stamp**: non-NULL means this Activity has been ended and no further push will address it. NULL is the ordinary active state, so a freshly upserted row is correct untouched. A timestamp rather than a boolean, like `suspended_at` in §1.15 — it records WHEN, and "when did this Activity stop being updated?" is the first question asked when a rider reports a stuck lock screen |
| `progress_leg` | `TEXT?` (CHECK `pickup`/`dropoff`) | P0 | No | Yes | **MYR-398, migration 0027.** Which leg the anchor below describes — `pickup` is the car driving to the rider (ride status `accepted`), `dropoff` is the car driving them onward (`enroute`). A closed two-member enum, not a name anybody chose. Stored rather than derived at read time because it is what makes the leg FLIP detectable: a mismatch against the leg being pushed discards the anchor instead of measuring leg two against leg one's distance |
| `progress_source` | `TEXT?` (CHECK `nav_distance`/`eta`) | P0 | No | Yes | **MYR-398.** Which quantity `progress_baseline` is measured in — Tesla's `tripDistanceRemaining` (miles) or `minutesToArrival` (minutes). An enum naming a UNIT; it says nothing about the car beyond which of its two readings was available |
| `progress_baseline` | `DOUBLE PRECISION?` | P0 | No | Yes | **MYR-398.** The leg's total in that unit — the value corresponding to progress 0. **Not location data and worth saying why it is not P1:** it is a scalar magnitude with no origin and no bearing. "Twelve miles" is a property of a journey the row's own P1 neighbours already describe more precisely, and it resolves to a place only for somebody who already holds the ride's endpoints. Same tier as the `Vehicle.tripDistanceRemaining` it is copied from (§1.3 nav group) |
| `progress_value` | `DOUBLE PRECISION?` (CHECK `0..1`) | P0 | No | Yes | **MYR-398.** The last fraction actually DELIVERED to this Activity, and the floor the next push clamps to — which is how "the track never runs backwards" becomes a property of the sequence the CLIENT observes. Unitless and endpoint-free; the same tier and the same reasoning as the wire field it feeds (`LiveActivityContentState.progress`, P0). Written only after APNs accepted a push |
| `progress_reading` | `DOUBLE PRECISION?` | P0 | No | Yes | **MYR-398.** The raw nav observation `progress_value` was derived from, in `progress_source`'s unit. Same tier and same argument as `progress_baseline` — a scalar magnitude with no origin and no bearing, copied from the P0 `Vehicle.tripDistanceRemaining` / `Vehicle.etaMinutes` it is read from (§1.3 nav group) |
| `progress_reading_at` | `TIMESTAMPTZ?` | P0 | No | Yes | **MYR-398.** When that observation was first seen to hold — a clock reading, tiered like every other timestamp on this table. It exists because `Vehicle."lastUpdated"` dates the ROW and not the nav fields: any telemetry write moves it, so a car that has gone quiet on navigation while something else keeps writing its row would otherwise pass the 3-minute freshness gate forever |
| `alerted_phase` | `SMALLINT NOT NULL DEFAULT 0` (CHECK `0..6`) | P0 | No | Yes | **MYR-398, migration 0029.** The highest Dynamic Island **auto-expand** phase this Activity has already been alerted at — `0` none, `1` Dispatch, `2` Enroute, `3` Arriving, `4` Arrived, `5` On trip, `6` Completed. A closed ordinal on a ladder, the counterpart of `progress_value`: it records what one phone HAS BEEN SHOWN, not a fact about the ride, which is why it lives on this table rather than on the ride. Not identifying and not a capability — the position of a ride in its own lifecycle is already carried, in plainer form, by the P0 `go_ride_requests.status` — and the value is on the send log line beside the `has_progress` boolean, because "why did the island open again?" is the operational question this feature gets asked |

> **The foreign key is deliberate, and it is the first one in the `go_` namespace.** No previous Go-owned migration declares a `REFERENCES` clause, and that is not an accident: **CG-DL-9 forbids this SQL from referencing the sibling app's Prisma-owned tables**, so every cuid pointing at a person or a car — `user_id` here, and every `user_id` / `vehicle_id` in §1.9, §1.14, §1.15 — is an unenforced pointer that self-heals instead. `go_ride_requests` is **different in kind: it is OURS**, created by migration 0002 under the same namespace, and CG-DL-9 has nothing to say about it. The FK earns its place because this row is meaningless without its ride and because **the ride already has hard-delete paths** — owner teardown issues a bare `DELETE` over `go_ride_requests` by vehicle, and account deletion cascades. `ON DELETE CASCADE` means those paths cannot leave behind a ROW addressing a ride that no longer exists; the alternative is an unenforced pointer plus a bespoke delete in every teardown path, which is three places to forget instead of zero. **What the cascade does not do is end the Activity.** That lives on a phone; deleting the row removes the only address we had for it and publishes no event, so a hard-delete path must END the Activities before it deletes the rides — owner teardown does, and the cascade is the cleanup rather than the notification. `user_id` stays unenforced, as it must — the enforcement asymmetry inside one row is exactly the CG-DL-9 boundary made visible.
>
> **The four MYR-398 anchor columns are NULLABLE WITH NO DEFAULT, and that is a classification-adjacent decision too.** NULL means "we do not know where this car is on its leg", which is the ordinary state of most rows most of the time — a car with no active nav route, a car whose telemetry has gone quiet, an Activity registered before dispatch. A `DEFAULT 0` would have been a lie with a number on it: the wire field is omitted entirely when unknown, precisely because `0` is the claim that the car has covered none of the distance. Nothing is backfilled; existing rows acquire an anchor on their next push if their leg is still running. **None of the four is ever logged** — the send line carries a `has_progress` BOOLEAN, not the fraction (§7.21.5) — although unlike `activity_push_token` that is an economy rather than a rule, since all four are P0.
>
> **Token secrecy (MYR-172), and why the tier decision is `go_push_devices`'s and not `go_refresh_tokens`'s.** `activity_push_token` is P1 for the same reason `device_token` is (§1.14): it is credential-adjacent, a capability rather than data about somebody. It takes the same §3.2 posture for the same mechanical reason — the sender must replay the **exact bytes** to Apple on every send, so unlike `go_refresh_tokens.token_hash` there is no one-way form that works, and an encrypt/decrypt round-trip per push would buy nothing against a database-read attacker who still cannot push without `APNS_KEY_P8`, an environment secret and not a column. **Log redaction is the control**, and it is enforced STRUCTURALLY rather than by convention, in four places: the store never puts a token in an error string; the handler never echoes a rejected value; every log line carries at most an 8-character prefix; and the APNs sender — the one place the token is interpolated into a string, since it forms the request path — percent-escapes it into the URL and **strips the `*url.Error` skin off every transport failure before wrapping it**, because that type prints the whole URL and the retry path logs it on any ordinary bad network day. Registration additionally refuses a non-hexadecimal token outright (§7.21.1), so a pathological value never reaches that code at all. The registry self-heals the same way too — APNs answers `410 Unregistered` for an Activity the rider dismissed or the system expired, and the sender drops that row **without touching the device registry**, because the ACTIVITY is gone, not the phone.
>
> **`ended_at` is a tombstone, not a delete, and that is a classification-adjacent decision.** A terminal ride sends one last update carrying `event: "end"` and then stamps the column; the row is kept because a deleted row would be silently re-created by a late registration (the ride-status event that produced the end can be republished — see the at-most-once caveat in §7.17), leaving a live token nobody will ever end. Rows are swept 24 hours after their last write, which also covers the row whose Activity died on the phone without ever telling us. The practical consequence for this document: **a P1 token outlives the ride it belongs to by up to 24 hours by design**, and the sweep — not a cascade and not the end call — is what bounds that window.
>
> **The content-state is not a REST or WebSocket surface, but it is not nothing.** What the sender delivers over APNs (`v`, `status`, `eta`, `vehicleName`, `destination`, `progress`, `asOf` — §7.21) never appears on any endpoint and reaches the device only over a token that identifies exactly one Activity, on exactly one ride, for exactly one rider. It nonetheless carries the **P1 `destination` label**, the one value the ride-lifecycle ALERT copy policy (`internal/push/copy.go`) refuses to put on a lock screen. That is a deliberate, narrow exception rather than an oversight: a Live Activity is the rider's own ride on the rider's own device, and a ride card that cannot say where the car is taking you is not the feature. It is never sent to an owner's Activity and never appears in an alert body — see `rest-api.md` §7.21. `asOf` (MYR-398) is P0 on the same test `progress` passes: a clock reading about the rider's own ride names no place, locates no car, and is meaningless to a reader who does not already hold the ride.
>
> **THE ISLAND ALERT IS AN ALERT BODY, AND THE COPY POLICY APPLIES TO IT IN FULL.** The six phase-change pushes carry an `aps.alert` dictionary, which is what makes iOS expand the island (§7.21.4). That dictionary renders as an ordinary banner on a device with no Dynamic Island and on a paired watch — a locked screen, to whoever is holding it — so it is governed by `internal/push/copy.go`'s payload policy and not by the narrow content-state exception above. Its title and body are **fixed constants naming no place, no person and no vehicle** ("Your ride" / "Your ride is here"); `destination` stays in the content-state, where the token's scope is the control, and never crosses into the alert. The alert is also the one place this surface sends PROSE at all, against §7.21.3's "the server sends the enum and never prose" — APNs offers no way to expand an island using copy the app already holds, and the localised alternative renders the raw key when the installed build's string table lacks it.
>
> **Wire projection (Rule CG-DC-5):** `activity_push_token` is projected onto **NO wire field at all** — no REST response and no WebSocket frame carries it, because the §7.21 responses deliberately return only booleans (`{"registered":true,"sandbox":…}` and `{"ended":…}`) — so `internal/mask` needs no entry for this table, and the §7.21 endpoints are self-scoped to the ride's RIDER, so there is no role dimension to mask across.

---

### 1.19 go_fleet_config_attempts table (Go-owned, MYR-448; extended MYR-489; first wire-exposed by MYR-491)

The fleet-config reconciler's per-vehicle retry schedule. The table is **self-draining**: a successful config push DELETEs the row (migration 0031), so a row's ABSENCE remains a meaningful, load-bearing fact rather than a gap.

**Presence is weaker than absence, and MYR-517 is why.** A row used to mean "this car is currently failing to stream", because only a failed attempt wrote one. That made the row a handle the self-heal machinery could not assume it had: prod showed a live onboarding whose link-time push did not answer `missing_key`, so no row was written, so the in-band pairing signal had nothing to land on. A row is now ALSO written as a **seed** — `attempt_count = 0`, `last_outcome = ''` — by the link-time provisioning hook on every door (fresh link, re-link, deliberate re-add) and by the applied-signed-command pairing write. A seed row is scheduling-identical to no row (the candidate query admits both at the same instant) and wire-identical to no row (`''` derives to `null`), and cardinality is still bounded by vehicles rather than by attempts or commands, because `vehicle_id` is the primary key.

Classified here for the first time because **MYR-491 made it wire-visible**. Nothing in this table crosses the wire *directly* — the four columns below are read together and collapsed into the single derived `setupState` object on `VehicleState` (rest-api.md §7.1) and `VehicleSummary` (§7.0) — but Rule CG-DC-5 attaches to the field, not to the column, so the source columns are enumerated.

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `vehicle_id` | `TEXT` (PK) | P0 | No | Yes | The car's opaque Prisma cuid. **No FK (CG-DL-9)** — a Go-owned table may not reference a Prisma-owned one. Not the VIN, so nothing here identifies a physical vehicle |
| `attempt_count` | `INTEGER NOT NULL DEFAULT 0` | P0 | No | Yes | Consecutive unsuccessful attempts, the input to the exponential backoff. A counter about OUR scheduling, not about the car or its owner. **Deliberately NOT wire-exposed** even now: it is an implementation detail of the retry curve, and an owner shown "attempt 6 of ∞" learns nothing actionable |
| `last_attempt_at` | `TIMESTAMPTZ NOT NULL DEFAULT NOW()` | P0 | No | Yes | When we last asked Tesla about this car. **Wire-projected** as `setupState.since` for `awaiting_virtual_key` / `token_failed` (MYR-491) — a clock reading about a server action, carrying nothing about the owner's whereabouts or behaviour |
| `next_attempt_at` | `TIMESTAMPTZ NOT NULL DEFAULT NOW()` | P0 | No | Yes | When the car becomes eligible again. Pure scheduling; **not wire-exposed**, and deliberately so — publishing it would invite a client to render a countdown to a retry that a pairing signal can cancel at any moment |
| `last_outcome` | `TEXT NOT NULL DEFAULT ''` | P0 | No | Yes | An INTERNAL label (`awaiting_virtual_key`, `synced_not_streaming`, `token_failed`, `forced_repush`, …), **never a raw Tesla response body** — that rule predates this issue and exists because a Tesla body can echo a full VIN (§2.1). **Wire-projected**, but NOT verbatim: it is mapped through a server-side derivation onto the three-member `setupState.state` enum, so the internal vocabulary can change without a wire break |
| `signed_command_at` | `TIMESTAMPTZ?` | P0 | No | Yes | **MYR-489, migration 0036.** When a signed command last APPLIED — proof the virtual key is paired and the car was reachable. **Wire-projected** as `setupState.since` for `configuring`. Note what it is NOT: it records that *a* command succeeded, never WHICH command, so it cannot reveal that the owner unlocked their car at 2am |
| `forced_repush_at` | `TIMESTAMPTZ?` | P0 | No | Yes | **MYR-489, migration 0036.** When the escalation last recreated this car's config, and the budget that rations it to one per pairing epoch. **Wire-projected** as `setupState.since` for `configuring` when it is the later stamp |

> **Why the whole table is P0.** Every column is either an opaque cuid, a counter, a clock reading about a server action, or a fixed internal label. There is no VIN, no coordinate, no token, no free text and no user content — nothing whose accumulation reveals anything about a person. The table describes what OUR reconciler did and when, which is the same tier as `Vehicle.status`. It is log-safe in full, and the reconciler already logs most of it (`fleet_config_synced_not_streaming`, `fleet_config_forced_repush`) with the VIN redacted to last-4 per §2.1.
>
> **Wire projection (Rule CG-DC-5):** four of the seven columns feed exactly one wire field, `setupState`, on two resources — `vehicle_state` (§7.1) and `vehicle_summary` (§7.0) — in **BOTH role masks** (`vehicleStateOwnerFields`, `vehicleStateViewerFields`, `vehicleSummaryOwnerFields`, and the viewer summary list by copy). The viewer entry is a deliberate product decision on the same footing as `rideShareEnabled`: a car that has never streamed sits at `status: "offline"` permanently (MYR-437), so a viewer without `setupState` cannot distinguish a broken shared car from an unfinished one. The derivation is a **narrowing**, not a projection — three enum members out of seven-plus internal outcomes, plus one timestamp out of four — and that is intentional: the internal scheduling vocabulary stays free to change, and nothing about our retry cadence leaks to a client.
>
> **`attempt_count` and `next_attempt_at` are withheld ON PURPOSE, and the reason is a classification one.** Both are P0 and both would be harmless to publish, so the decision is not about tier — it is that neither is ACTIONABLE, and a card whose whole contract is "name the one thing left to do" must not carry numbers its reader cannot act on. Adding either would require a fresh CG-DC-5 entry here and a §7.0/§7.1 field row, which is exactly the friction that should stand in the way.
>
> **A THIRD resource projects the same four columns as of MYR-505: the §7.23 complete-setup response.** `POST /api/tesla/vehicles/{vehicleId}/complete-setup` answers with the same derived `setupState` object, built by the same server-side helpers from the same four columns, so **no new column is exposed, no new column is added, and there is no migration**. Two differences from the read surfaces are worth recording here because they are contract, not implementation. (1) The object is **never null** on that surface — a request that asked the server to finish setting up a car always gets an answer — whereas `null` remains the ordinary no-claim value on §7.0/§7.1. (2) The enum is a **superset**, carrying a fourth member `streaming` that §7.0/§7.1 never emit; it is derived from `Vehicle.status` and `Vehicle."lastUpdated"` (both already P0 and already wire-exposed on both read surfaces), not from this table, so it widens the projection's VOCABULARY without widening its SOURCES. **Role dimension: none.** The endpoint is owner-only and self-scoped, so unlike §7.0/§7.1 there is no viewer mask to enumerate and `internal/mask` needs no entry for it.

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
| License plate (`licensePlate`) | Omit entirely — from log lines, error envelopes, and metric labels alike. Log the P0 `vehicle_id` / `user_id` instead, plus a shape marker (e.g. `cleared=true`) if the set-vs-clear distinction is needed. This binds the MYR-286 write path in particular: the §7.14 `PUT .../plate` handler must not echo a rejected plate back in its `400 invalid_request` message (the envelope describes the RULE only) and must not log the accepted value on success. |
| User identity (`User.name`, `User.image`) | Omit entirely. Use user ID instead. |

### 2.3 P2 (Access-logged) — P1 rules plus audit trail

No P2 fields exist in v1. When P2 fields are introduced:

- All P1 redaction rules apply.
- Every read or write of a P2 column MUST emit an audit log entry containing: timestamp, actor user ID, operation (read/write), column name, and target record ID.
- Audit log entries themselves are classified P0 (they contain only opaque IDs, not the actual P2 values).

**Partial precedent (MYR-447).** No column's tier changed, but the mechanism this section describes now exists and is in use. Operator tooling that decrypts P1 columns emits a [`data-lifecycle.md`](data-lifecycle.md) §4.2 `operator_decrypt` row carrying the timestamp, the actor (`metadata.operator` — see §4.4 there on why the handle is P0, and note the actor is deliberately NOT the `userId` column, which remains the data subject), the operation (the action enum, a read), the column names (`metadata.fields`) and the target record id. A future P2 column should reuse `store.OperatorAuditor` rather than inventing a second access-log path.

What MYR-447 does NOT do is extend this to the SERVER's own reads of P1 columns: the telemetry pipeline decrypts location on every broadcast, and a row per read would be a volume problem, not an audit trail. The operator audit covers *human* access through internal tooling, which is the unaccountable path the MYR-427 privacy audit actually found.

### 2.4 Role exposure — what a shared VIEWER may see (MYR-435)

> **Client decision, 2026-08-02 ([MYR-435](https://linear.app/myrobotaxi/issue/MYR-435)):** *"Remove media/cabin and any vehicle controls."* Arising from the [MYR-427](https://linear.app/myrobotaxi/issue/MYR-427) privacy-policy audit.

**Tier is not the same question as role.** §1's P0/P1/P2 labels answer *"how carefully must this value be handled — logs, encryption, retention?"*. This section answers a different question: *"may a person the owner shared their car with see it at all?"* Most of what MYR-435 withheld from viewers is **P0** — `locked`, `interiorTemp`, `fanSpeed` and the rest identify nobody and are perfectly log-safe. They are withheld anyway, because P0 means "not sensitive to leak", not "everyone with a link should receive it". Conflating the two is precisely how the pre-MYR-435 mask came to hand a viewer the owner's now-playing track: every media field was correctly tiered, and the role question was never asked.

**Stated as an allow-list, deliberately.** The MYR-427 audit's structural finding was that the viewer mask was a SUBTRACTION (owner-minus-`vin`), so every field added to the owner list reached viewers by default, silently, decided by whoever did not think about it. Subtraction masks rot. The viewer set is therefore enumerated positively, and the enumeration is canonical in [`rest-api.md`](rest-api.md) §5.2.1.1 — **not duplicated here**, because two copies of a field list is how they drift. The summary:

| | Viewer receives | Viewer does NOT receive |
|---|---|---|
| **Live vehicle state** (`vehicle_state`) | Location (`latitude`, `longitude`, `heading`, `speed`, `locationName`, `locationAddress`); navigation + route + live trail; identity (`name`, `model`, `year`, `color`, `trim`, `licensePlate`, …); charge (`chargeLevel`, `chargeState`, `estimatedRange`, `timeToFull`); availability (`status`, `gearPosition`, `rideShareEnabled`, `serviceEstimatedEndAt`, `setupState`); counters (`odometerMiles`, `fsdMilesSinceReset`); freshness (`lastUpdated`) | The full `vin` (§1.3, §2.1 — MYR-279); **all** media/now-playing fields; **all** cabin-climate fields incl. `interiorTemp` and `exteriorTemp`; **all** vehicle-controls state (`locked`, `chargePortDoorOpen`, `frunkOpen`, `trunkOpen`, `virtualKeyPaired`) |
| **Vehicles list** (`vehicle_summary`) | The full catalog row plus `sharePermission` | The full `vin` (the catalog carries `vinLast4` only). The list has never carried media, cabin or controls state, and a test now guards that it stays that way. |
| **Drives** (`drive_summary` / `drive_detail` / `drive_route`) | — | Everything: drives are owner-only ([MYR-369](https://linear.app/myrobotaxi/issue/MYR-369)). |

**The withheld groups, and why** (full reasoning in `rest-api.md` §5.2.1.1):

- **Media / now-playing.** The five free-text fields are P1 user content; accumulated, they reveal listening habits, and by inference language, mood, politics, religion. The prior justification for sharing them — *"a rider in the car can already hear it"* — is **retired**, and the reason is worth recording: a share grant is durable and REMOTE. It keeps streaming while the grantee is at home and the owner is driving alone. The defense only ever held for the minutes someone was actually in the passenger seat, and the mask cannot tell the difference.
- **Cabin climate.** The cabin is the owner's private space, and `interiorTemp` is a live occupancy signal — it says someone has been sitting in that car with the heat on. `exteriorTemp` is the genuinely ambiguous member and was resolved toward removal per the issue's default-to-REMOVE rule.
- **Vehicle controls.** These exist to render the OWNER's control tiles. A viewer cannot actuate any of them, so the state serves no viewer feature — while disclosing whether the owner's car is standing unlocked with the trunk open.

**Consequence for §1's tables:** no column's tier changed. `go_vehicle_control_state` (§1.13) is still P0/P1 exactly as listed; what changed is that its wire exposure is now **owner-only**. A future column added to that table inherits owner-only exposure by construction — the mask is an allow-list, so a field nobody adds to `vehicleStateViewerFields` never reaches a viewer.

**Standing rule, and the trap it caught here.** A field the viewer mask removes MUST be OPTIONAL in the resource's JSON Schema. MYR-435 hit this: `interiorTemp` and `exteriorTemp` were in the `required` list of [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json), so masking them would have made every viewer snapshot invalid against the shape its own consumer decodes — and for a consumer with generated non-optional bindings, would have failed the decode outright rather than degrading. Both were moved out of `required` in the same change. See `rest-api.md` §5.2.1.2 for the consumer-regeneration requirement.

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
| `Vehicle` | `locationName` | `String` | `Text` (base64 ciphertext) | Reverse-geocoded place name for the current position (MYR-447) |
| `Vehicle` | `locationAddress` | `String` | `Text` (base64 ciphertext) | Street address for the current position (MYR-447) |
| `Vehicle` | `destinationName` | `String?` | `Text` (base64 ciphertext) | Navigation destination place name (MYR-447) |
| `Vehicle` | `destinationAddress` | `String?` | `Text` (base64 ciphertext) | Navigation destination street address (MYR-447) |
| `Drive` | `routePoints` | `Json` | `Text` (base64 ciphertext) | Full GPS trail for drive playback |
| `Drive` | `startLocation` | `String` | `Text` (base64 ciphertext) | Place name where the drive began (MYR-447) |
| `Drive` | `startAddress` | `String` | `Text` (base64 ciphertext) | Street address where the drive began (MYR-447) |
| `Drive` | `endLocation` | `String` | `Text` (base64 ciphertext) | Place name where the drive ended (MYR-447) |
| `Drive` | `endAddress` | `String` | `Text` (base64 ciphertext) | Street address where the drive ended (MYR-447) |
| `go_ride_requests` | `pickup_lat_enc` | `Float` (logical) | `Text` (base64 ciphertext) | Ride pickup latitude — encrypt-only, no plaintext column (MYR-173) |
| `go_ride_requests` | `pickup_lng_enc` | `Float` (logical) | `Text` (base64 ciphertext) | Ride pickup longitude — encrypt-only |
| `go_ride_requests` | `dropoff_lat_enc` | `Float` (logical) | `Text` (base64 ciphertext) | Ride drop-off latitude — encrypt-only |
| `go_ride_requests` | `dropoff_lng_enc` | `Float` (logical) | `Text` (base64 ciphertext) | Ride drop-off longitude — encrypt-only |
| `go_saved_places` | `lat_enc` | `Float` (logical) | `Text` (base64 ciphertext) | Saved Home/Work latitude — encrypt-only, no plaintext column (MYR-321) |
| `go_saved_places` | `lng_enc` | `Float` (logical) | `Text` (base64 ciphertext) | Saved Home/Work longitude — encrypt-only |

> **MYR-447 — the eight label rows.** These are the only rows in this table whose ciphertext column shares a name-plus-`Enc` convention with a plaintext column that is NOT dropped in the same change. `Vehicle` and `Drive` are Prisma-owned, so CG-DL-9 bars a Go migration from adding the `*Enc` columns: the DDL ships as a deploy artifact at [`docs/migrations/myr-447-prisma-label-enc.sql`](../migrations/myr-447-prisma-label-enc.sql) and MUST be applied as a `react-frontend` Prisma migration **before** the telemetry server deploys — every label read fails outright against a database that has not run it. `cmd/backfill-location-labels` seals the legacy values; `cmd/purge-plaintext-columns` scrubs the residue afterwards.
>
> All eight `*Enc` columns are NULLABLE, and that is load-bearing rather than incidental. NULL is the absent sentinel, which is the only way the geocode backfill's discovery predicate can be written: AES-GCM ciphertext is nondeterministic (fresh nonce per encrypt), so `"startAddress" = ''` — the pre-MYR-447 predicate — could never match a sealed value again. It is now `"startAddressEnc" IS NULL`.

### 3.2 P1 columns NOT encrypted (rationale)

These P1 columns are sensitive and must never appear in logs, but are NOT encrypted at rest because they are human-readable strings that do not carry coordinate-precision location data or credential material. They benefit from database-level encryption (Supabase encrypts at the disk level) but do not require application-level AES-256-GCM:

| Table | Column | Rationale for no app-level encryption |
|-------|--------|---------------------------------------|
| `User` | `name` | Prisma-owned; disk encryption sufficient for display names |
| `User` | `email` | Prisma-owned; disk encryption sufficient; not queried by telemetry server |
| `User` | `image` | URL to avatar; disk encryption sufficient |
| `Vehicle` | `licensePlate` | Disk encryption sufficient. **Updated by MYR-286:** the telemetry server now both reads this column (on `/snapshot` + the vehicles list) and writes it (the §1.4 owner carve-out backing `PUT .../plate`), so the old "not queried by telemetry server" rationale no longer holds. App-level encryption is still not warranted: the value is short, owner-supplied, and read on every catalog row — encrypting it would force a decrypt per row on the hot list path for a value the owner is deliberately publishing to their own riders. Log redaction (§2.2) plus party-scoped masking is the control. |
| `Invite` | `email` | Prisma-owned; disk encryption sufficient |
| `TripStop` | `name` | Prisma-owned; disk encryption sufficient |
| `TripStop` | `address` | Prisma-owned; disk encryption sufficient |
| `go_ride_requests` | `pickup_label` | Place name, not coordinate data (same rationale as `Drive.startLocation`) |
| `go_ride_requests` | `pickup_address` | Street address, not coordinate-precision data |
| `go_ride_requests` | `dropoff_label` | Place name, not coordinate data |
| `go_ride_requests` | `dropoff_address` | Street address, not coordinate-precision data |
| `go_ride_requests` | `passenger_name` | **DEPRECATED (MYR-447) — retained at 30 days from terminal, and cleared table-wide once at deploy.** Display name; disk encryption sufficient (same rationale as `User.name`). The promote-to-encryption question is now moot for the going-forward case: the column has no writer left, so the correct control is the shorter window rather than a stronger cipher over data that should not exist |
| `go_ride_requests` | `passenger_phone` | **DEPRECATED (MYR-447) — same 30-day window and same one-time scrub, and always scrubbed together with `passenger_name`.** PII, never logged; disk encryption sufficient today. The prior note said "promote to app-level encryption if threat modeling changes"; MYR-447 answers that differently and it is worth recording why — **retiring the data beat encrypting it**. The feature that wrote it is gone, so the cheapest control that fully closes the exposure is deletion, and an AES round-trip on a column with no reader would have been effort spent protecting something that had no reason to persist |
| `go_saved_places` | `label` | **MYR-321.** Display address/name for a saved Home or Work place — the same rationale as `go_ride_requests.pickup_label` and `Drive.startLocation`: a formatted address string is meaningfully coarser than the coordinate pair it came from, and the `lat_enc`/`lng_enc` pair beside it already carries the precision under AES-256-GCM. Worth naming that this sits on the platform's most sensitive coordinate row (§1.17) and is still not promoted: encrypting it would protect a string a database-read attacker could reconstruct from the row's own context, while the coordinates — the part that is actually precise — are already sealed. Log redaction is the control, and it is absolute here: no handler or store line carries this value, and a `400` names the field without echoing it |
| `go_push_devices` | `device_token` | **MYR-186.** Must be replayed VERBATIM to Apple on every send, so unlike `go_refresh_tokens.token_hash` there is no one-way form that works — an encrypt/decrypt round-trip per notification would buy nothing against a database-read attacker, who still cannot push without `APNS_KEY_P8` (an environment secret, not a column). Log redaction is the control: only an 8-character prefix is ever logged, and the value is never echoed into a response or error envelope |
| `go_live_activities` | `activity_push_token` | **MYR-172.** The same tier decision as `go_push_devices.device_token` above, taken for the same mechanical reason and with the same control: the ActivityKit update token is replayed VERBATIM to Apple on every Live Activity push, so there is no hashed or derived form that works, and encryption would cost a round-trip per push against an attacker who still cannot reach a lock screen without `APNS_KEY_P8`. Two differences are worth naming and neither changes the tier: it addresses **one running Activity** rather than an installation, and it **rotates mid-ride** — so the exposure window is a single ride rather than the life of an install, and a leaked token stops working when ActivityKit next rotates or the ride ends. Log redaction is identical (8-character prefix only, never echoed into a response or error envelope), and the §7.21 responses carry no token at all |

> **Design decision (revised by MYR-447).** Application-level encryption is reserved for columns where a database breach would expose precise geolocation trails or credential material. Until MYR-447 that scope excluded human-readable location strings on the grounds that a place name is coarser than the coordinate pair it came from. On `Vehicle` and `Drive` that reasoning did not survive contact with the threat it was written against: a street address needs no map and no tooling to read, so sealing the latitude beside it and leaving `locationAddress` in plaintext protected almost nothing an operator with a psql prompt actually wanted. Those eight columns are now in the §3.1 scope.
>
> The remaining entries in this table are still correct on their own terms, and the distinction is worth stating precisely so the next reviewer does not read this as a general reversal:
>
> - **`go_ride_requests.pickup_label` / `pickup_address` / `dropoff_*` and `go_saved_places.label` are NOT promoted.** Those tables are Go-owned and encrypt-only — there is no plaintext coordinate column beside them at all, so the label is genuinely the coarsest thing in the row rather than a readable restatement of a sealed neighbour. Promoting them would also cost a decrypt on the ride-list hot path for a string a database-read attacker can approximate from the row's own context.
> - **Capability-shaped columns (`go_push_devices.device_token`, `go_live_activities.activity_push_token`, `go_vehicle_shares.code`) are NOT promoted**, for the mechanical reason recorded against each: the exact bytes are replayed or matched, so no derived form works.
> - **Name/PII columns (`User.name`, `User.email`, `Vehicle.licensePlate`, …) are NOT promoted**: disk encryption plus log redaction remains the control.
>
> If threat modeling changes (e.g., multi-tenant deployment, regulatory requirements), the remaining columns can be promoted by adding them to the AES-256-GCM scope.

### 3.3 Encryption implementation contract

- **Algorithm:** AES-256-GCM (authenticated encryption with associated data).
- **Canonical primitive:** [`internal/cryptox.Encryptor`](../../internal/cryptox/encryptor.go) is the only entry point for column-level encryption. The store layer uses the interface; never call `crypto/aes` or `crypto/cipher` directly from outside `internal/cryptox/`.
- **Key source:** `ENCRYPTION_KEY` environment variable (Fly.io secret, NFR-3.24) for single-key deployments. During key rotation, switch to the versioned shape: `ENCRYPTION_KEY_V{N}` plus `ENCRYPTION_WRITE_VERSION`. See [`key-rotation.md`](key-rotation.md) for the full env-var schema and rotation procedure (NFR-3.26).
- **Ciphertext format:** `[1B version][12B nonce][N B ciphertext + 16B GCM tag]`, base64-encoded as `Text` in PostgreSQL. The version byte routes the decrypt path to the matching key in the active `KeySet`, enabling in-place key rotation without re-encrypting old rows up front. Version `0x00` is reserved as invalid; `0x01` is the only emitted version today.
- **Nonce:** 12-byte cryptographically-random nonce per call (NIST SP 800-38D §5.2.1.1). Catastrophic GCM failure mode is nonce reuse; the package guarantees fresh nonces per `Encrypt` call.
- **Transparency:** Encrypt on write, decrypt on read, entirely within the store layer (NFR-3.25). The SDK, WebSocket broadcaster, and REST API handlers operate on plaintext values.
- **Foundation complete (as of MYR-65, 2026-05-09):** every P1 column identified by §3.1 now has an encrypt-on-write path; the unfinished-rollouts list is empty. The `internal/cryptox` package + startup wiring are landed; `ENCRYPTION_KEY` is required at boot. **OAuth tokens (`Account.access_token`, `Account.refresh_token`, `Account.id_token`) are encrypted on disk as of MYR-62** via the dual-write rollout (TS Phase 1 in `myrobotaxi/react-frontend#256`, Go Phase 2 in this repo). **Vehicle GPS coordinates (`Vehicle.latitude`/`longitude`, `destinationLatitude`/`destinationLongitude`, `originLatitude`/`originLongitude`) are encrypted on disk as of MYR-63** via the dual-write rollout (TS Phase 1 in `myrobotaxi/react-frontend#257`, Go Phase 2 in this repo). **Route-blob polylines (`Vehicle.navRouteCoordinates`, `Drive.routePoints`) are encrypted on disk as of MYR-64** via the dual-write rollout (TS Phase 1 in `myrobotaxi/react-frontend#258`, Go Phase 2 in this repo). Operators monitor rollout completion via `account_token_plaintext_remaining_total{column=...}`, `vehicle_gps_plaintext_remaining_total{column=...}`, and `route_blob_plaintext_remaining_total{column=...}` (each gauge → 0 once every legacy row has been backfilled), and rotation observability via `cryptox_decrypt_total{version="N"}` (wired in MYR-65; per-version successful-decrypt counter on the existing `/metrics` endpoint, used to confirm the retiring version's series decays to zero before the key is removed — see [`key-rotation.md`](key-rotation.md) §"Procedure" step 6).

#### 3.3.1 Dual-write retired (MYR-433, 2026-08-02)

The three rollouts above (MYR-62 tokens, MYR-63 vehicle GPS, MYR-64 route blobs) shipped as **dual-writes**: ciphertext AND plaintext, with the read path preferring ciphertext and falling back. That regime is over on this server. Reads select only the ciphertext columns; writes populate only the ciphertext columns; no read path has a plaintext fallback.

The dual-write was never wrong for the reason it existed — it bought roll-back safety during a cross-repo rollout. But it meant the classification tables above described encrypt-at-rest while a fully readable copy of every token and coordinate sat in the next column over. An operator with a database connection, or anyone holding a leaked dump, could read a user's home, their route, and a credential that drives their car. The MYR-427 privacy audit found the gap; MYR-433 closed it against this client bar:

> "I should not be able to go into the db and see users drive or if there's a db leak it should be encrypted."

**Key custody.** The bar's second clause holds only if the key is not in the database it protects. It is not. `ENCRYPTION_KEY` (or the versioned `ENCRYPTION_KEY_V{N}` shape) is a **Fly.io app secret** injected as an environment variable at process start, per NFR-3.24. It is never stored in Postgres, never in a config file, and never in a column. A dump of the Supabase database therefore contains ciphertext and no means to read it. The same secret value is configured in the Next.js app's environment so the two implementations interoperate.

**Residual plaintext.** The plaintext columns have not been dropped — they are on Prisma-owned tables and `docs/architecture/migrations.md` §4.2 / CG-DL-9 forbid Go migrations from touching those, including with `UPDATE`. Their *values* are removed by [`cmd/purge-plaintext-columns/`](../../cmd/purge-plaintext-columns/), an application-level command following the same pattern as the three backfills. Dropping the columns themselves is a react-frontend Prisma migration, tracked as MYR-433 follow-up.

**Verify before destroy.** The purge does not trust a NULL-check on the ciphertext sibling. For every row it decrypts the ciphertext first, and a row it cannot decrypt is left completely intact — the purge never removes the only copy of anything. Rows that are **unsealed** (no ciphertext; run the backfill first) or **undecryptable** (a key mistake) are counted, logged **without their values**, and skipped. `-dry-run` performs the identical verification and writes nothing. Idempotent.

**Decryptable ⇒ scrub, even when the values differ.** This is the one subtle rule and it is worth stating plainly, because the obvious alternative is wrong. Once this server is deployed, nothing writes plaintext while ciphertext keeps advancing on every telemetry frame, token refresh and route flush — so on any live database the two copies of an *active* vehicle, drive or account diverge within seconds. A purge that required them to be EQUAL would refuse exactly those rows, forever (the backfills only populate a NULL ciphertext, so nothing ever re-converges them), leaving live credentials and live coordinates readable on the busiest accounts and making `remaining = 0` unreachable.

A plaintext that differs from a cleanly-decrypting ciphertext is therefore **stale by construction** — an older snapshot of a field whose current value just decrypted — and stale is precisely what this tool exists to delete. Equality is recorded (`redundant` vs `stale` in the report) for operator visibility only. On a busy fleet expect `stale` to dominate; that is health, not a warning. The safety of this rule rests entirely on a wrong key failing to decrypt at all, which AES-GCM's authentication tag guarantees and `TestVerifyRejectsWrongKey` pins.

**Targets, not columns.** Each `lat`/`lng` pair is purged as a single all-or-nothing unit. If one half verified and the other did not, scrubbing the verified half alone would both leave a readable coordinate and half-destroy the pair.

Because three columns are NOT NULL on the Prisma schema, "scrubbed" is per-column: `Vehicle.latitude`/`longitude` → `0` (the zero coordinate), `Drive.routePoints` → `[]`, everything else → NULL. None of those values reveals a location.

**Self-healing writes.** Two Go write paths scrub as they go, so an account does not have to wait for the purge: `UpdateTeslaToken` NULLs the plaintext token columns on every refresh (Tesla tokens are short-lived, so this is hours), and `queryProvisionAccount` NULLs them on re-link.

**Acceptance proof.** `internal/store/myr433_operator_view_test.go` writes a user through the production write paths, runs the purge, then queries the raw tables the way an operator would — plain SELECTs, no application code — and asserts that no coordinate, route geometry, or token is legible anywhere, while the same values remain recoverable through the repositories. Both halves are asserted: an empty database would satisfy the first and must not pass.

**Decrypt failures are now observable.** The at-rest read paths are deliberately fail-soft — a vehicle whose GPS ciphertext will not decrypt reports no location rather than 500ing the snapshot; a drive whose trail will not decrypt reports an empty trail. That is right for availability and dangerous for silence: a wrong `ENCRYPTION_KEY` would look exactly like "this car has never reported a position". Every such failure now increments `telemetry_store_decrypt_failures_total{column=...}` and logs at **Error**. Alert on rate > 0. The label is a column name (P2 operational metadata) and never carries a VIN, userId, or any decrypted value.

**Gauge semantics unchanged but reinterpreted.** `*_plaintext_remaining_total` still counts rows whose plaintext is populated and whose ciphertext is NULL — i.e. *backfill* backlog. It reaching zero is the precondition for running the purge, not evidence the purge has run. The purge reports its own per-column `remaining` count, which is the number that must be zero for the bar above to hold.

**A sentence in this section is easy to misread, so read it precisely.** The paragraph above says an operator with a database connection "could read a user's home, their route, and a credential that drives their car" — past tense, describing the hole MYR-433 closed. It is **not** a claim that operator access to plaintext no longer exists. §3.3.2 states what does.

#### 3.3.2 Operator access to production data (MYR-447, 2026-08-07)

MYR-433's bar was about a **database leak**: ciphertext in the dump, key outside it. That property holds. This section answers the different question a reader of the privacy page is actually asking — *can a person at this company read my data, and is it recorded?* — and the honest answer has two halves.

**Yes, operator tooling can decrypt user data.** Not as a bug: the `ops` CLI exists to support production, and support requires reading. Three subcommands decrypt and expose P1 material — `ops auth token` (a user's Tesla access + refresh tokens, i.e. fleet control of their car), `ops fields snapshot` (a whole Vehicle row: coordinates, origin/destination, the geocoded labels sealed by this same issue, and the nav polyline), and `ops fleet-config push` (both, in service of a Tesla write). `ops geocode backfill` decrypts every drive trail that is missing an address, fleet-wide. Two of these are keyed on a bare user id or VIN with **no ownership check and no second factor**: possession of the key and the database URL is the whole authorization model.

**Who holds the key.** `ENCRYPTION_KEY` is a Fly.io app secret (NFR-3.24), so the set of people who can decrypt is exactly the set who can reach that secret:

1. **Anyone with Fly deploy or `fly ssh console` access to the telemetry app.** The seal/purge binaries are shipped *inside* the production image deliberately, so they can run against the ambient secret without exporting it.
2. **CI.** The deploy workflow holds a `FLY_API_TOKEN` and auto-deploys on every push to `main`; a Fly API token is transitively read access to app secrets.
3. **Any developer running the `ops` CLI**, which runs from a laptop with the key exported — the documented setup sources the sibling Next.js app's production-shaped env file.
4. **The Next.js app's environment**, which is configured with the same key value so the two implementations interoperate.

That is the truthful perimeter. It is small and it is human, and no technical control in this repo narrows it further — narrowing it is a Fly/IAM change, not a code change.

**What MYR-447 adds is accountability, not restriction.** Every decrypting `ops` subcommand now writes an `operator_decrypt` audit row before the plaintext is printed or transmitted: **who** (the required `OPS_OPERATOR` handle — the command refuses to run without it, there is no default and no fallback to the OS username), **what** (the subcommand, the affected record, and the names of the fields exposed — never their values, CG-DL-5), and **when**. The write is **fail-closed**: if the audit insert fails the command aborts and prints nothing, the same posture CG-DL-3 requires of deletions. So operator access is no longer deniable, and "who read this account's data" is one indexed query.

**What is still NOT covered, stated rather than omitted:**

- **`GET /api/debug/fields`** streams raw decoded telemetry — including live GPS — for **every vehicle on the fleet**, authenticated by a single shared bearer secret with no user identity, no per-vehicle scope and no audit row, and it bypasses the role mask entirely because it subscribes to the raw event bus. It is **not mounted unless `DEBUG_FIELDS_TOKEN` is set**, so it does not exist on a default production deployment — but [`../ops-cli.md`](../ops-cli.md) documents setting it on production as a supported workflow. Treat enabling it as granting fleet-wide location access to the token holder. Giving it a per-operator identity and an audit row is the obvious follow-up and is not done here.
- **The server's own reads are not audited**, by design — the telemetry pipeline decrypts location on every broadcast, and a row per read would be a volume problem rather than an audit trail (§2.3).
- **The bulk maintenance binaries** (`purge-plaintext-columns` and the backfills) decrypt every row they scan. They emit no per-row audit entry. They expose no value — the reports are counts, and values are deliberately kept out of the logs — so the accountability gap is about the *run*, not the data; a run-scoped entry would be a reasonable addition.
- **`ops auth link`** writes a freshly-sealed token rather than reading one, so it is not a decrypt path, but it is an operator action against a user's account and is unaudited.

**Downstream doc change owed.** The `/privacy` page on myrobotaxi.app currently discloses the dual-write honestly — it says some data is stored both encrypted and in the clear. Once this is deployed and the purge has run, that storage section is no longer accurate and should be simplified to the plain claim that location data and Tesla credentials are encrypted at rest. That edit lives in the react-frontend repo and is **owned separately by Thomas**, not by this change. Do not simplify the disclosure before the purge reports zero remaining in production — until then the current wording is the true one.

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

**Extended by [MYR-435](https://linear.app/myrobotaxi/issue/MYR-435) — the role decision must be EXPLICIT, not defaulted.** For `vehicle_state`, listing a field under the owner role is no longer sufficient: it must be classified into **either** `vehicleStateViewerFields` **or** `vehicleStateOwnerOnlyFields` in `internal/mask/tables.go`, and §2.4 / §5.2.1.1 must say which. This is enforced in code by `TestVehicleStateRoleListsPartitionOwnerFields`, which fails the build when a field appears in the owner list and in neither role list. The rule exists because the previous viewer arm was a subtraction (`owner minus vin`), under which a new field's viewer exposure was decided by omission — the failure mode CG-DC-5's "Why it matters" paragraph anticipated in one direction (silent UX regression) turned out to bite in the other (silent over-exposure).

**Companion check — schema `required`.** A field withheld from a role MUST be OPTIONAL in that resource's JSON Schema, or every masked document is invalid against the shape its own consumer decodes. Enforced for `vehicle_summary` by `TestViewerMaskKeepsEverySchemaRequiredField`; MYR-435 relaxed `interiorTemp` / `exteriorTemp` out of `vehicle-state.schema.json`'s `required` for exactly this reason.

**Rule does NOT apply to:** Prisma-owned columns that are never surfaced over REST or WS (e.g., `User.id`, `Account.refresh_token`). These are documented in §1 for completeness but are out of the SDK contract surface.

---

## 6. Classification summary

### By tier

| Tier | Count | Description |
|------|-------|-------------|
| P0 | 156 | Public — timestamps, opaque IDs, aggregate stats, feature flags, enums, authorization tiers and per-grant authorization flags, notification-delivery preferences, saved-place slot names, Live Activity registration state (ride/party keys, APNs build flavour, end tombstones), its leg-progress anchor (leg/unit enums, a distance magnitude, a unitless fraction, and the raw reading plus the instant it was observed) and its island auto-expand high-water mark |
| P1 | 48 | Sensitive — GPS coordinates (including saved Home/Work), location names/addresses (encrypted on `Vehicle`/`Drive` since MYR-447; log-redaction-only on the Go-owned ride/saved-place tables), OAuth tokens, PII, route data, media now-playing text, push device tokens, ActivityKit Live Activity push tokens, sharing invite codes |
| P2 | 0 | Access-logged — reserved for future use |

> **Count audit trail.** The P0 count was bumped from 83 → 85 by [MYR-11](https://linear.app/myrobotaxi/issue/MYR-11) when it added `Vehicle.chargeState` (Tesla proto field **179** `DetailedChargeState`, enum — see DV-19 for the 2026-04-23 empirical finding that switched the source from proto 2 to proto 179) and `Vehicle.timeToFull` (Tesla proto field 43, `Float` **hours (decimal)**) to the v1 charge atomic group. Both fields are P0 because they describe charge state, not identity or location. See §1.3 Vehicle table and `vehicle-state-schema.md` §2.2 for the wire contract. The `timeToFull` unit was empirically verified as hours (1.0667h capture) on 2026-04-22 — [DV-17 RESOLVED](https://linear.app/myrobotaxi/issue/MYR-25#comment-4f1dcee9-ab10-4039-acc5-9e7ef25c3762). Future count changes MUST add a one-line entry here so the total is auditable without `git blame`. P0 85 → 97 and P1 26 → 36 by MYR-173: the new Go-owned `go_ride_requests` table (§1.9) adds 12 P0 columns (ids, status enums, timestamps), 4 P1 encrypted coordinate columns (encrypt-only, §3.1), and 6 P1 log-redaction-only columns (place labels/addresses + booked-for passenger name/phone, §3.2). MYR-270 adds 1 P0 column to §1.9 (`picked_up_at`, the owner-confirmed pickup timestamp; migration 0009) — no P1 change (the pickup/dropoff coordinate classification is unchanged). MYR-279 adds 2 P0 columns to §1.13 (`software_version`, `trim`; migration 0011) — no P1 change; it also newly exposes the existing P0 `Vehicle.vin` at FULL length on the owner `/snapshot` (owner-mask only, not a new column, no tier change). **Running total: P0 97 → 100** (MYR-270 `picked_up_at` +1 = 98, MYR-279 `software_version` + `trim` +2 = 100); P1 unchanged at 36. **Correction (recorded by MYR-303):** MYR-298 added 2 P0 columns to §1.13 (`seat_vent_enabled`, `media_playback_status`; migration 0014) without an entry here, so the total was understated — **P0 100 → 102**. MYR-303 adds 3 P0 columns to §1.13 (`media_now_playing_duration_ms`, `media_now_playing_elapsed_ms`, `media_volume_max`; migration 0015) and **5 P1 log-redaction-only columns** (`media_now_playing_title`/`_artist`/`_album`/`_station`, `media_playback_source`) — the first P1 rows in that table, classified so because they are free-text user content whose accumulation reveals listening habits. MYR-308 adds 1 P0 column to §1.13 (`seat_cooling_capable`; migration 0015), an equipment/spec fact tiered like `trim`. **Running total: P0 102 → 106** (MYR-303 +3 = 105, MYR-308 +1 = 106); **P1 36 → 41** (MYR-303 +5, log-redaction-only — no new encryption scope, since the P1 encryption scope in §3 covers GPS coordinates and these are not location data). MYR-316 adds 2 P0 columns to §1.13 (`service_etc`, `service_expected_end_at`; migration 0017) — operational timing about the car, tiered like `Vehicle.status`, unencrypted and log-safe. **Running total: P0 106 → 108**; P1 unchanged at 41. MYR-320 adds 2 P0 columns to §1.13 (`trim_label`, `fsd_version`; migration 0018) — equipment/software facts about the car, tiered like their siblings `trim` and `software_version`, unencrypted and log-safe; it also newly POPULATES the existing P0 `Vehicle.color` from Tesla (`vehicle_config.exterior_color`) via the fourth §1.4 write carve-out, which is a provenance change only — not a new column and not a tier change. **Running total: P0 108 → 110**; P1 unchanged at 41. MYR-186 adds the new Go-owned `go_push_devices` table (§1.14, migration 0019): **6 P0 columns** (`id`, `user_id`, `platform`, `sandbox`, `created_at`, `last_seen_at` — opaque ids, an enum, a build-flavour bool, timestamps) and **1 P1 log-redaction-only column** (`device_token`, §3.2 — replayed verbatim to Apple on every send, so no hashed or encrypted form is usable). **Running total: P0 110 → 116; P1 41 → 42.** MYR-184 adds the new Go-owned `go_vehicle_shares` table (§1.15, migration 0020): **10 P0 columns** (`id`, `vehicle_id`, `owner_user_id`, `permission`, `status`, `created_at`, `expires_at`, `accepted_at`, `accepted_by_user_id`, `revoked_at` — opaque cuids, an authorization tier, a lifecycle enum, clock readings) and **2 P1 log-redaction-only columns**: `label` (a person's name, owner-typed) and `code` (a live BEARER CREDENTIAL — §3.2 rather than §3.1 because the redeem lookup matches the exact bytes, so no hashed or encrypted form is usable, exactly as for `go_push_devices.device_token`). **Running total: P0 116 → 126; P1 42 → 44.** **Correction recorded here:** the "By tier" table above still read P0 106 / P1 41 at the time of this entry — stale relative to the MYR-316 and MYR-320 lines already in this paragraph — and has been reconciled to the running totals as part of MYR-186. MYR-342 adds 1 P0 column to §1.13 (`ride_share_enabled`; migration 0021) — the owner's ride-sharing switch, operational availability tiered like `Vehicle.status`, unencrypted and log-safe; it is the first NOT NULL column in that table, which is a nullability decision rather than a tier one. **Running total: P0 126 → 127**; P1 unchanged at 44. MYR-349 adds the new Go-owned `go_push_prefs` table (§1.16, migration 0022): **8 P0 columns** (`user_id`, the five category booleans `ride_lifecycle` / `drive_started` / `drive_completed` / `charging_complete` / `viewer_joined`, `created_at`, `updated_at`) and **no P1 columns at all** — the whole table is a person's own delivery switches, a capability for nothing, tiered like the sibling `Settings` flags (§1.8) and `go_vehicle_control_state.ride_share_enabled` (§1.13). Worth noting because it sits in the same feature as the P1 `go_push_devices.device_token`: the token is a capability, these are preferences about it. Every column is NOT NULL DEFAULT TRUE, a nullability decision rather than a tier one — there is no honest-unknown state, since a person who has never opened Settings IS receiving notifications. **Running total: P0 127 → 135**; P1 unchanged at 44. MYR-321 adds the new Go-owned `go_saved_places` table (§1.17, migration 0023): **4 P0 columns** (`user_id`, `kind`, `created_at`, `updated_at` — an opaque cuid, a two-member slot enum, two clock readings) and **3 P1 columns**, split across both encryption regimes: **2 P1 ENCRYPTED** (`lat_enc`, `lng_enc` — §3.1, encrypt-only with no plaintext column, the third table to take that posture after `go_ride_requests`) and **1 P1 log-redaction-only** (`label`, §3.2, the same tier decision as `go_ride_requests.pickup_label`). This is the first P1 encryption-scope growth since MYR-173, and worth naming: by the letter of NFR-3.23 these coordinates are the same tier as a ride's pickup, but by consequence they are worse — a ride coordinate is where somebody went once, a saved home coordinate is where they sleep, it is durable rather than transactional, and it is re-read on every app launch. **Running total: P0 135 → 139; P1 44 → 47.** MYR-369 adds **2 P0 columns** to §1.15 (`allow_rides`, `suspended_at`; migration 0024) — an authorization capability and an authorization state change, tiered exactly like the `permission` and `status` columns they sit beside. Neither is identifying, neither is a credential, and neither is log-redacted, so the P1 handling of that table's `label` and `code` is unchanged. **Running total: P0 139 → 141**; P1 unchanged at 47. MYR-172 adds the new Go-owned `go_live_activities` table (§1.18, migration 0025): **7 P0 columns** (`id`, `ride_request_id`, `user_id`, `sandbox`, `created_at`, `updated_at`, `ended_at` — opaque cuids, a build-flavour flag, clock readings and an end tombstone) and **1 P1 log-redaction-only column** (`activity_push_token`, §3.2 — the same tier decision as `go_push_devices.device_token`, and for the same mechanical reason: it is replayed verbatim to Apple on every push, so no hashed or encrypted form is usable). Worth naming that `ride_request_id` is the **first genuine foreign key in the `go_` namespace** — CG-DL-9 bars references to the Prisma-owned schema, but `go_ride_requests` is Go-owned, and the ride's hard-delete paths make `ON DELETE CASCADE` the safer choice — which is a referential-integrity decision, not a tier one. **Running total: P0 141 → 148; P1 47 → 48.** MYR-398 adds **6 P0 columns** to §1.18 (`progress_leg`, `progress_source`, `progress_baseline`, `progress_value`, `progress_reading`, `progress_reading_at`; migration 0027) — the Live Activity's leg-progress anchor, the memory that lets a 0..1 progress fraction mean anything. All six are P0 and it is worth naming why, given the table they join holds this platform's second capability: two are closed enums (a leg, a unit), three are SCALARS with no origin and no bearing (a magnitude, a unitless fraction, and the raw reading behind them) and one is a clock reading. Neither locates anything for a reader who does not already hold the ride's P1 endpoints, and the wire field they feed (`LiveActivityContentState.progress`) is P0 for exactly the same reason, while its neighbour `destination` stays P1 because it is a PLACE. No P1 change, no new encryption scope and no new log-redaction entry — the send line carries a `has_progress` boolean rather than the value. The same issue adds **2 further P0 columns** to §1.18 in review (`progress_reading`, `progress_reading_at`; same migration 0027) — the raw observation and the instant it was first seen, which date the NAV DATA rather than the car's row and are tiered exactly like the `progress_baseline` magnitude and the `updated_at` clock reading they mirror. **Running total: P0 148 → 154**; P1 unchanged at 48. MYR-398's v3 wave adds **1 P0 column** to §1.18 (`alerted_phase`; migration 0029) — the highest Dynamic Island auto-expand phase this Activity has been alerted at, a closed 0..6 ordinal on a fixed ladder. P0 for the same reason as its `progress_value` neighbour: it records what one phone has been SHOWN, and the underlying fact (where a ride is in its own lifecycle) is already carried in plainer form by the P0 `go_ride_requests.status`. Unlike the progress columns it IS logged, as an integer beside the existing `has_progress` boolean, because "why did the island open again?" is the question this feature gets asked and a ladder position answers it without naming anything. No P1 change, no new encryption scope, no new redaction entry. **Running total: P0 154 → 155**; P1 unchanged at 48. MYR-447 adds **no columns** and changes **no tier** — P0 stays 155 and P1 stays 48 — but it moves 8 columns from the log-redaction-only sub-list to the AES-256-GCM sub-list (§3.1/§3.2): `Vehicle.locationName`, `Vehicle.locationAddress`, `Vehicle.destinationName`, `Vehicle.destinationAddress`, `Drive.startLocation`, `Drive.startAddress`, `Drive.endLocation`, `Drive.endAddress`. **Encrypted 17 → 25; log-redaction-only 31 → 23.** Recorded here because the §6 sub-list headings carry counts of their own and a reader auditing them without `git blame` would otherwise find two numbers that changed with no entry. The reason is the one MYR-433 left open: it sealed the coordinates and left the reverse-geocoded street address beside them readable, which protected the harder attack only. Deploy order is a hard dependency — the `*Enc` columns land via a `react-frontend` Prisma migration ([`docs/migrations/myr-447-prisma-label-enc.sql`](../migrations/myr-447-prisma-label-enc.sql)) that MUST run before the server ships, because CG-DL-9 bars a Go migration from naming `Vehicle` or `Drive`. MYR-447 changes **no tier and no count** for the ride passenger fields either: `go_ride_requests.passenger_name` and `passenger_phone` stay P1 and stay in the §3.2 inventory, because a deprecated column that can still hold a value is still P1 while it holds one. What changed is retention (30 days from terminal rather than the row's 365 — [`data-lifecycle.md`](data-lifecycle.md) §2.2.2), plus the removal of both fields from `events.RideAcceptedEvent`, which was an internal-bus surface with no reader and therefore never a classified column. **Running total: P0 155, P1 48 — unchanged.** MYR-451 adds **1 P0 column** to §1.15 (`updated_at`; migration 0032) — the instant an owner last modified a grant's capability flags, tiered exactly like the `created_at`, `accepted_at` and `revoked_at` clock readings beside it, and for the same reason: it dates a ROW, names no party, and locates nothing. Worth recording why a timestamp column is a classification event at all rather than a footnote: it exists because `allow_rides` shipped as a bare boolean next to a `suspended_at` that deliberately recorded WHEN, and that asymmetry meant a withdrawn ride capability left no trace of when it was withdrawn — so when an owner reported a viewer riding after the toggle went off, the row could not distinguish a correct refusal from a live authorization bypass. The column is log-safe but is **not itself logged** — it would be redundant, since the `share invite patched` line's own emission timestamp *is* the instant being stamped; the column exists so the fact survives in the row after the logs have rolled. It is written by exactly the two statements that move a capability (`queryPatchShare` and redemption's `queryAcceptSharesByID`) and by no lifecycle transition, each of which has a dedicated timestamp of its own. No P1 change, no new encryption scope, no new redaction entry. **Running total: P0 155 → 156**; P1 unchanged at 48. MYR-409 adds **1 P0 column** to §1.13 (`nav_reading_at`; migration **0034**) — the instant the car last reported navigation, tiered exactly like the `updated_at` and `service_etc` clock readings beside it: it dates a READING, names no party and locates nothing. Recorded here because the column exists to fix a classification-adjacent honesty bug rather than to add data — the Arriving alert rung was gating on `Vehicle."lastUpdated"`, a ROW stamp moved by every telemetry write, so a car streaming motion reported fresh navigation of arbitrary age. It is **not exposed on any wire surface**: no `VehicleState` property, no mask entry, no snapshot key — it feeds the Live Activity content-state projections only. No P1 change, no new encryption scope, no new redaction entry. **Running total: P0 156 → 157**; P1 unchanged at 48. MYR-507 adds **no columns** and changes **no tier** — P0 stays 157 and P1 stays 48 — but it makes two changes worth an entry here, because both would otherwise look like drift to a later auditor. First, a **provenance change** on the existing P0 `Vehicle.model` and `Vehicle.year` (§1.3): both are now VIN-derived and written by the **fifth** §1.4 carve-out (`store.VehicleRepo.FillVehicleIdentity`), fill-if-empty, exactly as MYR-320 did for `Vehicle.color` via the fourth. Not new columns, not a tier change. Second, a **wire-exposure widening** on the existing P0 `go_vehicle_control_state.trim_label` (§1.13): it is now emitted on the `GET /api/vehicles` catalog row as well as the `/snapshot`, in both role allow-lists. Also not a tier change — an equipment fact already visible to both roles one surface over, joining the identity siblings `model`/`year`/`color` that were always in both catalog allow-lists. Its sibling `fsd_version` was deliberately not brought along. **Running total: P0 157, P1 48 — unchanged.** MYR-515 likewise adds **no columns** and changes **no tier** — P0 stays 157, P1 stays 48, and the encryption scope is untouched — but it records the **most consequential wire-exposure widening in this log**, and it is entered here so it cannot later read as drift: the existing **P1** `Vehicle.latitude` / `Vehicle.longitude` (§1.3) are now emitted on the `GET /api/vehicles` catalog row as `VehicleSummary.location`, in **both** role allow-lists. That makes it the first P1 field on that resource besides `licensePlate`, and the justification is deliberately the narrow one rather than a general "riders need location": **the viewer already receives this exact pair at this exact precision on the streaming path**, because `vehicleStateViewerFields` retains the whole Speed/GPS atomic group and MYR-435's narrowing deliberately left it. Same value, different transport, same entitled party — a row exists only because a live accepted grant produced it. What genuinely changes is **aggregation** (N positions in one response versus one per socket subscription), which affects convenience rather than reach, since the grant is per-vehicle. Coarsening was considered and rejected on the ground that rounding the catalog while the socket serves full precision to the same viewer protects nothing and degrades the pickup ETA the field exists for; **if a future policy coarsens shared positions it must coarsen the stream and this row together**, or the weaker surface silently sets the real bound. Two safeguards are new rather than inherited: the `(0,0)` sentinel is collapsed to `null` on this surface (a sentinel a consumer forgets to re-interpret is its own kind of coordinate leak), and the pair is emitted as ONE nullable object so the atomic group cannot be split. **Running total: P0 157, P1 48 — unchanged.**

### P1 fields requiring AES-256-GCM encryption (25 columns)

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
11. `Vehicle.locationName` (MYR-447)
12. `Vehicle.locationAddress` (MYR-447)
13. `Vehicle.destinationName` (MYR-447)
14. `Vehicle.destinationAddress` (MYR-447)
15. `Drive.routePoints`
16. `Drive.startLocation` (MYR-447)
17. `Drive.startAddress` (MYR-447)
18. `Drive.endLocation` (MYR-447)
19. `Drive.endAddress` (MYR-447)
20. `go_ride_requests.pickup_lat_enc`
21. `go_ride_requests.pickup_lng_enc`
22. `go_ride_requests.dropoff_lat_enc`
23. `go_ride_requests.dropoff_lng_enc`
24. `go_saved_places.lat_enc` (MYR-321)
25. `go_saved_places.lng_enc` (MYR-321)

> **MYR-447 rows 11-14 and 16-19.** The reverse-geocoded rendering of coordinates this list already sealed at rows 4-9. Promoted because the split was incoherent as a control: an operator reading `locationAddress` learns in one glance what `latitude`/`longitude` only yields to someone with a map, so encrypting the pair and leaving the label was protection against the harder attack only. These eight are the first entries here with a RETIRED PLAINTEXT SIBLING that is not dropped in the same change — `Vehicle` and `Drive` are Prisma-owned, so the column drop is a `react-frontend` migration and `cmd/purge-plaintext-columns` scrubs the residue in the interim. All eight ciphertext columns are nullable, with NULL as the absent sentinel; that is what makes the geocode backfill's discovery predicate expressible against nondeterministic ciphertext (§3.1 note).

> **MYR-321 rows 24-25.** The saved Home/Work coordinate pair, and the highest-consequence entry on this list. Rows 20-23 are where somebody went once; these are where they **sleep**, they are durable rather than transactional, and they are re-read on every app launch — so a breach here leaks an address, not a trip. Encrypt-only with no plaintext column, the same posture as rows 20-23 and for the same reason (the table is new, so there is no rollout window to bridge). The repo panics on a nil `Encryptor` at construction and treats decrypt failure as a hard read error, because there is no fallback column and a zeroed coordinate would route somebody to Null Island rather than home. The pair is atomic: both halves are written in one statement, and a read that cannot decrypt both fails whole. See §1.17.

### P1 fields with log-redaction only (no app-level encryption, 23 columns)

1. `User.name`
2. `User.email`
3. `User.image`
4. `Vehicle.licensePlate`
5. `Invite.email` (RETIRED UNUSED — see §1.6; the live sharing surface is §1.15)
6. `TripStop.name`
7. `TripStop.address`
8. `go_ride_requests.pickup_label`
9. `go_ride_requests.pickup_address`
10. `go_ride_requests.dropoff_label`
11. `go_ride_requests.dropoff_address`
12. `go_ride_requests.passenger_name` (DEPRECATED MYR-447 — 30-day window, §1.9)
13. `go_ride_requests.passenger_phone` (DEPRECATED MYR-447 — 30-day window, §1.9)
14. `go_vehicle_control_state.media_now_playing_title` (MYR-303)
15. `go_vehicle_control_state.media_now_playing_artist` (MYR-303)
16. `go_vehicle_control_state.media_now_playing_album` (MYR-303)
17. `go_vehicle_control_state.media_now_playing_station` (MYR-303)
18. `go_vehicle_control_state.media_playback_source` (MYR-303)
19. `go_push_devices.device_token` (MYR-186)
20. `go_vehicle_shares.label` (MYR-184)
21. `go_vehicle_shares.code` (MYR-184)
22. `go_saved_places.label` (MYR-321)
23. `go_live_activities.activity_push_token` (MYR-172)

> **MYR-172 row 23.** The ActivityKit update token behind the rider's Live Activity — the second capability-shaped entry on this list, and handled exactly like row 19. Log-redaction-only for the same mechanical reason: the sender replays the exact bytes to Apple on every push, so there is nothing to hash and no derived form that works. It differs from row 19 in scope rather than in tier — a device token addresses an INSTALLATION and lives as long as the app is installed, whereas this addresses ONE RUNNING ACTIVITY, lives for one ride, and **rotates mid-ride** — which makes the exposure window narrower, not the handling looser. Same 8-character logged prefix, never echoed into a response or error envelope, and the §7.21 responses deliberately return only booleans. The "never in full" rule is enforced by construction on the send path too — the token is percent-escaped into the APNs request URL and transport errors are unwrapped from `*url.Error` before wrapping, so no failure mode renders the address that contains it. See §1.18.

> **MYR-321 row 22.** The display address/name for a saved place — an ordinary location-string P1, handled like `go_ride_requests.pickup_label` and `Drive.startLocation`: coarser than the coordinate pair it came from, and that pair (rows 24-25 of the list above) already carries the precision under AES-256-GCM. It sits on the platform's most sensitive coordinate row and is still not promoted, deliberately: encrypting it would protect a string a database-read attacker could reconstruct from the row's own context while the genuinely precise part is already sealed. Redaction is absolute here — unlike row 19 there is no logged prefix, and a `400` names the field without echoing the value. See §1.17.

> **MYR-184 rows 20-21.** `label` is an ordinary name-shaped P1 — a person's name the owner typed, handled like `User.name`. `code` is the unusual one: it is not data *about* a person, it is a **capability**, and presenting it grants live access to somebody else's car. It is log-redaction-only for the same mechanical reason as row 19 — the redeem lookup matches the exact bytes, so there is nothing to hash and nothing derived that works — but the redaction bar is higher: never logged **at all** (not even a prefix, unlike the device token), never echoed into an error envelope, and blanked in SQL on every read of a non-pending row. See §1.15.

> **MYR-303 rows 22-26.** Free-text media metadata, not location or identity data — which is why they are log-redaction-only rather than encrypted (the §3 encryption scope covers GPS coordinates). Redaction means never logging the value: log presence/length only. As of MYR-303 no code path logs a telemetry field's value at all (see §1.13), so these entries are a standing constraint on future logging code rather than a fix to existing code.
