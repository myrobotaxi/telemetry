# WebSocket Protocol Contract

**Status:** TODO — placeholder
**Target artifact:** AsyncAPI 3.0 specification
**Owner:** `sdk-architect` agent

## Purpose

Defines every WebSocket message exchanged between the telemetry server and SDK clients (TypeScript and Swift). This contract binds the server's broadcaster to the SDKs' parsers and is the authoritative source for message shapes, atomic group payloads, connection handshake, heartbeat/ping cadence, and error framing.

## Anchored requirements

- **FR-1.1, FR-1.2, FR-1.3** — live vehicle telemetry stream (position, speed, heading, gear, charge, extensibility)
- **FR-2.1, FR-2.2, FR-2.3** — nav group streaming and atomic clear on cancel
- **FR-3.1** — live drive events (`drive_started`, `drive_updated`, `drive_ended`)
- **FR-8.1, FR-8.2** — `connectionState` surface and independent `dataState` per group
- **NFR-3.1, NFR-3.2** — server-side atomic grouping with 200ms debounce
- **NFR-3.10, NFR-3.11** — reconnect handshake, snapshot resume

## Sections to author (TODO)

- [ ] Connection handshake (auth header, token in first frame, server accept/reject)
- [ ] Server→client message catalog (one entry per atomic group + drive events + clears + error frames)
- [ ] Client→server message catalog (subscribe, unsubscribe, ping)
- [ ] Heartbeat / keepalive cadence
- [ ] Close codes and reconnection semantics
- [ ] AsyncAPI document link + generation instructions
- [ ] Canonical example payloads (link to `fixtures/`)
