# Canonical Fixtures

**Status:** TODO — placeholder
**Target artifact:** Canonical payload library (JSON files)
**Owner:** `sdk-architect` agent + `contract-tester` agent

## Purpose

Canonical payload library used for **Layer 1 — Contract Conformance** of the v1 test bench ship-gate (`NFR-3.45`). Every WebSocket message type, REST response, and atomic group payload defined in the other contracts has at least one happy-path fixture here plus edge-case fixtures (nulls, cleared groups, boundary values, malformed messages). Both SDKs parse these fixtures in round-trip tests; the server validates outgoing messages against them in CI.

## Anchored requirements

- **NFR-3.45** — test bench ship-gate, Layer 1 (contract conformance): every WS message type and REST endpoint validated against AsyncAPI/OpenAPI spec; SDK parsing verified against canonical fixtures.
- **NFR-3.46** — fixtures are consumed by both the TUI test bench (`cmd/testbench`) and the web test bench (`my-robo-taxi-testbench` standalone repo).

## Sections to author (TODO)

- [ ] Directory layout (`websocket/`, `rest/`, `atomic-groups/`, `edge-cases/`)
- [ ] File naming convention (`<message-type>.<scenario>.json`)
- [ ] Fixture index (one row per fixture: name, references schema, description)
- [ ] Round-trip validation harness reference
- [ ] Rules for adding a fixture when a new message/schema is introduced

## Planned WebSocket fixtures (MYR-11 forward-references)

The [`websocket-protocol.md`](../websocket-protocol.md) catalog (added in MYR-11) links every server->client and client->server message type to a fixture under `fixtures/websocket/`. The fixture files themselves are not yet authored — the table below is the TODO checklist for the follow-up issue that will land them. Until then, the markdown links resolve to 404 in the rendered docs; this is intentional and tracked in `websocket-protocol.md` §10 follow-up #9.

| Planned path | Source schema | Description |
|--------------|---------------|-------------|
| `websocket/auth.json` | `schemas/ws-messages.schema.json#/$defs/AuthPayload` | Happy-path client->server auth frame |
| `websocket/vehicle_update.charge.json` | `schemas/ws-messages.schema.json#/$defs/VehicleUpdatePayload` | Atomic charge group update (chargeLevel + estimatedRange) |
| `websocket/vehicle_update.gps.json` | `schemas/ws-messages.schema.json#/$defs/VehicleUpdatePayload` | Atomic GPS group update (latitude + longitude + heading) |
| `websocket/vehicle_update.gear.json` | `schemas/ws-messages.schema.json#/$defs/VehicleUpdatePayload` | Atomic gear group update (gearPosition + derived status) |
| `websocket/vehicle_update.nav_active.json` | `schemas/ws-messages.schema.json#/$defs/VehicleUpdatePayload` | Atomic navigation group update with active route, ETA, polyline |
| `websocket/vehicle_update.nav_clear.json` | `schemas/ws-messages.schema.json#/$defs/VehicleUpdatePayload` | Atomic navigation clear (Tesla nav cancelled — every nav field null) |
| `websocket/vehicle_update.route.json` | `schemas/ws-messages.schema.json#/$defs/VehicleUpdatePayload` | Drive route accumulator flush carrying `routeCoordinates` |
| `websocket/drive_started.json` | `schemas/ws-messages.schema.json#/$defs/DriveStartedPayload` | Drive detector announcing a new drive |
| `websocket/drive_ended.json` | `schemas/ws-messages.schema.json#/$defs/DriveEndedPayload` | Completed drive with summary stats |
| `websocket/connectivity.online.json` | `schemas/ws-messages.schema.json#/$defs/ConnectivityPayload` | Vehicle mTLS connection came online |
| `websocket/connectivity.offline.json` | `schemas/ws-messages.schema.json#/$defs/ConnectivityPayload` | Vehicle mTLS connection went offline |
| `websocket/heartbeat.json` | `schemas/ws-messages.schema.json#/$defs/HeartbeatPayload` | Bare server keepalive frame |
| `websocket/error.auth_failed.json` | `schemas/ws-messages.schema.json#/$defs/ErrorPayload` | Auth failure error frame (precedes close code 1008) |
| `websocket/error.auth_timeout.json` | `schemas/ws-messages.schema.json#/$defs/ErrorPayload` | Auth deadline exceeded error frame |

> **Owner note:** Fixture authoring is non-blocking for MYR-11 (the spec captures the wire shape exhaustively via JSON Schemas + AsyncAPI). The follow-up issue must (a) emit each fixture above, (b) validate it against its schema in CI, and (c) wire the fixtures into the `contract-tester` round-trip suite for both the TypeScript and Swift SDKs.
