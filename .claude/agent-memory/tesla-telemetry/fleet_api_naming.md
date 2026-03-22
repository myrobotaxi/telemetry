---
name: Fleet API type naming convention
description: Types for Tesla Fleet API use Fleet prefix (not Telemetry) to avoid stutter with telemetry package name
type: feedback
---

Fleet API types in `internal/telemetry/` use the `Fleet` prefix instead of `Telemetry` to avoid Go stutter warnings (e.g., `FleetConfigRequest` not `TelemetryConfigRequest`).

**Why:** The `revive` linter flags `telemetry.TelemetryConfigRequest` as stuttering since the type name repeats the package name. All seven Fleet API types were renamed during implementation.

**How to apply:** When adding new types to the telemetry package that relate to the Fleet API, use the `Fleet` prefix. For types that relate to the telemetry receiver itself, neither prefix is needed since the package already provides context.
