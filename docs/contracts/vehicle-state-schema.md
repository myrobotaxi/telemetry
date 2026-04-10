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

> **SPEC-ONLY CALLOUT (MYR-24):** Seven fields in this schema are not yet loaded by the Go `Vehicle` struct in [`internal/store/types.go`](../../internal/store/types.go) and therefore cannot be populated by the server today: `model`, `year`, `color`, `fsdMilesToday`, `locationName`, `locationAddress`, and `destinationAddress`. These fields are marked **nullable** (`Spec-only`) in the table below and in the JSON Schema until the follow-up issue **[MYR-24](https://linear.app/myrobotaxi/issue/MYR-24)** extends the `Vehicle` struct (and the underlying persistence path) to load them. Once MYR-24 lands, they will be promoted to non-nullable and this callout removed. SDK consumers MUST tolerate `null` for every spec-only field until then. See §7.2 for the full open question entry.

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
| `estimatedRange` | `integer` | No | miles | P0 | `charge` | Tesla `EstBatteryRange` |

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
| `fsdMilesToday` | `number` or `null` | Yes (Spec-only, MYR-24) | miles | P0 | -- | Tesla `SelfDrivingMilesSinceReset` |
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

**Members:** `chargeLevel`, `estimatedRange`

**Server-side implementation:** Both fields are mapped from Tesla telemetry (`Soc`/`BatteryLevel` and `EstBatteryRange`) and delivered together in the same `vehicle_update` message when either field changes. Tesla emits these at a 30-second interval.

**Nullability:** Non-nullable. Both fields default to `0` in the database on vehicle creation. A value of `0` means either the battery is genuinely depleted or no telemetry has been received yet -- the UI should interpret `chargeLevel: 0` with `status: offline` as "unknown" and with `status: driving` as "critical".

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
| `fsdMilesToday` | Tesla `SelfDrivingMilesSinceReset` telemetry | Live stream; 60s interval |
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
    public let fsdMilesToday: Double?
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
| No `chargingState` in charge group | Tesla emits `DetailedChargeState` but the current Vehicle table and Go structs do not persist it. Adding it is a v1 stretch goal tracked separately. The `status` field set to `charging` provides a coarse equivalent. |
| No `timeToFull` in charge group | Not available from Tesla Fleet Telemetry in v1 field set. Deferred to v2. |
| No `tripStartTime` in navigation group | The requirements doc (NFR-3.1) lists `tripStartTime` in the navigation atomic group, but this field is not currently received from Tesla telemetry nor persisted in the Vehicle table. Deferred to v2 or implementation tracked separately. |
| Temperatures in Fahrenheit | Matches the current DB schema and frontend display. Conversion to user-preferred units is a UI concern, not an SDK concern. |
| `navRouteCoordinates` uses `[lng, lat]` order | GeoJSON/Mapbox standard. Tesla's raw protobuf uses `[lat, lng]`; the server converts on decode. |
| Integer rounding applied server-side | SDK consumers receive pre-rounded values. This prevents inconsistent rounding across TypeScript/Swift/etc. |

### 7.2 Open questions

| Question | Owner | Target |
|----------|-------|--------|
| **Schema vs `internal/store/types.go` gap (spec-only fields).** The canonical v1 schema defines seven fields that the current Go `Vehicle` struct does not load: `model`, `year`, `color`, `fsdMilesToday`, `locationName`, `locationAddress`, and `destinationAddress`. Until the Go struct is extended (and the SELECT / scan path in `internal/store` is updated), the server physically cannot populate these fields, so they are marked **nullable and `x-spec-only: true`** in both the MD (§1.1) and JSON Schema. SDK consumers MUST tolerate `null` for every spec-only field until MYR-24 lands. Once MYR-24 ships, these fields will be promoted back to non-nullable, the `x-spec-only` markers and the §1.1 callout removed, and this row closed. The gap is explicitly tracked in **[MYR-24](https://linear.app/myrobotaxi/issue/MYR-24)** ("Extend `internal/store.Vehicle` to load `model`/`year`/`color`/`fsdMilesToday`/`locationName`/`locationAddress`/`destinationAddress`"). | sdk-architect + go-engineer | **MYR-24** |
| Should `chargingState` (string enum) be added to the charge group in v1? | sdk-architect | MYR-TBD |
| Should `tripStartTime` be derived from drive detection events and added to nav group? | sdk-architect | MYR-TBD |
| Should temperature units be configurable (C/F) at the SDK level? | sdk-architect | v2 |

---

## Change log

| Date | Change | Author |
|------|--------|--------|
| 2026-04-09 | Initial draft -- all fields, atomic groups, consistency predicates, type generation docs | sdk-architect agent |
| 2026-04-09 | PR #161 review fixes: (1) mark 7 spec-only fields nullable + add §1.1 callout; (2) add §7.2 entry for MYR-24 Go struct gap; (3) clarify §3.1 predicates are `contract-tester`-enforced, not schema-enforced; (4) remove unreachable `$defs` sub-schemas from schema and rewrite §5.2; (5) fix latitude/longitude descriptions to reference the `0,0` convention instead of "nullable"; (6) align schema `chargeLevel: 0` semantics with §2.2 (context-dependent on `status`); update Swift struct in §6.2 to reflect new optionality | sdk-architect agent |
