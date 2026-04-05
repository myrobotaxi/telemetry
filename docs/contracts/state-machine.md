# State Machine Contract

**Status:** TODO — placeholder
**Target artifact:** State diagrams + transition tables
**Owner:** `sdk-architect` agent

## Purpose

Formalizes the two independent state dimensions the SDK exposes to consumers — **`connectionState`** (transport health) and **`dataState`** (per-atomic-group freshness) — plus the drive lifecycle state machine. This contract is shared verbatim by the TypeScript and Swift SDKs so both expose identical state transitions.

## Anchored requirements

- **FR-8.1** — two independent state dimensions exposed as separate enums
  - `connectionState`: `initializing | connecting | connected | disconnected | error`
  - `dataState` per atomic group: `loading | ready | stale | cleared | error`
- **FR-8.2** — UI composes the two dimensions; SDK never collapses them
- **FR-3.1** — drive lifecycle: `drive_started | drive_updated | drive_ended`
- **NFR-3.7, NFR-3.8, NFR-3.9** — freshness is event-driven (no client TTLs); stale only on server-signaled clear or WS disconnect; clears apply atomically per group
- **NFR-3.10, NFR-3.11** — reconnect behavior drives connection transitions; snapshot re-fetch resets `dataState` to `loading → ready`
- **NFR-3.12, NFR-3.13** — offline tolerance: cached state visible indefinitely

## Sections to author (TODO)

- [ ] `connectionState` state diagram + transition table (events, guards, actions)
- [ ] `dataState` per-group state diagram + transition table
- [ ] Drive lifecycle state machine (idle → driving → ended)
- [ ] Mapping of server events to state transitions
- [ ] Reconnect sequence diagram (disconnect → backoff → reconnect → snapshot → resume)
- [ ] Consumer usage examples (UI composition patterns, NOT implementations)
