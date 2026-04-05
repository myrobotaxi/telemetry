# Vehicle State Schema Contract

**Status:** TODO — placeholder
**Target artifact:** JSON Schema (draft-2020-12)
**Owner:** `sdk-architect` agent

## Purpose

Canonical shape of vehicle state as consumed by the SDKs and rendered by consumer UIs. Defines every field name, type, unit, nullability rule, and — critically — which fields belong to which **atomic group**. Both the WebSocket protocol and the REST snapshot endpoint return subsets of this schema. Both SDKs generate types from it.

## Anchored requirements

- **FR-1.1, FR-1.2** — telemetry field set (position, speed, heading, gear, battery, charge state, range)
- **FR-2.1** — nav field set (destinationName, ETA, polyline, origin, etc.)
- **FR-4.2** — vehicle-scoped API signatures
- **NFR-3.1** — atomic groups declared here: `navigation`, `charge`, `gps`, `gear`
- **NFR-3.3, NFR-3.4** — self-consistency rules (partial groups are invalid)
- **NFR-3.5** — every UI-rendered field is persisted and returned in the snapshot

## Sections to author (TODO)

- [ ] Root `VehicleState` schema
- [ ] Atomic group sub-schemas: `navigation`, `charge`, `gps`, `gear`
- [ ] Per-field types, units (km vs mi, kW vs kWh, degrees), nullability rules
- [ ] Atomic-group consistency predicates (NFR-3.3)
- [ ] Field-to-source-of-truth mapping (see `data-lifecycle.md`)
- [ ] JSON Schema file link + type generation targets (TS via json-schema-to-typescript, Swift via generator)
