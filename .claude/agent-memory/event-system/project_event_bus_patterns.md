---
name: event-bus-implementation-patterns
description: Design decisions and concurrency patterns established in the event bus implementation (Issue #2)
type: project
---

## Event Bus Implementation (Issue #2, 2026-03-17)

### Architecture Decisions
- Per-subscriber goroutine + buffered channel (not a single fan-out goroutine)
- Drop-oldest backpressure: when subscriber buffer is full, oldest event is evicted via non-blocking receive, then new event is sent
- Publisher never blocks: the sendToSubscriber method uses three non-blocking selects (try send, try drain oldest, try send again)
- Topic entries are lazily created on first Subscribe using double-checked locking pattern

### Backpressure Tuning
- Default buffer size: 256 per subscriber
- Default drain timeout: 5 seconds
- The drop-oldest implementation handles a race where the channel is drained concurrently between the "full" detection and the "drain oldest" step — handled via nested default cases

### Concurrency Structure
- `sync.RWMutex` on the bus for the topics map (read-heavy path)
- `sync.RWMutex` per topicEntry for the subscribers map
- `sync/atomic.Bool` for closed state (lock-free fast path in Publish)
- `sync.WaitGroup` tracks all subscriber goroutines for graceful shutdown

### Shutdown Sequence
1. CompareAndSwap closed flag (idempotent)
2. Collect all active subscribers under read lock
3. Close all subscriber `done` channels (signals drain)
4. Wait for WaitGroup with context timeout
5. Each goroutine drains its buffered channel before exiting

### Benchmarks (Apple M4)
- Publish with 10 subscribers: ~960ns/op, 1 alloc (subscriber slice)
- Publish with no subscribers: ~57ns/op, 0 allocs

### Testing Notes
- Tests use internal package (no `_test` suffix) to test internal types directly
- countingMetrics helper type for asserting metric calls
- Backpressure tests need careful timing: first event must be consumed by goroutine before filling buffer
- SlowSubscriberDoesNotBlockFast test uses buffer=256 to avoid fast subscriber drops from buffer overflow
