# Data Lifecycle Contract

**Status:** TODO — placeholder
**Target artifact:** Lifecycle policy doc + DB schema notes
**Owner:** `sdk-architect` agent

## Purpose

Defines — for every persisted field — its **single source of truth**, its **retention window**, its **deletion semantics**, and the **audit log entry** written on user-initiated deletion. Enforces the "raw telemetry is never persisted as a historical log" principle (`requirements.md` §1 design principle 5).

## Anchored requirements

- **FR-10.1** — user-initiated deletion of all user data (drive history, vehicle snapshot, invites, sessions)
- **FR-10.2** — immutable audit log entry per deletion (user ID, timestamp, what, initiator)
- **NFR-3.27** — drive records: 1 year rolling window, background pruning >365 days
- **NFR-3.28** — raw telemetry NOT persisted; only `Vehicle` snapshot (overwritten) and `Drive.routePoints` (bounded by drive lifetime)
- **NFR-3.29** — audit logs retained indefinitely

## Sections to author (TODO)

- [ ] Single-source-of-truth mapping (per field: DB vs. WebSocket-only)
- [ ] Retention windows per table
- [ ] Deletion cascade and ordering for FR-10.1
- [ ] Audit log schema (table definition, write path, immutability enforcement)
- [ ] Pruning job definition (schedule, batch size, failure handling)
- [ ] Partial-group persistence rules (NFR-3.3 alignment with schema contract)
