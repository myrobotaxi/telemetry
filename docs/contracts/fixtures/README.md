# Canonical Fixtures

**Status:** Active -- v1 fixture library authored (35 fixtures across 4 directories)
**Target artifact:** Canonical payload library (JSON files)
**Owner:** `sdk-architect` agent + `contract-tester` agent
**Last updated:** 2026-04-15

## Purpose

Canonical payload library used for **Layer 1 -- Contract Conformance** of the v1 test bench ship-gate (`NFR-3.45`). Every WebSocket message type, REST response, and atomic group payload defined in the other contracts has at least one happy-path fixture here plus edge-case fixtures (nulls, cleared groups, boundary values, transitional states). Both SDKs parse these fixtures in round-trip tests; the server validates outgoing messages against them in CI.

## Anchored requirements

- **NFR-3.45** -- test bench ship-gate, Layer 1 (contract conformance): every WS message type and REST endpoint validated against AsyncAPI/OpenAPI spec; SDK parsing verified against canonical fixtures.
- **NFR-3.46** -- fixtures are consumed by both the TUI test bench (`cmd/testbench`) and the web test bench (`my-robo-taxi-testbench` standalone repo).

---

## Directory layout

```
fixtures/
  README.md                              -- this file (fixture index + rules)
  websocket/                             -- WebSocket envelope fixtures (auth, updates, events, errors)
  rest/                                  -- REST response body fixtures (snapshot, drives, errors)
  atomic-groups/                         -- Bare atomic group shapes (fields map only)
  edge-cases/                            -- Boundary conditions, null sentinels, transitional states
```

---

## File naming convention

Fixture files follow the pattern `<message-type>.<variant>.json`:

- **WebSocket:** `<type>.json` for happy-path, `<type>.<qualifier>.json` for variants (e.g., `vehicle_update.charge.json`, `vehicle_update.nav_clear.json`, `error.auth_failed.json`)
- **REST:** `<resource>.json` for happy-path, `<resource>.<qualifier>.json` for variants (e.g., `snapshot.json`, `error.not_found.json`)
- **Atomic groups:** `<group-name>.json` (e.g., `navigation.json`, `charge.json`)
- **Edge cases:** `<type>.<edge-condition>.json` (e.g., `vehicle_update.zero_position.json`, `snapshot.charging.json`)

---

## The `_meta` block

Every fixture MUST have a `_meta` block at the top level:

```json
{
  "_meta": {
    "description": "Human-readable description of what this fixture represents",
    "anchoredFRs": ["FR-1.1", "NFR-3.1"],
    "scenario": "happy-path"
  }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `description` | string | Yes | What the fixture tests and why it matters. Reference contract doc sections where relevant. |
| `anchoredFRs` | string[] | Yes | FR and NFR IDs from `requirements.md` that this fixture validates. |
| `scenario` | string (enum) | Yes | One of: `happy-path`, `edge-case`, `error`, `transitional`. |

### Fixture independence

Each fixture is a **standalone canonical payload** -- not a scene from a sequential timeline. Fixtures share consistent synthetic identifiers (same `vehicleId`, `userId`, `driveId`) for cross-referencing, but their timestamps and states are independent. Do not assume that the REST snapshot, the WS drive_started, and the WS vehicle_update fixtures form a coherent chronological sequence.

### `_meta` and JSON Schema validation

The `ws-envelope.schema.json` schema sets `additionalProperties: false`. The `_meta` block is NOT a member of the envelope schema. **Validation harnesses MUST strip `_meta` from the top-level object before validating a fixture against the schema.** This is a fixture-only concern -- production wire frames never contain `_meta`.

The recommended approach in test harnesses:

```javascript
// JavaScript/TypeScript
const { _meta, ...payload } = fixture;
validate(schema, payload);
```

```go
// Go
var raw map[string]json.RawMessage
json.Unmarshal(data, &raw)
delete(raw, "_meta")
data, _ = json.Marshal(raw)
// validate(data, schema)
```

```swift
// Swift
var dict = try JSONSerialization.jsonObject(with: data) as! [String: Any]
dict.removeValue(forKey: "_meta")
let cleaned = try JSONSerialization.data(withJSONObject: dict)
// validate(cleaned, schema)
```

---

## Fixture index

### WebSocket fixtures (`websocket/`)

| File | Schema reference | Scenario | Description |
|------|------------------|----------|-------------|
| `auth.json` | `ws-messages.schema.json#/$defs/AuthPayload` | happy-path | Client->server auth frame with opaque token |
| `auth_ok.json` | `ws-messages.schema.json#/$defs/AuthOkPayload` | happy-path | Server->client auth acknowledgement (triggers C-3) |
| `vehicle_update.charge.json` | `ws-messages.schema.json#/$defs/VehicleUpdatePayload` | happy-path | Charge atomic group -- all 4 members |
| `vehicle_update.charge_partial.json` | `ws-messages.schema.json#/$defs/VehicleUpdatePayload` | transitional | Historical pre-MYR-40 WS shape with chargeState/timeToFull null; retained as reference for REST /snapshot while DB persistence is pending |
| `vehicle_update.gps.json` | `ws-messages.schema.json#/$defs/VehicleUpdatePayload` | happy-path | GPS atomic group -- latitude, longitude, heading |
| `vehicle_update.gear.json` | `ws-messages.schema.json#/$defs/VehicleUpdatePayload` | happy-path | Gear atomic group -- gearPosition D, status driving |
| `vehicle_update.nav_active.json` | `ws-messages.schema.json#/$defs/VehicleUpdatePayload` | happy-path | Navigation group with active route (all 9 members) |
| `vehicle_update.nav_clear.json` | `ws-messages.schema.json#/$defs/VehicleUpdatePayload` | happy-path | Navigation cancelled -- all nav fields null (FR-2.3) |
| `vehicle_update.route.json` | `ws-messages.schema.json#/$defs/VehicleUpdatePayload` | happy-path | Drive route accumulator flush (routeCoordinates) |
| `vehicle_update.speed.json` | `ws-messages.schema.json#/$defs/VehicleUpdatePayload` | happy-path | Ungrouped individual field (speed only) |
| `drive_started.json` | `ws-messages.schema.json#/$defs/DriveStartedPayload` | happy-path | Drive lifecycle start with startedAt (tripStartTime) |
| `drive_ended.json` | `ws-messages.schema.json#/$defs/DriveEndedPayload` | happy-path | Drive lifecycle end with summary stats |
| `connectivity.online.json` | `ws-messages.schema.json#/$defs/ConnectivityPayload` | happy-path | Vehicle mTLS online |
| `connectivity.offline.json` | `ws-messages.schema.json#/$defs/ConnectivityPayload` | happy-path | Vehicle mTLS offline |
| `heartbeat.json` | `ws-messages.schema.json#/$defs/HeartbeatPayload` | happy-path | Bare keepalive (no payload) |
| `error.auth_failed.json` | `ws-messages.schema.json#/$defs/ErrorPayload` | error | Auth failure (precedes close 1008) |
| `error.auth_timeout.json` | `ws-messages.schema.json#/$defs/ErrorPayload` | error | Auth deadline exceeded |
| `error.rate_limited.json` | `ws-messages.schema.json#/$defs/ErrorPayload` | error | Per-user cap breach with subCode device_cap |

### REST fixtures (`rest/`)

| File | Schema reference | Scenario | Description |
|------|------------------|----------|-------------|
| `snapshot.json` | `vehicle-state.schema.json` (VehicleState) | happy-path | Full VehicleState from GET /api/vehicles/{vehicleId}/snapshot |
| `drives.json` | `rest.openapi.yaml#/components/schemas/PaginatedDrives` | happy-path | Paginated drive list (2 items, hasMore=true) |
| `drive_detail.json` | `rest.openapi.yaml#/components/schemas/DriveDetail` | happy-path | Full FR-3.4 drive record from GET /api/drives/{driveId} |
| `drive_route.json` | `rest.openapi.yaml#/components/schemas/DriveRoute` | happy-path | Drive route polyline from GET /api/drives/{driveId}/route |
| `error.not_found.json` | `rest.openapi.yaml#/components/schemas/ErrorEnvelope` | error | REST 404 error envelope |
| `error.invalid_request.json` | `rest.openapi.yaml#/components/schemas/ErrorEnvelope` | error | REST 400 error envelope |

### Atomic group fixtures (`atomic-groups/`)

| File | Schema reference | Scenario | Description |
|------|------------------|----------|-------------|
| `navigation.json` | `vehicle-state.schema.json` (x-atomic-groups.navigation) | happy-path | All 9 nav members with active route |
| `charge.json` | `vehicle-state.schema.json` (x-atomic-groups.charge) | happy-path | All 4 charge members |
| `gps.json` | `vehicle-state.schema.json` (x-atomic-groups.gps) | happy-path | latitude, longitude, heading |
| `gear.json` | `vehicle-state.schema.json` (x-atomic-groups.gear) | happy-path | gearPosition, status |

### Edge-case fixtures (`edge-cases/`)

| File | Schema reference | Scenario | Description |
|------|------------------|----------|-------------|
| `vehicle_update.nav_clear.json` | `ws-messages.schema.json#/$defs/VehicleUpdatePayload` | edge-case | Atomic nav clear (tests SDK amplification rule) |
| `vehicle_update.zero_position.json` | `ws-messages.schema.json#/$defs/VehicleUpdatePayload` | edge-case | 0,0 GPS sentinel (no fix, not Gulf of Guinea) |
| `vehicle_update.gear_null.json` | `ws-messages.schema.json#/$defs/VehicleUpdatePayload` | edge-case | null gearPosition with status offline |
| `snapshot.spec_only_nulls.json` | `vehicle-state.schema.json` (VehicleState) | edge-case | All spec-only + transitional fields null |
| `snapshot.charging.json` | `vehicle-state.schema.json` (VehicleState) | edge-case | Active charging: chargeState Charging, timeToFull non-null |
| `drive_ended.micro_drive.json` | `ws-messages.schema.json#/$defs/DriveEndedPayload` | edge-case | Minimal drive that passed micro-drive filter |
| `error.rate_limited_device_cap.json` | `ws-messages.schema.json#/$defs/ErrorPayload` | edge-case | rate_limited with subCode device_cap |

---

## Synthetic data values

All fixtures use consistent synthetic data:

| Entity | Value | Notes |
|--------|-------|-------|
| vehicleId | `clxyz1234567890abcdef` | Consistent across all fixtures |
| userId | `clxyz1234567890userid` | Used in auth_ok |
| driveId (primary) | `clmno9876543210zyxw0001` | Used in drive_started, drive_ended, drive_detail, drive_route |
| driveId (secondary) | `clmno9876543210zyxw0002` | Used in drives list (second item), micro_drive edge case |
| Timestamps | `2026-04-13T18:22:00Z` vicinity | Consistent with rest-api.md examples |
| GPS coordinates | 37.7749, -122.4194 | San Francisco area |
| Speed | 35 mph | City driving |
| Charge | 78% / 245 miles | Typical daily charge |
| Nav destination | "Whole Foods Market" | Matches schema examples |
| Vehicle name | "Stumpy" | Consistent with rest-api.md examples |

Coordinate order in arrays: `[longitude, latitude]` (GeoJSON/Mapbox convention).

---

## Rules for adding new fixtures

When a new WebSocket message type, REST endpoint, or atomic group is added to the contract:

1. **Add at least one happy-path fixture** in the appropriate directory (`websocket/`, `rest/`, or `atomic-groups/`).
2. **Add edge-case fixtures** for any boundary condition, null sentinel, or transitional state in `edge-cases/`.
3. **Every fixture MUST have a `_meta` block** with `description`, `anchoredFRs`, and `scenario`.
4. **Use consistent synthetic data** from the table above. Extend the table if new entities are needed.
5. **Validate JSON** before committing: `python3 -c "import json; json.load(open('path/to/fixture.json'))"`.
6. **Update this README** with a row in the fixture index table.
7. **Validate against schemas** using the `_meta`-stripping approach documented above.
8. **Wire into contract-tester** so the fixture is consumed by CI round-trip tests.

---

## Consumers

| Consumer | How it uses fixtures |
|----------|---------------------|
| `contract-tester` | Round-trip parse/serialize tests for every fixture against its schema |
| TypeScript SDK (`sdk-typescript`) | Type guard validation, snapshot hydration tests, WS message parsing tests |
| Swift SDK (`sdk-swift`) | Codable round-trip tests, state machine transition tests |
| Go server (`go-engineer`) | Outbound message shape validation in integration tests |
| TUI test bench (`cmd/testbench`) | Reference payloads for expected message shapes |
| Web test bench | UI rendering tests with known fixture data |

---

## Change log

| Date | Change | Author |
|------|--------|--------|
| 2026-04-15 | Initial fixture library (MYR-13): 18 WebSocket fixtures, 6 REST fixtures, 4 atomic-group fixtures, 7 edge-case fixtures. Full README with index, naming convention, _meta spec, validation guidance, and rules for adding fixtures. | sdk-architect |
