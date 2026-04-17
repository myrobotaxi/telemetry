# Vehicle State Schema Contract

**Status:** Draft -- v1
**Target artifact:** JSON Schema (draft-2020-12)
**Owner:** `sdk-architect` agent
**Schema file:** [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json)
**Last updated:** 2026-04-09

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

> **SPEC-ONLY CALLOUT (MYR-24):** Seven fields in this schema are not yet loaded by the Go `Vehicle` struct in [`internal/store/types.go`](../../internal/store/types.go) and therefore cannot be populated by the server today: `model`, `year`, `color`, `fsdMilesSinceReset`, `locationName`, `locationAddress`, and `destinationAddress`. These fields are marked **nullable** (`Spec-only`) in the table below and in the JSON Schema until the follow-up issue **[MYR-24](https://linear.app/myrobotaxi/issue/MYR-24)** extends the `Vehicle` struct (and the underlying persistence path) to load them. Once MYR-24 lands, they will be promoted to non-nullable and this callout removed. SDK consumers MUST tolerate `null` for every spec-only field until then. See §7.2 for the full open question entry.

#### Identity fields

| Field | Type | Nullable | Unit | Classification | Group | Source |
|-------|------|----------|------|----------------|-------|--------|
| `vehicleId` | `string` | No | -- | P0 | -- | DB `Vehicle.id` |
| `name` | `string` | No | -- | P0 | -- | DB `Vehicle.name` |
| `model` | `string` or `null` | Yes (Spec-only, MYR-24) | -- | P0 | -- | DB `Vehicle.model` |
| `year` | `integer` or `null` | Yes (Spec-only, MYR-24) | -- | P0 | -- | DB `Vehicle.year` |
| `color` | `string` or `null` | Yes (Spec-only, MYR-24) | -- | P0 | -- | DB `Vehicle.color` |

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
| `status` | `string` (enum) | No | -- | P0 | `gear` | Derived from `gearPosition` |

`status` enum values: `driving`, `parked`, `charging`, `offline`, `in_service`.
`gearPosition` enum values: `P`, `D`, `R`, `N`, or `null`.

#### Charge group

| Field | Type | Nullable | Unit | Classification | Group | Source |
|-------|------|----------|------|----------------|-------|--------|
| `chargeLevel` | `integer` | No | percent (0-100) | P0 | `charge` | Tesla `Soc` / `BatteryLevel` |
| `chargeState` | `string` (enum) | Yes (until wired) | -- | P0 | `charge` | Tesla proto field **2** (`ChargeState`). Enum: `Unknown`, `Disconnected`, `NoPower`, `Starting`, `Charging`, `Complete`, `Stopped`. v1 field; implementation follow-up pending -- see `websocket-protocol.md` §10 DV-03 (RESOLVED). |
| `estimatedRange` | `integer` | No | miles | P0 | `charge` | Tesla `EstBatteryRange` |
| `timeToFull` | `number` | Yes (until wired) | **hours** (decimal) | P0 | `charge` | Tesla proto field **43** (`TimeToFullCharge`, `double`). Unit is hours per the `tesla-fleet-telemetry-sme` skill and the legacy Tesla REST API; empirical verification tracked as `websocket-protocol.md` §10 DV-17. v1 field; implementation follow-up pending -- see `websocket-protocol.md` §10 DV-04. |

#### Navigation group

| Field | Type | Nullable | Unit | Classification | Group | Source |
|-------|------|----------|------|----------------|-------|--------|
| `destinationName` | `string` or `null` | Yes | -- | P1 | `navigation` | Tesla `DestinationName` |
| `destinationAddress` | `string` or `null` | Yes (Spec-only, MYR-24) | -- | P1 | `navigation` | Tesla / reverse-geocoded |
| `destinationLatitude` | `number` or `null` | Yes | degrees | P1 (encrypted) | `navigation` | Tesla `DestinationLocation` |
| `destinationLongitude` | `number` or `null` | Yes | degrees | P1 (encrypted) | `navigation` | Tesla `DestinationLocation` |
| `originLatitude` | `number` or `null` | Yes | degrees | P1 (encrypted) | `navigation` | Tesla `OriginLocation` |
| `originLongitude` | `number` or `null` | Yes | degrees | P1 (encrypted) | `navigation` | Tesla `OriginLocation` |
| `etaMinutes` | `integer` or `null` | Yes | minutes | P0 | `navigation` | Tesla `MinutesToArrival` |
| `tripDistanceRemaining` | `number` or `null` | Yes | miles | P0 | `navigation` | Tesla `MilesToArrival` |
| `navRouteCoordinates` | `array` or `null` | Yes | [lng, lat] pairs | P1 (encrypted) | `navigation` | Tesla `RouteLine` (decoded) |

#### Individual fields (no atomic group)

| Field | Type | Nullable | Unit | Classification | Group | Source |
|-------|------|----------|------|----------------|-------|--------|
| `speed` | `integer` | No | mph | P0 | -- | Tesla `VehicleSpeed` |
| `odometerMiles` | `integer` | No | miles | P0 | -- | Tesla `Odometer` |
| `interiorTemp` | `integer` | No | fahrenheit | P0 | -- | Tesla `InsideTemp` |
| `exteriorTemp` | `integer` | No | fahrenheit | P0 | -- | Tesla `OutsideTemp` |
| `fsdMilesSinceReset` | `number` or `null` | Yes (Spec-only, MYR-24) | miles | P0 | -- | Tesla `SelfDrivingMilesSinceReset` |
| `locationName` | `string` or `null` | Yes (Spec-only, MYR-24) | -- | P1 | -- | Reverse-geocoded server-side |
| `locationAddress` | `string` or `null` | Yes (Spec-only, MYR-24) | -- | P1 | -- | Reverse-geocoded server-side |
| `lastUpdated` | `string` (ISO 8601) | No | -- | P0 | -- | Server timestamp |

### 1.2 Design notes

- **`vehicleId` is the SDK identifier, not VIN.** Per FR-4.2, the SDK API is vehicle-scoped using the opaque database ID. VINs are internal to the telemetry server and never exposed to SDK consumers.
- **`speed` is NOT in the GPS group.** Although the requirements doc lists speed in the GPS group (NFR-3.1), the implementation broadcasts speed independently (high-frequency, 2s interval) while GPS/heading are delivered together. Speed is decoupled from GPS because it changes more frequently than position and does not require atomic consistency with coordinates. This is an intentional divergence documented here; the requirements doc should be updated to reflect this.
- **Integer rounding.** Tesla emits most numeric fields as floats. The telemetry server rounds `speed`, `heading`, `chargeLevel`, `estimatedRange`, `interiorTemp`, `exteriorTemp`, `odometerMiles`, and `etaMinutes` to the nearest integer before delivery.
- **Coordinate order.** `navRouteCoordinates` uses `[longitude, latitude]` order (GeoJSON/Mapbox convention), NOT `[lat, lng]`.
- **`locationName` and `locationAddress` are derived fields.** They are reverse-geocoded from GPS coordinates on the server. They are NOT part of the GPS atomic group because they update asynchronously (geocoding is async) and are not sourced from Tesla telemetry.

---

## 2. Atomic group sub-schemas

### 2.1 Navigation group

**Requirement traceability:** FR-2.1, FR-2.2, FR-2.3, NFR-3.1, NFR-3.2, NFR-3.3, NFR-3.4

**Members:** `destinationName`, `destinationAddress`, `destinationLatitude`, `destinationLongitude`, `originLatitude`, `originLongitude`, `etaMinutes`, `tripDistanceRemaining`, `navRouteCoordinates`

**Server-side implementation:** The broadcaster routes all navigation-related Tesla fields (`DestinationName`, `DestinationLocation`, `OriginLocation`, `RouteLine`, `MinutesToArrival`, `MilesToArrival`) through a `navAccumulator` with a 500ms flush window. When the first nav field arrives for a vehicle, a timer starts. All subsequent nav fields within the 500ms window are merged into the batch. On timer expiry, the complete batch is broadcast as a single `vehicle_update` message.

**Nullability:** All navigation fields are nullable. When no navigation is active, all fields are `null`. This is the steady state for a parked vehicle or a vehicle driving without an active route.

**Nav cancellation:** When Tesla cancels navigation, it marks the nav fields as `Invalid`. The server atomically clears ALL navigation fields to `null` -- both in the WebSocket broadcast and in the database. This ensures the UI never displays a stale destination, ETA, or polyline (FR-2.3).

### 2.2 Charge group

**Requirement traceability:** FR-1.2, NFR-3.1

**Members (v1):** `chargeLevel`, `chargeState`, `estimatedRange`, `timeToFull`

**Server-side implementation:** All four fields are mapped from native Tesla Fleet Telemetry fields and delivered together in the same `vehicle_update` message when any of them change:

| Wire field | Tesla proto field | Notes |
|------------|-------------------|-------|
| `chargeLevel` | `Soc` / `BatteryLevel` | Integer-rounded server-side. |
| `chargeState` | **Field 2** `ChargeState` (enum) | Native Tesla emission. Enum values: `Unknown`, `Disconnected`, `NoPower`, `Starting`, `Charging`, `Complete`, `Stopped`. `DetailedChargeState` (proto field 179) is the finer-grained companion already configured at 30 s interval in [`internal/telemetry/fleet_api_fields.go`](../../internal/telemetry/fleet_api_fields.go) line 129 and MAY be layered in later via the ungrouped-field path. |
| `estimatedRange` | `EstBatteryRange` | Integer-rounded server-side. |
| `timeToFull` | **Field 43** `TimeToFullCharge` (`double`, **hours**) | Unit is **hours (decimal)** per the `tesla-fleet-telemetry-sme` skill's `data-fields-and-protobuf.md` §"TimeToFullCharge" ("Estimated hours to reach charge limit") and the legacy Tesla REST API `time_to_full_charge` field. Tesla proto type is `double`; the wire value is a JSON number (NOT rounded to integer; fractional hours supported -- e.g. `1.5` for 90 minutes). Note: Tesla also exposes `EstimatedHoursToChargeTermination` (proto field 190); the delineation between the two fields is tracked for research before the DV-04 implementation PR. Empirical protobuf-level unit verification is tracked as `websocket-protocol.md` §10 DV-17. |

Tesla emits these fields on the shared 30-second charge cadence once the DV-03 / DV-04 wiring lands. When any of the four fields change in the same Tesla 500 ms vehicle-side bucket, they will arrive in a single upstream `Payload` protobuf message and the broadcaster will emit them as a four-field batch. Today (before DV-03 / DV-04 implementation), only `chargeLevel` and `estimatedRange` are configured in `DefaultFieldConfig`, so the charge group is a two-field batch on the wire; the `null` placeholder rule below applies to the missing two fields.

**Nullability:** `chargeLevel` and `estimatedRange` are non-nullable and default to `0` in the database on vehicle creation. A value of `0` for `chargeLevel` with `status: offline` means "unknown"; with `status: driving` it means "critical". `chargeState` and `timeToFull` are **transitional nullable** until the implementation follow-up (see `websocket-protocol.md` §10 DV-03 / DV-04): SDK consumers MUST tolerate `null` for these two fields on any frame delivered before the server begins emitting them. Once the implementation ships, the charge atomic group will arrive as a four-field batch.

**Implementation follow-up:** `chargeState` and `timeToFull` are part of the v1 wire contract but are not yet wired in the Go server. The follow-up issue must add `FleetFieldChargeState` (proto 2) and `FleetFieldTimeToFullCharge` (proto 43) to [`internal/telemetry/fleet_api_fields.go`](../../internal/telemetry/fleet_api_fields.go), extend the decoder mapping, add DB columns (P0, non-encrypted), and wire through [`internal/ws/field_mapping.go`](../../internal/ws/field_mapping.go) under their canonical wire names.

### 2.3 GPS group

**Requirement traceability:** FR-1.1, NFR-3.1

**Members:** `latitude`, `longitude`, `heading`

**Server-side implementation:** `Location` (a compound `{latitude, longitude}` value from Tesla) is split into separate `latitude` and `longitude` fields for the client. `GpsHeading` is delivered alongside. Tesla emits `Location` with a 10-meter minimum delta filter to suppress GPS jitter while parked.

**Nullability:** Non-nullable in the database (default `0`). A position of `(0, 0)` indicates no GPS fix has been received. SDK consumers SHOULD treat `latitude == 0 && longitude == 0` as "no position available" rather than a valid location in the Gulf of Guinea.

### 2.4 Gear group

**Requirement traceability:** NFR-3.1

**Members:** `gearPosition`, `status`

**Server-side implementation:** When a `Gear` telemetry event arrives, the server maps it to `gearPosition` and derives `status` using `deriveVehicleStatus()`: gear D or R = `driving`, otherwise = `parked`. Both fields are included in the same `vehicle_update` message.

**Nullability:** `gearPosition` is nullable (`null` = vehicle asleep or gear not yet reported). `status` is non-nullable (default `offline`).

---

## 3. Atomic-group consistency predicates (NFR-3.3)

These predicates define what constitutes a valid vehicle state snapshot. The server MUST enforce these on every DB write. The SDK validates these on snapshot load and live updates.

> **Enforcement boundary.** The predicates in this section are **semantic invariants** -- they constrain _values_ (null vs non-null, cross-field equivalence, enum derivation) in ways that JSON Schema cannot express. JSON Schema keywords like `dependentRequired` only enforce the _presence_ of sibling _keys_, not the non-nullness of their values, and `dependentSchemas` cannot express "if A's value is non-null, then B's value must be non-null" across independently-typed fields. Therefore these predicates are **NOT enforced by the `vehicle-state.schema.json` file** -- they are enforced at runtime by the [`contract-tester`](../../.claude/agents/contract-tester.md) agent's FR/NFR conformance suite, and at write-time by the server's persistence layer. Any wording below that sounds prescriptive applies to the runtime validators, not the schema. If you need to confirm an invariant, check the `contract-tester` fixtures, not `vehicle-state.schema.json`.

### 3.1 Navigation group predicates

Enforced by `contract-tester` (runtime) + server persistence layer (write-time). NOT enforced by JSON Schema.

1. **Coordinate pairs are atomic.** If `destinationLatitude` is non-null, then `destinationLongitude` MUST also be non-null, and vice versa. Same rule applies to `originLatitude`/`originLongitude`. (`dependentRequired` in the schema enforces _key presence_ as a weaker best-effort hint, but the non-null invariant is a semantic check by `contract-tester`.)

2. **Active navigation completeness.** If `destinationName` is non-null, then `destinationLatitude`, `destinationLongitude`, and `navRouteCoordinates` MUST also be non-null (NFR-3.3). The reverse is also true: if `navRouteCoordinates` is non-null, then `destinationName` MUST be non-null. Semantic invariant only; not schema-enforceable.

3. **All-or-nothing clear.** When navigation is cancelled, ALL navigation fields MUST be null. A snapshot where `destinationName` is null but `navRouteCoordinates` is non-null is invalid (FR-2.3, NFR-3.4). Semantic invariant only; not schema-enforceable. **Spec-only exemption:** Fields marked `x-spec-only: true` (currently `destinationAddress`, until MYR-24 lands) are exempt from this invariant — they will always be null today regardless of nav state. `contract-tester` MUST skip spec-only fields when evaluating this predicate.

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
| `tripDistanceRemaining` | Tesla `MilesToArrival` telemetry | Live stream; 1s interval, 30s resend |
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
    // Spec-only until MYR-24 -- Optional until the Go Vehicle struct loads these.
    public let model: String?
    public let year: Int?
    public let color: String?
    public let status: VehicleStatus
    public let speed: Int
    public let heading: Int
    public let latitude: Double
    public let longitude: Double
    // Spec-only until MYR-24.
    public let locationName: String?
    public let locationAddress: String?
    public let gearPosition: String?
    public let chargeLevel: Int
    public let estimatedRange: Int
    public let interiorTemp: Int
    public let exteriorTemp: Int
    public let odometerMiles: Int
    // Spec-only until MYR-24.
    public let fsdMilesSinceReset: Double?
    public let destinationName: String?
    // Spec-only until MYR-24.
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
| `timeToFull` IS in the v1 charge group (was previously deferred with an incorrect rationale) | **Overturned 2026-04-14 by MYR-11 Tesla SME audit.** Tesla emits `TimeToFullCharge` natively as proto field **43** (`double`), verified in `vehicle_data.proto:57`. **Unit is HOURS (decimal), not seconds** -- the `tesla-fleet-telemetry-sme` skill documents it as "Estimated hours to reach charge limit" and the legacy Tesla REST API uses hours. A prior MYR-11 commit labeled the unit as "seconds" without a source; this was caught by a post-freeze audit and corrected across all contract files. The earlier "not available from Tesla Fleet Telemetry" claim (from MYR-8) was separately **factually wrong** -- it was authored without checking the Tesla proto schema. Empirical unit verification via charging-vehicle protobuf capture is tracked as **DV-17** in `websocket-protocol.md` §10. Implementation follow-up tracked as DV-04 in `websocket-protocol.md` §10. |
| `tripStartTime` relocated from navigation group to drive group | **Clarified 2026-04-13 by MYR-11.** `tripStartTime` is derived from the drive detector's `started_at` timestamp in [`internal/drives/`](../../internal/drives/) and has no corresponding Tesla field. Forcing it into the navigation atomic group would require a cross-subsystem join that Tesla's 500 ms bucket floor cannot deliver atomically, and a vehicle can have no nav but still have an active drive. v1 therefore carries `tripStartTime` as `drive_started.payload.startedAt` on the wire (see `websocket-protocol.md` §4.2). NFR-3.1 literal amendment pending -- tracked as DV-13 in `websocket-protocol.md` §10. |
| Temperatures in Fahrenheit | Matches the current DB schema and frontend display. Conversion to user-preferred units is a UI concern, not an SDK concern. |
| `navRouteCoordinates` uses `[lng, lat]` order | GeoJSON/Mapbox standard. Tesla's raw protobuf uses `[lat, lng]`; the server converts on decode. |
| Integer rounding applied server-side | SDK consumers receive pre-rounded values. This prevents inconsistent rounding across TypeScript/Swift/etc. |
| `fsdMilesToday` renamed to `fsdMilesSinceReset` (MYR-27, 2026-04-15) | Tesla's `SelfDrivingMilesSinceReset` does NOT reset daily -- it resets on OTA updates, factory resets, etc. The wire name `fsdMilesToday` was a cosmetic label applied without checking the upstream source. Renamed to `fsdMilesSinceReset` before any SDK type-gen ships, avoiding a breaking change. If a "miles today" metric is needed, the SDK can compute it by sampling `fsdMilesSinceReset` at midnight. |

### 7.2 Open questions

| Question | Owner | Target |
|----------|-------|--------|
| **Schema vs `internal/store/types.go` gap (spec-only fields).** The canonical v1 schema defines seven fields that the current Go `Vehicle` struct does not load: `model`, `year`, `color`, `fsdMilesSinceReset`, `locationName`, `locationAddress`, and `destinationAddress`. Until the Go struct is extended (and the SELECT / scan path in `internal/store` is updated), the server physically cannot populate these fields, so they are marked **nullable and `x-spec-only: true`** in both the MD (§1.1) and JSON Schema. SDK consumers MUST tolerate `null` for every spec-only field until MYR-24 lands. Once MYR-24 ships, these fields will be promoted back to non-nullable, the `x-spec-only` markers and the §1.1 callout removed, and this row closed. The gap is explicitly tracked in **[MYR-24](https://linear.app/myrobotaxi/issue/MYR-24)** ("Extend `internal/store.Vehicle` to load `model`/`year`/`color`/`fsdMilesSinceReset`/`locationName`/`locationAddress`/`destinationAddress`"). | sdk-architect + go-engineer | **MYR-24** |
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
