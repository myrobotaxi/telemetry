---
name: events package test coverage baseline
description: Coverage metrics and test patterns for internal/events/ as of 2026-03-17 after Issue #2 review
type: project
---

## internal/events/ Coverage: 96.2% (target 80%)

### Test count: 27 tests + 2 benchmarks

### Coverage gaps that are intentionally accepted:
- `NoopBusMetrics` methods (0%) -- trivial empty one-liners, used as test infrastructure
- `BasePayload.eventPayload()` (0%) -- sealed marker method, zero logic
- `deliverLoop` `!ok` branch (88.9%) -- defensive channel-closed guard, not reachable in normal operation
- `drainSubscriber` `!ok` branch (85.7%) -- same pattern
- `getOrCreateTopicEntry` race-check path (91.7%) -- double-checked locking, hard to deterministically trigger

### Test infrastructure patterns established:
- `testBus(bufSize int)` helper creates a bus with NoopBusMetrics and discarded logger
- `testPayload` struct with BasePayload embedding for simple test events
- `countingMetrics` struct with atomic fields for verifying metric calls (includes lastSubCount)
- `testLogger()` returns slog.Logger that discards output
- `prometheus.NewRegistry()` for isolated Prometheus metric tests
- All tests use `time.After()` deadlines, never unbounded waits
- Backpressure tests use `gate` channels to control handler blocking
