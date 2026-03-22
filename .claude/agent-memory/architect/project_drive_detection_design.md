---
name: Drive Detection Design (Issue #11)
description: State machine design for internal/drives/ - Detector struct, per-vehicle state with mutex, debounce timers, FSD tracking, DriveStats extension, no store dependency. Designed 2026-03-18.
type: project
---

## Drive Detection Design Decisions

Designed for Issue #11 on 2026-03-18. Per-vehicle state machine that detects drive start/end from gear telemetry.

### Key Decisions

1. **Detector subscribes to TopicVehicleTelemetry only** -- all input comes through the event bus
2. **Per-vehicle state via sync.Map** keyed by VIN, with per-state sync.Mutex for debounce timer coordination
3. **Debounce via time.AfterFunc + mutex** -- timer callback locks state.mu before ending drive; simplest correct approach
4. **No store dependency** -- detector publishes DriveStarted/Updated/Ended events; store subscriber (future) handles persistence
5. **DriveStats extended** with StartChargeLevel, EndChargeLevel, FSDMiles, FSDPercentage, RoutePoints fields
6. **RoutePoints included in DriveEndedEvent** so store subscriber is stateless (doesn't need to accumulate from DriveUpdatedEvent)
7. **FSD per-trip tracking**: snapshot fsdMilesSinceReset at start/end, delta = FSD miles, percentage = fsdMiles/distance*100
8. **Micro-drive filtering**: discard if duration < MinDuration OR distance < MinDistanceMiles (OR logic, not AND)
9. **Gear values are strings** "P", "D", "R", "N" -- only D/R trigger start, only P triggers debounce
10. **No VehicleReader/DriveWriter interfaces in drives/** -- those belong with the store subscriber when implemented
11. **8 files** in internal/drives/, all under 300 lines

### Design Doc

Full design at docs/design/011-drive-detection.md

**Why:** Prior store design (004) assumed drives/ would define VehicleReader+DriveWriter interfaces. Revised: detector is pure event-in/event-out, no store coupling.

**How to apply:** When implementing the store subscriber that persists drives, define the interfaces in that subscriber's package, not in internal/drives/.
