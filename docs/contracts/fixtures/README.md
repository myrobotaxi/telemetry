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
