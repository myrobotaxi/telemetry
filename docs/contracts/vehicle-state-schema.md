# Vehicle State Schema Contract

**Status:** Draft -- v1
**Target artifact:** JSON Schema (draft-2020-12)
**Owner:** `sdk-architect` agent
**Schema file:** [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json)
**Last updated:** 2026-04-23

## Purpose

Canonical shape of vehicle state as consumed by the SDKs and rendered by consumer UIs. Defines every field name, type, unit, nullability rule, and -- critically -- which fields belong to which **atomic group**. Both the WebSocket protocol and the REST snapshot endpoint return subsets of this schema. Both SDKs generate types from it.

## Anchored requirements

- **FR-1.1, FR-1.2** -- telemetry field set (position, speed, heading, gear, battery, charge state, range)
- **FR-2.1** -- nav field set (destinationName, ETA, polyline, origin, etc.)
- **FR-4.2** -- vehicle-scoped API signatures (all SDK calls use `vehicleId`, not VIN)
- **NFR-3.1** -- atomic groups declared here: `navigation`, `charge`, `gps`, `gear`
- **NFR-3.3, NFR-3.4** -- self-consistency rules (partial groups are invalid)
- **NFR-3.5** -- every UI-rendered field is persisted and returned in the snapshot

---

## 1. Root `VehicleState` schema

The `VehicleState` object represents the complete current state of a single vehicle as persisted in the `Vehicle` table and delivered to SDK consumers. The canonical JSON Schema is at [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json) (draft-2020-12).

### 1.1 Field reference

Every field below corresponds to a column in the `Vehicle` table or a value derived at broadcast time. Fields are grouped by category. The **Group** column indicates atomic group membership (fields without a group are delivered individually).

#### Identity fields

| Field | Type | Nullable | Unit | Classification | Group | Source |
|-------|------|----------|------|----------------|-------|--------|
| `vehicleId` | `string` | No | -- | P0 | -- | DB `Vehicle.id` |
| `name` | `string` | No | -- | P0 | -- | DB `Vehicle.name` (user-assigned; see §1.2 design note on name disambiguation) |
| `model` | `string` | No | -- | P0 | -- | DB `Vehicle.model` |
| `year` | `integer` | No | -- | P0 | -- | DB `Vehicle.year` |
| `color` | `string` | No | -- | P0 | -- | DB `Vehicle.color` |

#### GPS group

| Field | Type | Nullable | Unit | Classification | Group | Source |
|-------|------|----------|------|----------------|-------|--------|
| `latitude` | `number` | No | degrees | P1 (encrypted) | `gps` | Tesla `Location` |
| `longitude` | `number` | No | degrees | P1 (encrypted) | `gps` | Tesla `Location` |
| `heading` | `integer` | No | degrees (0-359) | P0 | `gps` | Tesla `GpsHeading` |

#### Gear group

| Field | Type | Nullable | Unit | Classification | Group | Source |
|-------|------|----------|------|----------------|-------|--------|
| `gearPosition` | `string` or `null` | Yes | -- | P0 | `gear` | Tesla `Gear` |
| `status` | `string` (enum) | No | -- | P0 | `gear` | Derived from `gearPosition`. Recomputed on every frame that carries `gearPosition` AND on speed-only frames after the broadcaster has cached a gear for the VIN — see §2.4. |

`status` enum values: `driving`, `parked`, `charging`, `offline`, `in_service`.
`gearPosition` enum values: `P`, `D`, `R`, `N`, or `null`.

#### Charge group

| Field | Type | Nullable | Unit | Classification | Group | Source |
|-------|------|----------|------|----------------|-------|--------|
| `chargeLevel` | `integer` | No | percent (0-100) | P0 | `charge` | Tesla `Soc` / `BatteryLevel` |
| `chargeState` | `string` (enum) | Yes (vehicles that have never charged) | -- | P0 | `charge` | Tesla proto field **179** (`DetailedChargeState`). Enum: `Unknown`, `Disconnected`, `NoPower`, `Starting`, `Charging`, `Complete`, `Stopped`. Live WS wiring completed by MYR-40 on 2026-04-22; **re-sourced from proto 2 to proto 179 by MYR-42 on 2026-04-23** after empirical capture showed proto 2 is accepted into `fleet_telemetry_config` but never emitted by recent Tesla firmware (≥ 2024.44.25). Proto 179 fires on the same transitions with identical enum string values. DB persistence (`chargeState String?` on the Prisma-owned `Vehicle` table + Go store SELECT/UPDATE wiring) landed in MYR-41 on 2026-04-25. See §7.1, `websocket-protocol.md` §10 DV-03 + DV-19. |
| `estimatedRange` | `integer` | No | miles | P0 | `charge` | Tesla `EstBatteryRange` |
| `timeToFull` | `number` | Yes (no active charging session) | **hours** (decimal) | P0 | `charge` | Tesla proto field **43** (`TimeToFullCharge`, `double`). Unit is hours per the `tesla-fleet-telemetry-sme` skill and the legacy Tesla REST API; empirically verified as 1.066666841506958h on 2026-04-22 (DV-17 resolved). Live WS wiring completed by MYR-40 on 2026-04-22; DB persistence (`timeToFull Float?` on the Prisma-owned `Vehicle` table + Go store SELECT/UPDATE wiring) landed in MYR-41 on 2026-04-25. See `websocket-protocol.md` §10 DV-04. |

#### Navigation group

| Field | Type | Nullable | Unit | Classification | Group | Source |
|-------|------|----------|------|----------------|-------|--------|
| `destinationName` | `string` or `null` | Yes | -- | P1 | `navigation` | Tesla `DestinationName` |
| `destinationAddress` | `string` or `null` | Yes | -- | P1 | `navigation` | Tesla / reverse-geocoded |
| `destinationLatitude` | `number` or `null` | Yes | degrees | P1 (encrypted) | `navigation` | Tesla `DestinationLocation` |
| `destinationLongitude` | `number` or `null` | Yes | degrees | P1 (encrypted) | `navigation` | Tesla `DestinationLocation` |
| `originLatitude` | `number` or `null` | Yes | degrees | P1 (encrypted) | `navigation` | Tesla `OriginLocation` |
| `originLongitude` | `number` or `null` | Yes | degrees | P1 (encrypted) | `navigation` | Tesla `OriginLocation` |
| `etaMinutes` | `integer` or `null` | Yes | minutes | P0 | `navigation` | Tesla `MinutesToArrival` |
| `tripDistanceRemaining` | `number` or `null` | Yes | miles | P0 | `navigation` | Tesla `MilesToArrival`. Unit is literally miles regardless of the vehicle's user-facing distance-unit preference; see §7.1 resolved-decisions row for the MYR-29 research citation. |
| `navRouteCoordinates` | `array` or `null` | Yes | [lng, lat] pairs | P1 (encrypted) | `navigation` | Tesla `RouteLine` (decoded) |

#### Individual fields (no atomic group)

| Field | Type | Nullable | Unit | Classification | Group | Source |
|-------|------|----------|------|----------------|-------|--------|
| `speed` | `integer` | No | mph | P0 | -- | Tesla `VehicleSpeed` |
| `odometerMiles` | `integer` | No | miles | P0 | -- | Tesla `Odometer` |
| `interiorTemp` | `integer` | No | fahrenheit | P0 | -- | Tesla `InsideTemp` |
| `exteriorTemp` | `integer` | No | fahrenheit | P0 | -- | Tesla `OutsideTemp` |
| `fsdMilesSinceReset` | `number` | No | miles | P0 | -- | Tesla `SelfDrivingMilesSinceReset`. DB column `Vehicle.fsdMilesSinceReset` (non-null, default 0) — renamed from the legacy `fsdMilesToday` by MYR-24 (cross-repo Prisma migration). |
| `locationName` | `string` | No | -- | P1 | -- | Reverse-geocoded server-side. DB NOT NULL DEFAULT `""`; an empty string means no geocode is available yet. |
| `locationAddress` | `string` | No | -- | P1 | -- | Reverse-geocoded server-side. DB NOT NULL DEFAULT `""`; an empty string means no geocode is available yet. |
| `lastUpdated` | `string` (ISO 8601) | No | -- | P0 | -- | Server timestamp |

### 1.2 Design notes

- **`vehicleId` is the SDK identifier, not VIN.** Per FR-4.2, the SDK API is vehicle-scoped using the opaque database ID. VINs are internal to the telemetry server and never exposed to SDK consumers.
- **`speed` is NOT in the GPS group.** Although the requirements doc lists speed in the GPS group (NFR-3.1), the implementation broadcasts speed independently (high-frequency, 2s interval) while GPS/heading are delivered together. Speed is decoupled from GPS because it changes more frequently than position and does not require atomic consistency with coordinates. This is an intentional divergence documented here; the requirements doc should be updated to reflect this.
- **Integer rounding.** Tesla emits most numeric fields as floats. The telemetry server rounds `speed`, `heading`, `chargeLevel`, `estimatedRange`, `interiorTemp`, `exteriorTemp`, `odometerMiles`, and `etaMinutes` to the nearest integer before delivery.
- **Coordinate order.** `navRouteCoordinates` uses `[longitude, latitude]` order (GeoJSON/Mapbox convention), NOT `[lat, lng]`.
- **`locationName` and `locationAddress` are derived fields.** They are reverse-geocoded from GPS coordinates on the server. They are NOT part of the GPS atomic group because they update asynchronously (geocoding is async) and are not sourced from Tesla telemetry.
- **`name` is sourced from the DB, not Tesla telemetry.** `VehicleState.name` comes exclusively from the DB `Vehicle.name` column (user-assigned via the Next.js settings UI). Tesla also streams a `VehicleName` proto field (decoded as internal field `vehicleName` in `internal/telemetry/fields.go`) at a 300s interval, but this value is received by the telemetry decoder and is NOT broadcast to SDK clients or used to populate the SDK `name` field. Rationale: (1) the user can rename their vehicle in the MyRoboTaxi app, so the DB is the source of truth for user-facing names; (2) Tesla's `VehicleName` may lag a user rename by up to 300s, creating stale-name confusion; (3) if Tesla-to-DB name sync is ever needed, that responsibility belongs to the Next.js app layer (which owns the `Vehicle` table via Prisma), not the telemetry server. The `Settings.teslaVehicleName` column (see `data-classification.md` §1.8) stores the Tesla-reported name separately and may differ from `Vehicle.name` if the user renames the vehicle in the MyRoboTaxi app.

---

## 2. Atomic group sub-schemas

### 2.1 Navigation group

**Requirement traceability:** FR-2.1, FR-2.2, FR-2.3, NFR-3.1, NFR-3.2, NFR-3.3, NFR-3.4

**Members:** `destinationName`, `destinationAddress`, `destinationLatitude`, `destinationLongitude`, `originLatitude`, `originLongitude`, `etaMinutes`, `tripDistanceRemaining`, `navRouteCoordinates`

**Server-side implementation:** The broadcaster routes all navigation-related Tesla fields (`DestinationName`, `DestinationLocation`, `OriginLocation`, `RouteLine`, `MinutesToArrival`, `MilesToArrival`) through the `groupAccumulator` (`internal/ws/group_accumulator.go`) registered against `groupNavigation` with a 500 ms flush window. When the first nav field arrives for a vehicle, a timer starts. All subsequent nav fields within the 500 ms window are merged into the batch. On timer expiry, the complete batch is broadcast as a single `vehicle_update` message. The 500 ms window aligns with Tesla's vehicle-side emission bucket floor; a shorter server window cannot improve atomicity because Tesla itself cannot deliver companion fields any faster (decision recorded by [MYR-14](https://linear.app/myrobotaxi/issue/MYR-14)).

**Nullability:** All navigation fields are nullable. When no navigation is active, all fields are `null`. This is the steady state for a parked vehicle or a vehicle driving without an active route.

**Nav cancellation:** When Tesla cancels navigation, it marks the nav fields as `Invalid`. The server atomically clears ALL navigation fields to `null` -- both in the WebSocket broadcast and in the database. This ensures the UI never displays a stale destination, ETA, or polyline (FR-2.3).

### 2.2 Charge group

**Requirement traceability:** FR-1.2, NFR-3.1

**Members (v1):** `chargeLevel`, `chargeState`, `estimatedRange`, `timeToFull`

**Server-side implementation:** All four fields are mapped from native Tesla Fleet Telemetry fields and delivered together in the same `vehicle_update` message when any of them change:

| Wire field | Tesla proto field | Notes |
|------------|-------------------|-------|
| `chargeLevel` | `Soc` / `BatteryLevel` | Integer-rounded server-side. |
| `chargeState` | **Field 179** `DetailedChargeState` (enum) | Enum values: `Unknown`, `Disconnected`, `NoPower`, `Starting`, `Charging`, `Complete`, `Stopped`. **Re-sourced from proto 2 to proto 179 by MYR-42 on 2026-04-23** — empirical capture showed Tesla firmware ≥ 2024.44.25 accepts proto 2 in `fleet_telemetry_config` but never emits it. Proto 179 fires on the same transitions with identical enum string values via the `Value_DetailedChargeStateValue` oneof variant (primary) or `Value_ChargingValue` (pre-2024.44.25 fallback). See §7.1 resolved decisions and `websocket-protocol.md` §10 DV-19 for the full capture + analysis. |
| `estimatedRange` | `EstBatteryRange` | Integer-rounded server-side. |
| `timeToFull` | **Field 43** `TimeToFullCharge` (`double`, **hours**, firmware 2024.26+) | Unit is **hours (decimal)** per the `tesla-fleet-telemetry-sme` skill's `data-fields-and-protobuf.md` §"TimeToFullCharge" ("Estimated hours to reach charge limit") and the legacy Tesla REST API `time_to_full_charge` field. Tesla proto type is `double`; the wire value is a JSON number (NOT rounded to integer; fractional hours supported -- e.g. `1.5` for 90 minutes). Note: Tesla also exposes `EstimatedHoursToChargeTermination` (proto field 190, `double`, firmware 2024.44.25+) as a related "simple" ETA that always targets `ChargeLimitSoc`; proto 43 is the v1 source because it is trip-aware -- see §7.1 for the delineation and decision rationale resolved by MYR-28. Empirical protobuf-level unit verification is tracked as `websocket-protocol.md` §10 DV-17. |

Tesla emits these fields on the shared 30-second charge cadence; all four are in `DefaultFieldConfig` as of [MYR-40](https://linear.app/myrobotaxi/issue/MYR-40) (2026-04-22). When any of the four change in the same Tesla 500 ms vehicle-side bucket they arrive in a single upstream `Payload` protobuf message and the broadcaster emits the four-field batch in one `vehicle_update`. If only a subset changes in the bucket, Tesla emits just that subset — atomic-group semantics are "siblings are included when they change together", not "every frame carries all four."

**No server-side accumulator** is registered for `groupCharge` (decision recorded by [MYR-14](https://linear.app/myrobotaxi/issue/MYR-14)). Tesla's 500 ms vehicle-side bucket is the atomicity guarantee; layering a server-side debounce on top would delay charge updates by the flush interval with zero benefit since the upstream cannot deliver companions faster. The `groupAccumulator` is generalized over groupID so a future MYR can opt charge in if empirical capture ever shows the bucket guarantee fails.

**Nullability:** `chargeLevel` and `estimatedRange` are non-nullable and default to `0` in the database on vehicle creation. A value of `0` for `chargeLevel` with `status: offline` means "unknown"; with `status: driving` it means "critical". `chargeState` and `timeToFull` are nullable in steady state — the Prisma-owned `Vehicle` columns are `String?` / `Float?` (post-MYR-41) and a vehicle that has never charged (or whose first charge frame has not arrived yet) surfaces `null` on both wire and REST snapshot. SDK consumers MUST tolerate `null` for these two fields and render a neutral placeholder rather than a spinner.

**Implementation status:** live WS wiring completed by [MYR-40](https://linear.app/myrobotaxi/issue/MYR-40) — `FleetFieldChargeState` (proto 2) and `FleetFieldTimeToFullCharge` (proto 43) added to `DefaultFieldConfig`; both are in `fieldMap`; `convertChargeState` routes the proto 179 `Value_DetailedChargeStateValue` enum (with proto 2 `Value_ChargingValue` fallback) via `chargingStateString`; `timeToFull` passes through `convertNumericOrString` as `float64` hours. DB persistence completed by [MYR-41](https://linear.app/myrobotaxi/issue/MYR-41) on 2026-04-25 — Prisma migration adds `chargeState String?` + `timeToFull Float?` to `Vehicle`, the Go writer pipeline applies both fields from `VehicleTelemetryEvent.Fields` on every event, and `internal/store` SELECT/UPDATE round-trips them to the cold-load snapshot path.

### 2.3 GPS group

**Requirement traceability:** FR-1.1, NFR-3.1

**Members:** `latitude`, `longitude`, `heading`

**Server-side implementation:** `Location` (a compound `{latitude, longitude}` value from Tesla) is split into separate `latitude` and `longitude` fields for the client. `GpsHeading` is delivered alongside. Tesla emits `Location` with a 10-meter minimum delta filter to suppress GPS jitter while parked.

**No server-side accumulator** is registered for `groupGPS` (decision recorded by [MYR-14](https://linear.app/myrobotaxi/issue/MYR-14)). Atomicity of `latitude`/`longitude` is upstream-guaranteed: both values arrive in the same Tesla `Location` protobuf field, so they are split server-side and never broadcast independently. `heading` flows independently — both DB columns are non-nullable with default `0`, so the §3.3 "heading accompanies coordinates" predicate is satisfied at write time without a debounce layer.

**Nullability:** Non-nullable in the database (default `0`). A position of `(0, 0)` indicates no GPS fix has been received. SDK consumers SHOULD treat `latitude == 0 && longitude == 0` as "no position available" rather than a valid location in the Gulf of Guinea.

### 2.4 Gear group

**Requirement traceability:** NFR-3.1

**Members:** `gearPosition`, `status`

**Server-side implementation:** When a `Gear` telemetry event arrives, the server maps it to `gearPosition` and derives `status` using `deriveVehicleStatus()`: gear D or R = `driving`, otherwise = `parked`. Both fields are included in the same `vehicle_update` message.

**No server-side accumulator** is registered for `groupGear` (decision recorded by [MYR-14](https://linear.app/myrobotaxi/issue/MYR-14)). `status` is derived synchronously from `gearPosition` at broadcast time, so the two fields are produced in the same code path within the same frame — there is no temporal split for an accumulator to reconcile.

**Speed-only frames recompute `status` against the cached gear ([MYR-61](https://linear.app/myrobotaxi/issue/MYR-61)).** The broadcaster keeps a per-VIN cache of the most recently seen `gearPosition`. On a frame carrying only `speed`, the cached gear is injected into the wire payload and `status` is derived from the combined fields, so the gear group remains atomic on the wire (`status` never appears without `gearPosition`). Implemented in [`internal/ws/nav_broadcast.go`](../../internal/ws/nav_broadcast.go) `ensureGearGroupAtomic`. The cache is wiped on connectivity disconnect to prevent stale gear from leaking across reconnects. Cold-start frames (no cache hit yet) leave `status` off the wire so the SDK keeps its prior value rather than receiving a partial group.

**Nullability:** `gearPosition` is nullable (`null` = vehicle asleep or gear not yet reported). `status` is non-nullable (default `offline`).

---

## 3. Atomic-group consistency predicates (NFR-3.3)

These predicates define what constitutes a valid vehicle state snapshot. The server MUST enforce these on every DB write. The SDK validates these on snapshot load and live updates.

> **Enforcement boundary.** The predicates in this section are **semantic invariants** -- they constrain _values_ (null vs non-null, cross-field equivalence, enum derivation) in ways that JSON Schema cannot express. JSON Schema keywords like `dependentRequired` only enforce the _presence_ of sibling _keys_, not the non-nullness of their values, and `dependentSchemas` cannot express "if A's value is non-null, then B's value must be non-null" across independently-typed fields. Therefore these predicates are **NOT enforced by the `vehicle-state.schema.json` file** -- they are enforced at runtime by the [`contract-tester`](../../.claude/agents/contract-tester.md) agent's FR/NFR conformance suite, and at write-time by the server's persistence layer. Any wording below that sounds prescriptive applies to the runtime validators, not the schema. If you need to confirm an invariant, check the `contract-tester` fixtures, not `vehicle-state.schema.json`.

### 3.1 Navigation group predicates

Enforced by `contract-tester` (runtime) + server persistence layer (write-time). NOT enforced by JSON Schema.

1. **Coordinate pairs are atomic.** If `destinationLatitude` is non-null, then `destinationLongitude` MUST also be non-null, and vice versa. Same rule applies to `originLatitude`/`originLongitude`. (`dependentRequired` in the schema enforces _key presence_ as a weaker best-effort hint, but the non-null invariant is a semantic check by `contract-tester`.)

2. **Active navigation completeness.** If `destinationName` is non-null, then `destinationLatitude`, `destinationLongitude`, and `navRouteCoordinates` MUST also be non-null (NFR-3.3). The reverse is also true: if `navRouteCoordinates` is non-null, then `destinationName` MUST be non-null. Semantic invariant only; not schema-enforceable.

3. **All-or-nothing clear.** When navigation is cancelled, ALL navigation fields MUST be null. A snapshot where `destinationName` is null but `navRouteCoordinates` is non-null is invalid (FR-2.3, NFR-3.4). Semantic invariant only; not schema-enforceable. The prior `x-spec-only` exemption was retired by MYR-24 (2026-04-23) — no field in the navigation group is currently exempt.

4. **ETA/distance independence during accumulation.** `etaMinutes` and `tripDistanceRemaining` MAY arrive slightly after other nav fields during the 500ms accumulation window. However, the DB snapshot (used for cold page load) MUST be fully consistent -- these fields are either all present or all null. Semantic invariant only; not schema-enforceable.

### 3.2 Charge group predicates

1. **Both fields present.** `chargeLevel` and `estimatedRange` are always present in the DB snapshot (non-nullable, default 0). There is no partial charge state.

### 3.3 GPS group predicates

1. **Coordinate pair atomic.** `latitude` and `longitude` MUST always be updated together. A state where one is non-zero and the other is zero is invalid (except for the edge case of `latitude == 0` at the equator, which is handled by the GPS delta filter).

2. **Heading accompanies coordinates.** `heading` MUST be present whenever `latitude`/`longitude` are present (both are non-nullable with defaults).

### 3.4 Gear group predicates

1. **Gear-to-status derivation.** When `gearPosition` is `D` or `R`, `status` MUST be `driving`. When `gearPosition` is `P` or `N`, `status` MUST be `parked` (unless overridden by `charging`, `offline`, or `in_service` states set by server-side logic).

2. **Status is never null.** Even when `gearPosition` is null, `status` has a valid value (typically `offline`).

---

## 4. Field-to-source-of-truth mapping

Every field has exactly one authoritative source. This mapping is critical for understanding data freshness and staleness behavior.

| Field | Source of Truth | Freshness Model |
|-------|----------------|-----------------|
| `vehicleId`, `name`, `model`, `year`, `color` | DB (Prisma-owned) | Static; changes only via user action in web app |
| `latitude`, `longitude` | Tesla `Location` telemetry | Live stream; 2s interval with 10m delta filter |
| `heading` | Tesla `GpsHeading` telemetry | Live stream; 5s interval |
| `speed` | Tesla `VehicleSpeed` telemetry | Live stream; 2s interval |
| `gearPosition` | Tesla `Gear` telemetry | Live stream; 1s interval |
| `status` | Derived from `gearPosition` at broadcast time | Derived; updates with gear |
| `chargeLevel` | Tesla `Soc` / `BatteryLevel` telemetry | Live stream; 30s interval |
| `estimatedRange` | Tesla `EstBatteryRange` telemetry | Live stream; 30s interval |
| `interiorTemp` | Tesla `InsideTemp` telemetry | Live stream; 60s interval, 120s resend |
| `exteriorTemp` | Tesla `OutsideTemp` telemetry | Live stream; 60s interval, 120s resend |
| `odometerMiles` | Tesla `Odometer` telemetry | Live stream; 60s interval |
| `fsdMilesSinceReset` | Tesla `SelfDrivingMilesSinceReset` telemetry | Live stream; 60s interval |
| `locationName`, `locationAddress` | Reverse-geocoded from GPS coordinates (server-side) | Derived; async update after GPS change |
| `destinationName` | Tesla `DestinationName` telemetry | Live stream; 1s interval, 30s resend |
| `destinationAddress` | Tesla / reverse-geocoded | Live stream or derived |
| `destinationLatitude`, `destinationLongitude` | Tesla `DestinationLocation` telemetry | Live stream; 1s interval, 30s resend |
| `originLatitude`, `originLongitude` | Tesla `OriginLocation` telemetry | Live stream; 1s interval, 30s resend |
| `etaMinutes` | Tesla `MinutesToArrival` telemetry | Live stream; 1s interval, 30s resend |
| `tripDistanceRemaining` | Tesla `MilesToArrival` telemetry | Live stream; 1s interval, 30s resend. Unit is miles regardless of vehicle locale — see §7.1 for the MYR-29 research. |
| `navRouteCoordinates` | Tesla `RouteLine` (decoded from protobuf/polyline) | Live stream; 1s interval, 30s resend |
| `lastUpdated` | Server timestamp on each telemetry write | Updated on every telemetry event |

---

## 5. JSON Schema file

The canonical JSON Schema (draft-2020-12) is committed at:

```
docs/contracts/schemas/vehicle-state.schema.json
```

This file is the single source of truth for the `VehicleState` shape. Type generators, contract tests, and SDK implementations all derive from this file.

### 5.1 Extension keywords

The schema uses the following `x-*` extension keywords for tooling and contract enforcement:

| Keyword | Purpose | Example |
|---------|---------|---------|
| `x-classification` | Data classification tier from `data-classification.md` | `"P0"`, `"P1"` |
| `x-encrypted` | Whether the field requires AES-256-GCM encryption at rest | `true` |
| `x-atomic-group` | Atomic group membership (per field) | `"navigation"`, `"charge"`, `"gps"`, `"gear"` |
| `x-unit` | Physical unit for numeric fields | `"mph"`, `"miles"`, `"degrees"`, `"percent"`, `"fahrenheit"`, `"minutes"` |
| `x-atomic-groups` | Root-level object defining all atomic groups, members, and predicates | See schema file |

### 5.2 Sub-schemas (`$defs`)

The schema deliberately contains **no `$defs`**. Atomic group membership is encoded entirely via per-field `x-atomic-group` annotations and the root-level `x-atomic-groups` object (which lists each group's fields, consistency predicates, and nullability rules). Earlier drafts defined `NavigationGroup`/`ChargeGroup`/`GpsGroup`/`GearGroup`/`Coordinate` sub-schemas under `$defs`, but none of them were ever `$ref`'d from the main schema -- they were dead code -- and atomic-group validation happens at runtime in `contract-tester` (see §3), not through schema composition. To avoid confusing type generators and contract tooling with unreachable definitions, those stubs were removed.

---

## 6. Type generation targets

### 6.1 TypeScript (via `json-schema-to-typescript`)

**Tool:** [`json-schema-to-typescript`](https://github.com/bcherny/json-schema-to-typescript)

**Generation command:**

```bash
make gen-ts-types
```

The `gen-ts-types` Makefile target invokes `npx json-schema-to-typescript` against `docs/contracts/schemas/vehicle-state.schema.json` and writes to `sdk/typescript/src/types/vehicle-state.ts`. CI runs the same target so the doc and the actual command can never drift. See the Makefile for the exact arguments.

**Expected output:** A `VehicleState` interface with all fields typed, nullable fields as `T | null`, and the `status` / `gearPosition` fields as string literal unions.

**CI enforcement:** The generated types MUST be committed and kept in sync. A CI step runs the generator and fails if the output differs from the committed file (drift detection).

### 6.2 Swift (Codable generator)

**Approach:** Custom code generation using the JSON Schema as input. The Swift SDK defines a `VehicleState` struct conforming to `Codable`, `Sendable`, and `Observable` (Swift 6).

**Target struct:**

```swift
@Observable
public struct VehicleState: Codable, Sendable {
    public let vehicleId: String
    public let name: String
    public let model: String
    public let year: Int
    public let color: String
    public let status: VehicleStatus
    public let speed: Int
    public let heading: Int
    public let latitude: Double
    public let longitude: Double
    public let locationName: String
    public let locationAddress: String
    public let gearPosition: String?
    public let chargeLevel: Int
    public let estimatedRange: Int
    public let interiorTemp: Int
    public let exteriorTemp: Int
    public let odometerMiles: Int
    public let fsdMilesSinceReset: Double
    public let destinationName: String?
    public let destinationAddress: String?
    public let destinationLatitude: Double?
    public let destinationLongitude: Double?
    public let originLatitude: Double?
    public let originLongitude: Double?
    public let etaMinutes: Int?
    public let tripDistanceRemaining: Double?
    public let navRouteCoordinates: [[Double]]?
    public let lastUpdated: Date
}
```

**Enums:**

```swift
public enum VehicleStatus: String, Codable, Sendable {
    case driving, parked, charging, offline, inService = "in_service"
}

public enum GearPosition: String, Codable, Sendable {
    case park = "P", drive = "D", reverse = "R", neutral = "N"
}
```

**CI enforcement:** A schema-comparison test loads the JSON Schema and verifies that every `required` field exists in the Swift struct, every nullable field is `Optional`, and all enum values match. This test fails if the schema and struct diverge.

---

## 7. Decisions and open questions

### 7.1 Resolved decisions

| Decision | Rationale |
|----------|-----------|
| `speed` excluded from GPS atomic group | Speed updates at 2s while GPS uses a 10m delta filter. Coupling them would either delay speed updates or flood GPS updates. Documented divergence from NFR-3.1 text. |
| `chargeState` IS in the v1 charge group (was previously deferred) | **Overturned 2026-04-13 by MYR-11.** Tesla emits `ChargeState` natively as proto field **2** (enum: `Unknown`, `Disconnected`, `NoPower`, `Starting`, `Charging`, `Complete`, `Stopped`). The earlier "deferred; `status = charging` is the coarse equivalent" rationale was based on the assumption that only `DetailedChargeState` (proto 179) was available, which is incorrect. Implementation follow-up tracked as DV-03 (RESOLVED) in `websocket-protocol.md` §10. |
| `tripDistanceRemaining` unit is literally miles; does NOT follow vehicle's `SettingDistanceUnit` preference | **Resolved 2026-04-23 by [MYR-29](https://linear.app/myrobotaxi/issue/MYR-29) Tesla SME research.** The MYR-11 post-freeze audit flagged that `MilesToArrival` (the source for `tripDistanceRemaining`) has no authoritative unit documentation in the vendored `vehicle_data.proto` (no inline comments) or in the `tesla-fleet-telemetry-sme` skill (no entry). Community lore occasionally suggested Tesla's "miles" fields might rescale to kilometres on metric-configured vehicles. **Verdict: literal miles, always.** Sources: (1) Teslemetry's telemetry-fields reference (https://teslemetry.com/docs/topics/telemetry-fields) documents `MilesToArrival` verbatim as *"the miles until arriving at the navigation destination"* — unit stated in prose, not inferred from the name; (2) the sibling `SettingDistanceUnit` (proto 232) is documented as *"the units the vehicle uses when **displaying** distance"* — it governs the vehicle's own dashboard rendering, not the wire encoding; (3) Teslamate (the metric-native community telemetry project used heavily in Europe) internally treats all Tesla distance fields as miles and converts to km at read time — no Teslamate issue across years of metric-user reports claims a "miles" field arrives as km, which is strong negative evidence. SDK consumers rendering the field in kilometres MUST convert client-side. `SettingDistanceUnit` is intentionally NOT added to `DefaultFieldConfig` — it is only useful for rendering an SDK-consumer preference hint, which is a display concern out of scope for the unit-of-the-wire question. An empirical capture from a metric-configured Tesla (picking a navigation destination with a known km-and-mile ground-truth distance) is folded into MYR-25's acceptance criteria as a belt-and-suspenders check; until that capture lands, confidence is rated high (not certain) because all three pieces of evidence are secondary, not direct Tesla-engineer statements. |
| `chargeState` sourced from `DetailedChargeState` (proto 179), NOT `ChargeState` (proto 2) | **Corrected 2026-04-23 by [MYR-42](https://linear.app/myrobotaxi/issue/MYR-42).** [MYR-40](https://linear.app/myrobotaxi/issue/MYR-40) wired `chargeState` from proto 2 based on Tesla's published proto schema. Empirical capture on 2026-04-23 showed that Tesla firmware ≥ 2024.44.25 accepts proto 2 in `fleet_telemetry_config` (API returns `synced: true` with `ChargeState` listed) but the vehicle never actually emits proto 2 — even across plug/unplug transitions. Proto 179 `DetailedChargeState` fires on the same transitions with **identical enum string values** (verified by `detailedChargeStateString` in `internal/telemetry/enum_strings.go` producing the same 7 strings as `chargingStateString`). Evidence from a 2026-04-23 03:37-03:38 capture during unplug/plug cycle on VIN ending `3795`: two frames of `proto179="Stopped"` and `proto179="Charging"` emitted, with `proto2=""—""` in both. The contract's wire-level enum string values are unchanged; only the internal source proto changed. `Field_ChargeState` (proto 2) is now intentionally held out of `fieldMap` so uncontracted proto 2 data does not leak to SDK clients. Generalizable lesson: Tesla's proto schema documenting a field is NOT a guarantee the vehicle populates it on current firmware — empirical verification is required. The `tesla-fleet-telemetry-sme` skill lists proto 2 `ChargeState` as available without flagging deprecation; this should be re-audited (tracked alongside MYR-42). See `websocket-protocol.md` §10 DV-19. |
| `timeToFull` IS in the v1 charge group (was previously deferred with an incorrect rationale) | **Overturned 2026-04-14 by MYR-11 Tesla SME audit.** Tesla emits `TimeToFullCharge` natively as proto field **43** (`double`), verified in `vehicle_data.proto:57`. **Unit is HOURS (decimal), not seconds** -- the `tesla-fleet-telemetry-sme` skill documents it as "Estimated hours to reach charge limit" and the legacy Tesla REST API uses hours. A prior MYR-11 commit labeled the unit as "seconds" without a source; this was caught by a post-freeze audit and corrected across all contract files. The earlier "not available from Tesla Fleet Telemetry" claim (from MYR-8) was separately **factually wrong** -- it was authored without checking the Tesla proto schema. Empirical unit verification via charging-vehicle protobuf capture is tracked as **DV-17** in `websocket-protocol.md` §10. Implementation follow-up tracked as DV-04 in `websocket-protocol.md` §10. |
| `timeToFull` sourced from `TimeToFullCharge` (proto 43), not `EstimatedHoursToChargeTermination` (proto 190) | **Resolved 2026-04-21 by MYR-28 Tesla SME research.** Tesla exposes two charging-time fields with similar names and identical `double` hours units: proto **43** (`TimeToFullCharge`, firmware 2024.26+) is **trip-aware** -- when charging is part of an active Trip Planner session it reports time-to-trip-ready (the trip-planner-selected departure SoC); otherwise it reports time-to-`ChargeLimitSoc`. Proto **190** (`EstimatedHoursToChargeTermination`, firmware 2024.44.25+) is the "simple" ETA -- it always reports time-to-`ChargeLimitSoc` with no trip-planner branch (per the Teslemetry telemetry-fields reference at https://teslemetry.com/docs/topics/telemetry-fields, which describes proto 43 as "The number of hours until charging is complete. If the charge session is part of a trip, this is the time until ready to continue. Otherwise, this is time until the user set limit" and proto 190 as "The number of hours until reaching the desired state of charge. Desired state of charge is defined by ChargeLimitSoc"). The MyRoboTaxi `timeToFull` wire field sources from proto **43** because trip-aware semantics match the product UX question "when will my car be done charging?" -- if the driver has a trip planned they want time-to-trip-ready; if not, both fields return the same value. Proto 43 is also the direct analogue of the legacy Tesla REST API `time_to_full_charge` field, so existing integrator intuition carries forward. MYR-11 picked proto 43 without a comparative rationale; this decision confirms that choice with a documented comparison. Empirical side-by-side capture of both fields against a charging vehicle is folded into DV-17 (MYR-25): if proto 43 and proto 190 do NOT diverge during an observed Trip Planner charge stop, the Teslemetry description of proto 43's trip-aware behavior is wrong and the decision flips to proto 190 (the more specifically-named field). Neither field is marked deprecated in any source consulted. As of MYR-40 (2026-04-22), proto 43 is live on the WS wire via [`fleet_api_fields.go`](../../internal/telemetry/fleet_api_fields.go) `DefaultFieldConfig` + [`fields.go`](../../internal/telemetry/fields.go) `fieldMap`; proto 190 remains configured for observation in `DefaultFieldConfig` but is held out of `fieldMap` so it does not leak to SDK clients until MYR-25's Trip Planner capture confirms the decision. |
| `tripStartTime` relocated from navigation group to drive group | **Clarified 2026-04-13 by MYR-11.** `tripStartTime` is derived from the drive detector's `started_at` timestamp in [`internal/drives/`](../../internal/drives/) and has no corresponding Tesla field. Forcing it into the navigation atomic group would require a cross-subsystem join that Tesla's 500 ms bucket floor cannot deliver atomically, and a vehicle can have no nav but still have an active drive. v1 therefore carries `tripStartTime` as `drive_started.payload.startedAt` on the wire (see `websocket-protocol.md` §4.2). NFR-3.1 literal amendment pending -- tracked as DV-13 in `websocket-protocol.md` §10. |
| Temperatures in Fahrenheit | Matches the current DB schema and frontend display. Conversion to user-preferred units is a UI concern, not an SDK concern. |
| `navRouteCoordinates` uses `[lng, lat]` order | GeoJSON/Mapbox standard. Tesla's raw protobuf uses `[lat, lng]`; the server converts on decode. |
| Integer rounding applied server-side | SDK consumers receive pre-rounded values. This prevents inconsistent rounding across TypeScript/Swift/etc. |
| `fsdMilesToday` renamed to `fsdMilesSinceReset` (MYR-27, 2026-04-15) | Tesla's `SelfDrivingMilesSinceReset` does NOT reset daily -- it resets on OTA updates, factory resets, etc. The wire name `fsdMilesToday` was a cosmetic label applied without checking the upstream source. Renamed to `fsdMilesSinceReset` before any SDK type-gen ships, avoiding a breaking change. If a "miles today" metric is needed, the SDK can compute it by sampling `fsdMilesSinceReset` at midnight. |
| Vehicle `name` field source disambiguation (MYR-30, 2026-04-15) | `VehicleState.name` is sourced exclusively from DB `Vehicle.name` (user-assigned via the Next.js settings UI). Tesla's streamed `VehicleName` (proto field, 300s interval) is received by the telemetry decoder but is NOT broadcast to SDK clients and does NOT overwrite the DB value. If Tesla-to-DB name sync is needed, it belongs in the Next.js app layer, not the telemetry server. See §1.2 design note for full rationale. |
| Spec-only catalog fields (`model`, `year`, `color`, `fsdMilesSinceReset`, `locationName`, `locationAddress`, `destinationAddress`) promoted to non-nullable (MYR-24, 2026-04-23) | **Resolved 2026-04-23 by [MYR-24](https://linear.app/myrobotaxi/issue/MYR-24).** The `internal/store.Vehicle` Go struct (and the `vehicle_repo.go` SELECT / scan paths) now load all seven columns, so the server can fully populate them on snapshot. Six are promoted to non-nullable to match the Prisma `Vehicle` table's `NOT NULL DEFAULT` columns: `model`, `year`, `color` (DB `NOT NULL`), `fsdMilesSinceReset` (DB `NOT NULL DEFAULT 0`, renamed from the legacy `fsdMilesToday` by the companion cross-repo Prisma migration shipped with MYR-24), and `locationName` / `locationAddress` (DB `NOT NULL DEFAULT ''` — empty string means "no geocode available yet", treated equivalently to the previous transitional `null`). `destinationAddress` remains nullable because the Prisma column is `String?`. The JSON Schema `x-spec-only: true` markers and the `x-spec-only-issue: MYR-24` tags are removed, the six promoted fields are added to the top-level `required` array, and the §3.1 nav-group "all-or-nothing clear" predicate no longer has an exemption. |

### 7.2 Open questions

| Question | Owner | Target |
|----------|-------|--------|
| ~~Schema vs `internal/store/types.go` gap (spec-only fields).~~ | RESOLVED 2026-04-23 by MYR-24: the Go `Vehicle` struct + `vehicle_repo.go` SELECT / scan path now load all seven fields. Six are promoted to non-nullable (`model`, `year`, `color`, `fsdMilesSinceReset`, `locationName`, `locationAddress`); `destinationAddress` remains nullable (DB column is `String?`). `x-spec-only` markers removed from the JSON Schema and promoted fields added to the top-level `required` array. See §7.1 resolved-decisions row. | RESOLVED |
| ~~Should `chargingState` (string enum) be added to the charge group in v1?~~ | RESOLVED 2026-04-13 by MYR-11: YES. Tesla proto field 2 (`ChargeState`) is native. See §7.1 resolved decisions and `websocket-protocol.md` §10 DV-03. | RESOLVED |
| ~~Should `tripStartTime` be derived from drive detection events and added to nav group?~~ | RESOLVED 2026-04-13 by MYR-11: `tripStartTime` is relocated from the navigation group to the drive group; it is derived from the drive detector's `started_at` and carried as `drive_started.payload.startedAt`. See §7.1 resolved decisions and `websocket-protocol.md` §10 DV-13. | RESOLVED |
| Should temperature units be configurable (C/F) at the SDK level? | sdk-architect | v2 |

---

## Change log

| Date | Change | Author |
|------|--------|--------|
| 2026-04-09 | Initial draft -- all fields, atomic groups, consistency predicates, type generation docs | sdk-architect agent |
| 2026-04-09 | PR #161 review fixes: (1) mark 7 spec-only fields nullable + add §1.1 callout; (2) add §7.2 entry for MYR-24 Go struct gap; (3) clarify §3.1 predicates are `contract-tester`-enforced, not schema-enforced; (4) remove unreachable `$defs` sub-schemas from schema and rewrite §5.2; (5) fix latitude/longitude descriptions to reference the `0,0` convention instead of "nullable"; (6) align schema `chargeLevel: 0` semantics with §2.2 (context-dependent on `status`); update Swift struct in §6.2 to reflect new optionality | sdk-architect agent |
| 2026-04-13 | **MYR-11 v1 contract freeze, cross-contract updates from WebSocket protocol decisions.** (1) Charge group §1.1 and §2.2: added `chargeState` (Tesla proto field 2, enum) and `timeToFull` (Tesla proto field 43, `double` seconds) as v1 members; both are transitional nullable until server wiring lands. (2) §7.1 resolved decisions: overturned the previous "deferred" rulings for `chargingState` and `timeToFull`; explicitly flagged the prior "not available from Tesla Fleet Telemetry" rationale for `timeToFull` as factually wrong. (3) §7.1 resolved decisions: clarified that `tripStartTime` is relocated from the navigation group to the drive group (carried as `drive_started.payload.startedAt`, not as a vehicle_update field). (4) §7.2 open questions: closed the `chargingState` and `tripStartTime` entries as RESOLVED by MYR-11. Implementation follow-ups for `chargeState`, `timeToFull`, and the NFR-3.1 amendment for `tripStartTime` are tracked in `websocket-protocol.md` §10 as DV-03 (RESOLVED), DV-04 (RESOLVED), and DV-13 (amendment pending). | sdk-architect |
| 2026-04-14 | **MYR-11 Tesla SME audit corrections.** The `tesla-telemetry` subagent performed a trust-but-verify audit of every Tesla claim in the P1 contract foundation and found three errors in the previous freeze. Corrections: (1) `timeToFull` unit is **hours** (decimal), NOT seconds. The skill and the legacy Tesla REST API both use hours; the "seconds" label was fabricated. Corrected in §1.1 charge table, §2.2 wire field table, §7.1 resolved decisions, and the change log. (2) `VehicleTelemetryEvent.Fields` is a fabricated protobuf type name; Tesla's actual top-level message is `Payload` with repeated `Datum` entries. Corrected in §2.2. (3) Flagged that `chargeState` + `timeToFull` are not yet in `DefaultFieldConfig`, so the "shared 30-second charge cadence" claim is aspirational until DV-04 ships. New divergences DV-17 (empirical unit verification) and DV-18 (`FieldChargeState` internal constant collision) added to `websocket-protocol.md` §10. | sdk-architect |
| 2026-04-15 | **MYR-27: Rename `fsdMilesToday` to `fsdMilesSinceReset`.** Tesla's `SelfDrivingMilesSinceReset` does not reset daily; shipping the cosmetic `fsdMilesToday` wire name would bake a lie into SDK types. Renamed across §1.1 field table, §4 source-of-truth mapping, §6.2 Swift struct, §7.1 resolved decisions. Matching updates in all sibling contract docs, JSON Schema, fixtures, AsyncAPI, OpenAPI, and Go code (`field_mapping.go`, `broadcaster_test.go`, `db_test.go`). | sdk-architect |
| 2026-04-21 | **MYR-28: Delineate `TimeToFullCharge` (proto 43) vs `EstimatedHoursToChargeTermination` (proto 190).** MYR-11 picked proto 43 for `timeToFull` without comparing it to proto 190. The `tesla-telemetry` subagent confirmed via Teslemetry documentation that proto 43 is trip-aware (time-to-trip-ready during Trip Planner sessions, time-to-`ChargeLimitSoc` otherwise) while proto 190 is the simple always-targets-`ChargeLimitSoc` variant. Decision: keep proto 43 because trip-awareness matches the product UX "when will my car be done charging?". Added the full delineation + decision rationale as a new §7.1 resolved-decisions row; shrank the §2.2 "tracked for research" stub to a cross-reference. No `data-classification.md`, `schemas/vehicle-state.schema.json`, fixture, or Go code changes required because proto 43 remains the source. Empirical side-by-side capture of both fields folded into DV-17 (MYR-25); if the capture shows the two fields do NOT diverge during a Trip Planner charge stop, the decision flips to proto 190. | sdk-architect |
| 2026-04-22 | **MYR-40: Wire `chargeState` (proto 2) + `timeToFull` (proto 43) into the live WS path.** Flipped `websocket-protocol.md` §10 DV-03 and DV-04 from "RESOLVED (target documented; wiring still pending)" to "RESOLVED (wire wiring live; REST snapshot DB persistence pending)". DV-17 (empirical unit verification) flipped to "RESOLVED" — 1.0667h observed on 2026-04-22, see the [MYR-25 comment](https://linear.app/myrobotaxi/issue/MYR-25#comment-4f1dcee9-ab10-4039-acc5-9e7ef25c3762). Updated §2.2 prose: "Tesla emits these fields once the DV-03 / DV-04 wiring lands" → "Tesla emits these fields; all four are in `DefaultFieldConfig` as of MYR-40." Updated the Nullability paragraph to distinguish between live-WS non-nullable (post-MYR-40) and REST-`/snapshot` transitional-nullable (pre-DB-persistence). Updated the Implementation-status paragraph from "not yet wired in the Go server" to "wire-side complete; DB-persistence follow-up tracked separately" because the Prisma-owned `Vehicle` table requires a cross-repo PR in `../my-robo-taxi`. No `§1.1` or `data-classification.md` changes — the classification label (P0) and wire type (`string`, `number`) were already frozen. | go-engineer + sdk-architect |
| 2026-04-23 | **MYR-29: Confirm `tripDistanceRemaining` unit is literally miles.** Research via the `tesla-telemetry` subagent established that Tesla's `MilesToArrival` (the source for `tripDistanceRemaining`) always reports miles regardless of the vehicle's `SettingDistanceUnit` preference. Evidence: Teslemetry's telemetry-fields reference describes the field verbatim as "the miles until arriving at the navigation destination" (unit declared in prose, not inferred from the name); `SettingDistanceUnit` governs only the vehicle's on-dashboard display, not the wire encoding; Teslamate (metric-native community project) converts client-side and has no issue history claiming Tesla "miles" fields arrive as km on European vehicles. No contract correction needed — the existing `x-unit: miles` tag is correct. Added a new §7.1 resolved-decisions row with full citations; annotated §1.1 and §4 navigation source-mapping rows with forward-links. `SettingDistanceUnit` intentionally NOT added to `DefaultFieldConfig` — it is a display-preference hint, not a telemetry-rescaling directive. Empirical verification from a metric-configured Tesla is folded into MYR-25 as a belt-and-suspenders check. Zero code changes; zero fixture changes; zero schema changes. | sdk-architect |
| 2026-04-23 | **MYR-24: Extend `internal/store.Vehicle` to load seven spec-only catalog fields (`model`, `year`, `color`, `fsdMilesSinceReset`, `locationName`, `locationAddress`, `destinationAddress`).** Go struct + `vehicle_repo.go` SELECT / scan path updated to load all seven columns from the Prisma-owned `Vehicle` table. Six promoted to non-nullable on the wire (DB is `NOT NULL DEFAULT`): `model`, `year`, `color`, `fsdMilesSinceReset`, `locationName`, `locationAddress`. `destinationAddress` remains nullable (DB `String?`). Updated §1.1 identity / navigation / individual-field tables; removed the SPEC-ONLY callout; dropped `x-spec-only: true` + `x-spec-only-issue: MYR-24` tags from `schemas/vehicle-state.schema.json` and added the six promoted fields to the top-level `required` array; removed the §3.1 nav-group spec-only exemption; updated §6.2 Swift struct to drop the six `Optional` wrappers; updated `fixtures/rest/snapshot.json` and `fixtures/edge-cases/snapshot.charging.json` to carry real values for the six non-nullable fields; retired `fixtures/edge-cases/snapshot.spec_only_nulls.json` (purpose obsoleted — the remaining transitional-nullable charge fields are already covered by the parked-vehicle fixture). Added §7.1 resolved-decisions row; closed the §7.2 open question. Go code: `Vehicle.FsdMilesSinceReset float64` + `"fsdMilesSinceReset"` in `vehicleSelectColumns`; the companion cross-repo Prisma migration in `my-robo-taxi` renames `Vehicle.fsdMilesToday` → `Vehicle.fsdMilesSinceReset` (in-place `ALTER TABLE … RENAME COLUMN`, default preserved) so DB, wire, and code all share the MYR-27 name. Data classification unchanged (all six promoted fields were already labeled P0 / P1 pre-MYR-24). Deploy order: the Prisma migration MUST run against production before this telemetry server deploys. | go-engineer + sdk-architect |
| 2026-04-25 | **MYR-41: Persist `chargeState` + `timeToFull` to the Prisma-owned `Vehicle` table.** Closes the REST `/snapshot` DB-persistence gap left open by MYR-40. Cross-repo Prisma migration in `../my-robo-taxi` adds `chargeState String?` + `timeToFull Float?` columns. Go side: extended `store.Vehicle` and `store.VehicleUpdate` with `ChargeState *string` + `TimeToFull *float64`; added the columns to `vehicleSelectColumns`, `updateColumns`, and both scan paths in `vehicle_repo.go`; wired `FieldChargeState` + `FieldTimeToFull` appliers in `internal/store/field_mapper.go` so the writer pipeline persists every event. Added writer-pipeline + DB round-trip tests asserting `mapTelemetryToUpdate({"chargeState":"Charging","timeToFull":1.5})` → DB → `GetByVIN` returns both values exactly. Extended `cmd/ops fields snapshot` so the de-facto cold-load CLI surfaces both fields (emitted as JSON `null` when the column is NULL — no `omitempty` — for a uniform shape). Flipped `websocket-protocol.md` §10 DV-03 + DV-04 from "RESOLVED (wire wiring live; REST snapshot DB persistence pending)" to plain "RESOLVED". Dropped the "transitional-nullable on REST `/snapshot`" qualifier from §1.1 charge table, §2.2 nullability paragraph, and `schemas/vehicle-state.schema.json`. Both fields remain nullable in the canonical sense (steady-state null is possible — vehicle has never charged) but the contract no longer carries a transitional caveat. Deploy order: the Prisma migration MUST run against production before this telemetry server deploys. | go-engineer + sdk-architect |
| 2026-04-29 | **MYR-14: Declare 4 atomic field groups in code + generalize the navigation accumulator.** Adds `internal/ws/atomic_groups.go` as the single source of truth for atomic-group membership (`navigation`, `charge`, `gps`, `gear`) per §1.1. `groupOf()` becomes the partition predicate used by the broadcaster; `isNavField()` now derives from this map (no duplicate set). Renames `navAccumulator` → `groupAccumulator` and keys pending state by `(atomicGroupID, VIN)` so multiple groups can accumulate independently — but in v1 only `groupNavigation` registers, because charge atomicity is upstream-guaranteed by Tesla's 500 ms vehicle-side bucket and GPS/gear atomicity is guaranteed by co-emission and synchronous derivation respectively. Updated §2.1 / §2.2 / §2.3 / §2.4 to record per-group accumulator decisions. Captured a known gap in §2.4: `status` is not recomputed on speed-only frames; tracked as a follow-on Linear issue. Reconciled the issue's "200 ms" wording to the schema's 500 ms — Tesla's bucket floor is 500 ms, so a shorter server window cannot improve atomicity. No wire-level behavior change; partition is a pure refactor. | go-engineer + sdk-architect |
| 2026-04-23 | **MYR-42: Re-source `chargeState` from proto 179 `DetailedChargeState` (proto 2 deprecated in recent firmware).** Empirical capture on 2026-04-23 showed Tesla firmware ≥ 2024.44.25 accepts proto 2 in `fleet_telemetry_config` but never emits it. Proto 179 fires on the same transitions with identical enum strings. Updated §1.1 + §2.2 "Source" cells to reference proto 179. Added a new §7.1 resolved-decisions row recording the empirical finding. Added **DV-19** in `websocket-protocol.md` §10 (new resolved divergence). Updated `schemas/vehicle-state.schema.json` `chargeState.x-tesla-proto-field` from 2 to 179 + description. Updated `data-classification.md` §1.3 to reference proto 179. No wire-level behavior change — enum strings are identical; only the internal source proto changed. Go code: `Field_ChargeState` removed from `fieldMap` and `DefaultFieldConfig`; `Field_DetailedChargeState` now maps to `FieldChargeState` internal name; `FieldDetailedChargeState` constant deleted; `convertChargeState` consolidated to handle both `Value_DetailedChargeStateValue` primary and `Value_ChargingValue` legacy fallback. Generalizable lesson captured in `project_tesla_proto2_deprecated.md` user memory: always empirically verify a Tesla field fires before trusting the proto schema. | go-engineer + sdk-architect |
| 2026-05-03 | **MYR-61: Recompute `status` on speed-only telemetry frames against a per-VIN cached `gearPosition`.** Closes the §2.4 known gap left open by MYR-14. The broadcaster now keeps a `sync.Map` of VIN → most-recently-seen `gearPosition`. On a frame carrying only `speed`, `ensureGearGroupAtomic` injects the cached gear into the wire payload and derives `status` from the combined fields, so the gear group's "no partial group on the wire" invariant (NFR-3.1, NFR-3.4) is preserved. The cache is wiped on connectivity disconnect to prevent stale gear from leaking across reconnects. Cold-start frames (no cache hit yet) leave `status` off the wire so the SDK keeps its prior value rather than receiving a partial group. UX impact (per ux-audit on PR #198): the partner Next.js app's `SharedViewerScreen` no longer renders parked-mode UI when a shared vehicle starts moving but the next frame is speed-only. Removed the §2.4 known-gap callout and the §1.1 status-row cross-link to it. No wire format change beyond the previously-missing `gearPosition` + `status` pair on speed-only frames. | go-engineer + sdk-architect |
