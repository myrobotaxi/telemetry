---
name: Event Bus Design (Issue #2)
description: Architectural design for internal/events/ package - Bus interface, Event struct, typed topics, sealed EventPayload, domain event types, BusMetrics interface. Designed 2026-03-17.
type: project
---

## Event Bus Design Decisions

Designed for Issue #2 on 2026-03-17. This is the backbone module -- all inter-component communication flows through it.

### Key Decisions

1. **Concrete Event struct** (not interface) with sealed EventPayload marker interface for Payload field
2. **Topic as named string type** with package-level constants (not iota enum)
3. **Sealed interface pattern**: unexported `eventPayload()` method + exported `BasePayload` embed for domain event structs
4. **Bus interface**: Publish, Subscribe, Unsubscribe, Close(ctx) -- Unsubscribe on Bus, not on Subscription
5. **Handler returns error** but errors don't stop delivery (bus logs + meters errors)
6. **Domain events centralized in events/ package** to prevent circular imports between telemetry/, drives/, ws/, store/
7. **BusMetrics as interface** -- decoupled from prometheus/client_golang at the type level
8. **Constructor**: `NewBus(BusConfig, *slog.Logger, BusMetrics) *ChannelBus` -- accept interfaces, return struct
9. **TelemetryValue.Value is `any`** -- accepted exception because Tesla sends 200+ field types determined at runtime

### Topics

- telemetry.vehicle, telemetry.connectivity
- drive.started, drive.updated, drive.ended

### Files

11 files in internal/events/, all under 70 lines. One major type per file.
