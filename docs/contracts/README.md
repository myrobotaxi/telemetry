# MyRoboTaxi SDK v1 — Contracts

**Status:** Active — v1 contracts authored (six markdown docs + four machine-readable specs + 35 canonical fixtures).
**Owner:** `sdk-architect` agent
**Anchors:** All contracts in this directory trace back to [`docs/architecture/requirements.md`](../architecture/requirements.md).

---

## What lives here

This directory holds the **machine- and human-readable contracts** that bind the telemetry server, the TypeScript SDK, the Swift SDK, and the web/mobile consumers together. Every wire message, persisted field, and public SDK type has its authoritative definition here.

These contracts are the single source of truth. If the code and the contract disagree, the contract wins and the code is a bug.

---

## The seven contract documents

| Document | Purpose | Target artifact |
|----------|---------|-----------------|
| [`websocket-protocol.md`](websocket-protocol.md) | Defines every WebSocket message exchanged between server and clients: message shapes, atomic group payloads, connection lifecycle, server→client and client→server message catalogs. | AsyncAPI 3.0 spec at [`specs/websocket.asyncapi.yaml`](specs/websocket.asyncapi.yaml) + JSON Schemas under [`schemas/`](schemas/) |
| [`rest-api.md`](rest-api.md) | Defines REST endpoints for snapshot fetches, drive detail fetches (`GET /drives/{id}` — the canonical source for the full FR-3.4 drive record per DV-11, paired with the `drive_ended` WebSocket summary and the SDK's `fetchDrive(driveId)` helper), drive history pagination (cursor-based), sharing/invite flows, and user data deletion. Reuses the shared bearer-token auth scheme and typed error catalog from the WebSocket contract. | OpenAPI 3.1 spec at [`specs/rest.openapi.yaml`](specs/rest.openapi.yaml) |
| [`vehicle-state-schema.md`](vehicle-state-schema.md) | Canonical JSON Schema for vehicle, nav, charge, GPS, and gear state. Declares atomic groups and per-field types, nullability, and units. | JSON Schema draft-2020-12 at [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json) |
| [`data-classification.md`](data-classification.md) | Labels every persisted field P0 (public), P1 (sensitive, encrypted at rest), or P2 (sensitive + access-logged). Drives logging redaction rules and encryption boundaries. | Reference table |
| [`data-lifecycle.md`](data-lifecycle.md) | Retention windows, deletion semantics, audit log format, and the single-source-of-truth rule for every persisted field. | Policy doc + DB schema notes |
| [`state-machine.md`](state-machine.md) | Connection state machine (`initializing | connecting | connected | disconnected | error`), drive lifecycle states, and per-group data freshness states (`loading | ready | stale | cleared | error`). | State diagrams + transition tables |
| [`fixtures/README.md`](fixtures/README.md) | Index of canonical payload fixtures used for contract conformance testing across both SDKs and the server. | Fixture library |

### Machine-readable specs and schemas

The human-readable contract docs above are paired with machine-readable specs and schemas that SDK code generators, contract-tester, and contract-guard consume directly. Drift between a markdown doc and its machine twin is a CI failure.

| File | Anchored doc | Purpose |
|------|--------------|---------|
| [`specs/websocket.asyncapi.yaml`](specs/websocket.asyncapi.yaml) | [`websocket-protocol.md`](websocket-protocol.md) | AsyncAPI 3.0 description of the `/api/ws` channel, every server↔client message, and the auth security scheme. References JSON Schemas via `$ref` rather than duplicating payloads. |
| [`specs/rest.openapi.yaml`](specs/rest.openapi.yaml) | [`rest-api.md`](rest-api.md) | OpenAPI 3.1 description of every REST endpoint (snapshot, drives list, drive detail, drive route, invite lifecycle, user deletion), the shared bearer-token security scheme, the REST-only extensions to the typed error catalog, and the cursor-based pagination wrapper. References `vehicle-state.schema.json` via `$ref` for the snapshot response; REST-only shapes are declared inline. |
| [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json) | [`vehicle-state-schema.md`](vehicle-state-schema.md) | Canonical `VehicleState` shape, atomic-group annotations, classification labels. |
| [`schemas/ws-envelope.schema.json`](schemas/ws-envelope.schema.json) | [`websocket-protocol.md`](websocket-protocol.md) §3 | Top-level envelope (`type` discriminator, `payload`, planned `seq`/`ts`). |
| [`schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json) | [`websocket-protocol.md`](websocket-protocol.md) §4–§5 | Per-message payload schemas: auth, vehicle_update, drive_started, drive_ended, connectivity, heartbeat, error, plus the planned subscribe/unsubscribe/ping/pong control messages. |

---

## How the contracts relate

```
                     requirements.md (FRs + NFRs)
                              │
                              ▼
                    data-classification.md
                              │
              ┌───────────────┼───────────────┐
              ▼               ▼               ▼
   vehicle-state-schema   data-lifecycle   state-machine
              │               │               │
              └───────┬───────┴───────┬───────┘
                      ▼               ▼
              websocket-protocol   rest-api
                      │               │
                      └───────┬───────┘
                              ▼
                       fixtures/ (canonical payloads)
                              │
                              ▼
                     contract-tester (CI gate)
```

- **`data-classification.md`** is foundational — it labels fields P0/P1/P2, which every other contract references.
- **`vehicle-state-schema.md`** defines the shape of state (fields, types, atomic groups). Both the wire protocol and the DB lifecycle reference it.
- **`data-lifecycle.md`** pins each field to its source of truth (DB row vs. WebSocket event) and retention rules.
- **`state-machine.md`** defines transitions consumed by both protocols — e.g., `connectionState` affects WebSocket framing, `dataState` per group is exposed over both transports.
- **`websocket-protocol.md`** and **`rest-api.md`** are the wire-level contracts derived from the above.
- **`fixtures/`** holds canonical payloads (happy-path and edge-case) validated against the schemas, consumed by `contract-tester`, both SDKs, and the web test bench.

---

## How consumers use these contracts

- **TypeScript SDK (`sdk-typescript`)** generates TypeScript types from `vehicle-state-schema.md` and the AsyncAPI/OpenAPI specs, wires parsers and type guards against the fixtures, and exposes `connectionState` / per-group `dataState` from `state-machine.md`.
- **Swift SDK (`sdk-swift`)** generates Swift types the same way and consumes the same fixtures for round-trip parse tests.
- **Go server (`go-engineer`, `tesla-telemetry`)** validates outgoing messages against the schemas and enforces atomic-group debouncing per `NFR-3.1`/`NFR-3.2`.
- **`contract-tester`** runs the four ship-gate layers (`NFR-3.45`): conformance, FR validation, NFR measurement, chaos. Every layer reads from these contracts.
- **`contract-guard`** blocks any PR that drifts from these contracts — see merge policy below.

---

## How to update a contract

Contract changes are high-stakes. Follow this workflow:

1. **Open a Linear issue** anchored to the FR/NFR that justifies the change. If no FR/NFR fits, update `requirements.md` first in a separate issue.
2. **Draft the contract change on a feature branch.** Update the contract doc AND any code/fixtures that depend on it in the same PR. A contract PR that leaves the code out of sync will be blocked.
3. **Tag `Agent/sdk-architect` on the PR.** The architect performs the Contract Adherence review per `CLAUDE.md` §Merge Policy, verifying:
   - Atomic group integrity (nav/charge/GPS/gear)
   - Data classification label for every new persisted field
   - Latency budget preserved (NFR-3.1)
   - Change is in v1 scope (not in `requirements.md` §5 Out-of-Scope)
4. **`contract-guard` must pass.** It runs session-time against the working diff and again in CI as a required check. Drift — missing contract updates, missing P0/P1/P2 labels, broken atomic groups — blocks merge unconditionally.
5. **Bump SDK versions if the wire changes.** Per `NFR-3.37`, a breaking protocol change requires a major version bump and a migration note.
6. **Update fixtures.** Any schema or message shape change requires corresponding fixture updates so `contract-tester` stays green.

**Never bypass this workflow.** No `--admin` merges, no silent drift, no contract-then-code-later split PRs.

---

## Related docs

- [`docs/architecture/requirements.md`](../architecture/requirements.md) — FRs and NFRs (the north star)
- [`docs/architecture/`](../architecture/) — higher-level architecture and design notes
- [`CLAUDE.md`](../../CLAUDE.md) — agent routing, merge policy, and contract enforcement rules
