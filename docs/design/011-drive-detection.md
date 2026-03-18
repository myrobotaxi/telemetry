# Design: Drive Detection State Machine (Issue #11)

**Date:** 2026-03-18
**Status:** Proposed
**Author:** Architect agent

---

## 1. Overview

The drive detector subscribes to `VehicleTelemetryEvent` on the event bus and
maintains a per-vehicle state machine that transitions between Idle and Driving.
When a drive starts it publishes `DriveStartedEvent`, accumulates route points
(publishing `DriveUpdatedEvent`), and when the drive ends it calculates final
stats and publishes `DriveEndedEvent`. The detector does NOT write to the
database -- downstream subscribers handle persistence.

```
         Gear = D or R
Idle ──────────────────────► Driving
  ^                              |
  |   Gear = P                   |
  |   + 30s debounce elapsed     |
  |   + duration >= 2 min        |
  |   + distance >= 0.1 miles    |
  └──────────────────────────────┘
        (else discard micro-drive)
```

---

## 2. Package Structure

```
internal/drives/
  detector.go         -- Detector struct, constructor, Start/Stop, event handler
  state.go            -- vehicleState struct, DriveStatus enum, activeDrive struct
  transitions.go      -- handleTelemetry dispatch, transitionToIdle, transitionToDriving
  debounce.go         -- debounce timer management (per-vehicle)
  stats.go            -- haversine, distance, speed, energy, FSD calculations
  errors.go           -- domain error variables
  detector_test.go    -- tests for Detector (state transitions, edge cases)
  stats_test.go       -- tests for haversine and stats calculations
```

All files under 300 lines. One major concern per file.

---

## 3. Detector Struct

```go
// Detector subscribes to vehicle telemetry events and maintains a per-vehicle
// state machine that detects drive start/end transitions. It publishes drive
// lifecycle events back to the bus. The detector does not persist anything
// directly -- downstream event subscribers handle that.
type Detector struct {
    bus     events.Bus
    cfg     config.DrivesConfig
    logger  *slog.Logger
    metrics DetectorMetrics

    // states holds per-vehicle drive state. Keyed by VIN.
    // Using sync.Map because vehicles connect/disconnect dynamically and
    // reads vastly outnumber writes (every telemetry tick is a read;
    // new vehicle connections are rare writes).
    states sync.Map // map[string]*vehicleState

    sub    events.Subscription // telemetry subscription
    cancel context.CancelFunc // for stopping debounce timers
}
```

### Constructor

```go
func NewDetector(
    bus events.Bus,
    cfg config.DrivesConfig,
    logger *slog.Logger,
    metrics DetectorMetrics,
) *Detector
```

Accept interfaces, return concrete struct. The `bus` parameter is the only
external dependency. No store interfaces are needed because the detector
communicates exclusively through events.

### Lifecycle

```go
// Start subscribes to TopicVehicleTelemetry and begins processing.
// The context controls the lifetime of background goroutines (debounce timers).
func (d *Detector) Start(ctx context.Context) error

// Stop unsubscribes from the bus and cancels all debounce timers.
// Active drives are NOT forcibly ended -- they remain in memory and will
// resume if the vehicle reconnects. A separate reaper (future issue) handles
// stale drives.
func (d *Detector) Stop() error
```

`Start` subscribes to `TopicVehicleTelemetry` with a handler that type-asserts
the payload to `VehicleTelemetryEvent` and dispatches to `handleTelemetry`.
It also stores a cancel function for the parent context that governs all
debounce timer goroutines.

---

## 4. Per-Vehicle State

```go
// DriveStatus represents the current state of a vehicle's drive detection.
type DriveStatus int

const (
    StatusIdle    DriveStatus = iota
    StatusDriving
)

// vehicleState tracks the drive-detection state for a single vehicle.
// Each vehicle gets its own instance stored in the Detector's sync.Map.
// All access to a vehicleState is serialized by the event bus -- the bus
// delivers events to a single handler goroutine, and the handler processes
// them sequentially. Therefore vehicleState does NOT need a mutex.
//
// IMPORTANT CONCURRENCY NOTE: The event bus guarantees serial delivery per
// subscription. Since we have a single subscription to TopicVehicleTelemetry,
// all telemetry events (across all vehicles) are delivered sequentially to
// our handler. This means vehicleState does not need internal synchronization.
// The sync.Map is needed only because LoadOrStore is used to lazily initialize
// state for new VINs.
type vehicleState struct {
    status DriveStatus
    drive  *activeDrive // non-nil only when status == StatusDriving

    // debounceTimer is set when gear transitions to P during a drive.
    // If gear returns to D/R before the timer fires, the timer is cancelled
    // and the drive continues. If the timer fires, the drive ends.
    debounceTimer *time.Timer

    // lastGear caches the most recent gear value to detect transitions.
    lastGear string

    // lastLocation caches the most recent location for drives that start
    // without a location in the triggering event.
    lastLocation *events.Location
}

// activeDrive accumulates data during an in-progress drive.
type activeDrive struct {
    id             string    // generated drive ID (hex, same format as event IDs)
    startedAt      time.Time
    startLocation  events.Location
    routePoints    []events.RoutePoint
    maxSpeed       float64
    speedSum       float64   // running sum for average calculation
    speedCount     int       // number of speed samples
    startCharge    float64   // SOC at drive start (percent)
    startOdometer  float64   // odometer at drive start (miles)
    startEnergy    float64   // energyRemaining at drive start (kWh)
    startFSDMiles  float64   // fsdMilesSinceReset at drive start
    lastFSDMiles   float64   // most recent fsdMilesSinceReset seen
    lastLocation   events.Location
    lastTimestamp  time.Time
}
```

### Why no mutex on vehicleState

The event bus delivers events to a subscription's handler sequentially (one at
a time). Since the Detector registers a single subscription to
`TopicVehicleTelemetry`, all telemetry events across all vehicles flow through
one handler goroutine in order. This means:

- Two events for the same VIN are never processed concurrently.
- Two events for different VINs are never processed concurrently.

The `sync.Map` is used purely for lazy initialization (`LoadOrStore` when a
VIN is seen for the first time) and is safe for concurrent access between the
handler goroutine and the `Stop` method.

The only concurrent access risk is debounce timer callbacks firing on a
separate goroutine. This is addressed in section 6.

---

## 5. State Transitions

### handleTelemetry (transitions.go)

This is the main dispatch function called by the event bus handler.

```
handleTelemetry(ctx, VehicleTelemetryEvent):
  1. Extract VIN from event
  2. LoadOrStore vehicleState for this VIN (lazy init to StatusIdle)
  3. Extract gear from event.Fields[FieldGear] (may be absent)
  4. Cache current location if present in event.Fields[FieldLocation]
  5. Switch on state.status:
     - StatusIdle:   call handleIdle(ctx, state, event)
     - StatusDriving: call handleDriving(ctx, state, event)
```

### Idle -> Driving (transitionToDriving)

**Trigger:** `event.Fields["gear"].StringVal` is `"D"` or `"R"` AND the
vehicle is currently in `StatusIdle`.

**Actions:**
1. Generate a new drive ID.
2. Extract start location from `event.Fields["location"]`. If location is
   absent, use `state.lastLocation`. If that is also nil, use zero-value
   Location (the drive started without GPS -- this is possible if telemetry
   arrives before GPS lock).
3. Snapshot initial values from the event's fields:
   - `startCharge` from `FieldSOC` (if present)
   - `startOdometer` from `FieldOdometer` (if present)
   - `startEnergy` from `FieldEnergyRemaining` (if present)
   - `startFSDMiles` from `FieldFSDMiles` (if present, else 0)
4. Create `activeDrive` struct with these values.
5. Set `state.status = StatusDriving`, `state.drive = &activeDrive{...}`.
6. Publish `DriveStartedEvent` to the bus.

**What about Neutral (N)?** Tesla vehicles briefly pass through N when
shifting from P to D. We treat N as "not yet driving" -- the transition
only fires on D or R. If the vehicle is already driving and shifts to N
(e.g., coasting), we do NOT end the drive. Only P triggers the end-drive
debounce.

### Driving -> Idle (transitionToIdle)

This is a two-phase process due to debounce.

**Phase 1 -- Gear = P detected (start debounce):**
1. If `state.debounceTimer` is already running, do nothing (already debouncing).
2. Start a `time.AfterFunc(cfg.EndDebounce, callback)` timer.
3. Store the timer in `state.debounceTimer`.
4. The drive remains in `StatusDriving` during debounce. Route points continue
   to accumulate (the vehicle might still be reporting speed/location).

**Phase 2 -- Debounce timer fires (endDrive callback):**
1. The callback runs on a timer goroutine, NOT the event bus goroutine.
   To avoid data races, the callback publishes a sentinel event through the
   bus rather than mutating state directly. See section 6 for details.
2. Calculate final stats (section 7).
3. Check micro-drive thresholds:
   - Duration < `cfg.MinDuration` (default 2m) OR
   - Distance < `cfg.MinDistanceMiles` (default 0.1 mi)
   - If either fails: log at info level, reset state to Idle, do NOT publish
     `DriveEndedEvent`. The drive is silently discarded.
4. If drive meets thresholds:
   - Publish `DriveEndedEvent` with calculated stats.
   - Reset state to Idle.

**Debounce cancellation (gear returns to D/R during debounce):**
If during the debounce period the handler receives a telemetry event with
gear = D or R for the same VIN:
1. Stop the debounce timer (`state.debounceTimer.Stop()`).
2. Set `state.debounceTimer = nil`.
3. The drive continues as if the P-shift never happened.
4. This handles the common case of stopping at a red light or stop sign.

### Route Point Accumulation (during StatusDriving)

On every telemetry event while driving:
1. Extract speed, location, heading from event fields.
2. If location is present, append a `RoutePoint` to `activeDrive.routePoints`.
3. Update running stats: `maxSpeed`, `speedSum`, `speedCount`.
4. Update `lastFSDMiles` if `FieldFSDMiles` is present.
5. Update `lastLocation`, `lastTimestamp`.
6. Publish `DriveUpdatedEvent` with the new route point.

**DriveUpdatedEvent throttling:** Publish on every telemetry tick that has a
location. Tesla sends telemetry every 1-5 seconds depending on configuration.
If this proves too chatty for downstream subscribers, throttling can be added
later (e.g., publish every Nth point). For now, every point is published --
the bus handles backpressure via its drop-oldest policy.

---

## 6. Debounce Concurrency Model

The debounce timer is the one place where concurrency gets tricky. The timer
callback runs on a goroutine managed by `time.AfterFunc`, while vehicleState
is normally accessed only from the bus handler goroutine.

**Approach: Channel-based signal, not direct mutation.**

When the debounce timer fires, the callback does NOT directly mutate
vehicleState. Instead, it publishes a `DriveEndedEvent` through the bus by
calling a method on the Detector that is safe for concurrent use.

Wait -- publishing `DriveEndedEvent` directly from the timer would skip
the stats calculation and micro-drive filtering that must happen in the
handler goroutine with access to `vehicleState`.

**Revised approach: Internal end-drive channel.**

```go
type Detector struct {
    // ... existing fields ...
    endDriveCh chan string // receives VIN when debounce timer fires
}
```

The debounce timer callback sends the VIN to `endDriveCh`. The Detector's
`Start` method launches a goroutine that reads from `endDriveCh` and
processes drive endings.

But this creates a second goroutine that accesses `vehicleState` concurrently
with the bus handler goroutine. We need synchronization.

**Final approach: Per-vehicle mutex for debounce coordination.**

Actually, the simplest correct approach is a per-vehicle mutex on
`vehicleState`:

```go
type vehicleState struct {
    mu sync.Mutex // guards all fields below
    // ... all fields from section 4 ...
}
```

The bus handler locks `state.mu` before reading/writing any fields.
The debounce timer callback also locks `state.mu` before ending the drive.
This is simple, correct, and the contention is negligible (the timer fires
at most once per drive, while telemetry arrives every 1-5 seconds).

**Why not a channel?** A channel would require a second goroutine per vehicle
or a shared dispatcher goroutine, adding complexity for a rare event (drive
end). A mutex is straightforward and well-understood.

**Lock ordering:** There is only one mutex per vehicle, so no deadlock risk.
The bus handler and timer callback never hold two locks simultaneously.

**Timer callback pseudocode:**

```
debounceCallback(vin string):
    state := d.states.Load(vin)
    state.mu.Lock()
    defer state.mu.Unlock()

    if state.status != StatusDriving:
        return  // race: drive was already ended or state was reset

    if state.debounceTimer == nil:
        return  // race: timer was cancelled between firing and lock acquisition

    endDrive(ctx, state, vin)
```

The `handleTelemetry` function similarly locks `state.mu` at the top:

```
handleTelemetry(ctx, event):
    vin := event.VIN
    state := loadOrStore(vin)
    state.mu.Lock()
    defer state.mu.Unlock()
    // ... process event ...
```

---

## 7. Stats Calculation (stats.go)

### Distance (Haversine)

Sum of haversine distances between consecutive route points.

```go
// haversine returns the distance in miles between two coordinates.
func haversine(lat1, lon1, lat2, lon2 float64) float64
```

The total drive distance is the sum of `haversine(points[i], points[i+1])`
for all consecutive pairs.

### Duration

```go
duration := drive.lastTimestamp.Sub(drive.startedAt)
```

Wall-clock time from first D/R event to debounce-confirmed P event.

### Average Speed

```go
avgSpeed := drive.speedSum / float64(drive.speedCount)
```

Arithmetic mean of all speed samples collected during the drive. If
`speedCount` is 0, `avgSpeed` is 0.

### Max Speed

Tracked incrementally: `drive.maxSpeed = max(drive.maxSpeed, currentSpeed)`
on each telemetry tick.

### Energy Delta

```go
energyUsed := drive.startEnergy - currentEnergy  // kWh consumed (positive)
```

If `startEnergy` was not captured (field absent at drive start), `energyUsed`
is 0. This is acceptable -- not all telemetry configurations include
`EnergyRemaining`.

### FSD Miles

Per the issue comment:

```go
fsdMiles := drive.lastFSDMiles - drive.startFSDMiles
```

If `startFSDMiles` is 0 (field was absent at drive start), `fsdMiles` is 0.

```go
fsdPercentage := 0.0
if distanceMiles > 0 {
    fsdPercentage = (fsdMiles / distanceMiles) * 100.0
}
```

`fsdMilesSinceReset` is a monotonically increasing counter from Tesla. The
delta between start and end gives FSD miles for this trip. If Tesla resets
the counter mid-drive (extremely unlikely), the delta could be negative --
clamp to 0 in that case.

### Charge Delta

```go
startCharge := drive.startCharge  // SOC percent at start
endCharge   := currentSOC         // SOC percent at end
```

These are integer percentages (0-100). Stored on the DriveRecord for display.

---

## 8. DriveStats Struct Update

The existing `DriveStats` in `internal/events/drive_events.go` is missing FSD
and charge fields. It needs to be extended:

```go
type DriveStats struct {
    Distance         float64       // miles (haversine sum)
    Duration         time.Duration // wall-clock drive time
    AvgSpeed         float64       // mph
    MaxSpeed         float64       // mph
    EnergyDelta      float64       // kWh consumed (positive = used)
    StartLocation    Location
    EndLocation      Location
    StartChargeLevel int           // SOC percent at drive start
    EndChargeLevel   int           // SOC percent at drive end
    FSDMiles         float64       // FSD miles this trip
    FSDPercentage    float64       // (FSDMiles / Distance) * 100
    RoutePoints      []RoutePoint  // full route for this drive
}
```

**Why include RoutePoints in DriveStats?** The `DriveEndedEvent` is the
signal for the store subscriber to persist the drive. The subscriber needs
the full route to write `routePoints` JSONB. Including it in `DriveStats`
keeps the event self-contained -- the subscriber does not need to accumulate
route points from `DriveUpdatedEvent` messages. This is a design trade-off:

- Pro: Store subscriber is stateless -- it receives everything it needs in
  `DriveEndedEvent`.
- Con: `DriveEndedEvent` can be large for long drives (thousands of points).

The bus uses buffered channels with drop-oldest policy, so a large event
does not block delivery. Memory pressure is bounded because `activeDrive`
holds the points in memory anyway -- we are just moving the slice into the
event, not copying it.

**Alternative considered:** Have the store subscriber accumulate points from
`DriveUpdatedEvent`. Rejected because it would make the store subscriber
stateful (needing its own per-drive accumulator), which duplicates the state
the detector already maintains.

---

## 9. Micro-Drive Filtering

Before publishing `DriveEndedEvent`, check:

```go
if duration < cfg.MinDuration || distance < cfg.MinDistanceMiles {
    d.logger.Info("discarding micro-drive",
        slog.String("vin", redactVIN(vin)),
        slog.String("drive_id", drive.id),
        slog.Duration("duration", duration),
        slog.Float64("distance_miles", distance),
    )
    d.metrics.IncMicroDriveDiscarded()
    resetToIdle(state)
    return
}
```

Default thresholds from `DrivesConfig`:
- `MinDuration`: 2 minutes
- `MinDistanceMiles`: 0.1 miles

These filter out:
- Parking lot maneuvering (short distance)
- Quick P-D-P cycling (short duration)
- Remote summon/move that covers trivial distance

The thresholds use OR logic (either condition discards) because a 30-second
drive that covers 5 miles is likely a data glitch, and a 10-minute drive that
covers 0 miles is just sitting in D at a charging station.

---

## 10. Event Publishing

### DriveStartedEvent

Published immediately when gear transitions to D/R from Idle.

```go
events.DriveStartedEvent{
    VIN:       vin,
    DriveID:   drive.id,
    Location:  drive.startLocation,
    StartedAt: drive.startedAt,
}
```

### DriveUpdatedEvent

Published on each telemetry tick during an active drive that includes a
location field.

```go
events.DriveUpdatedEvent{
    VIN:     vin,
    DriveID: drive.id,
    RoutePoint: events.RoutePoint{
        Latitude:  loc.Latitude,
        Longitude: loc.Longitude,
        Speed:     speed,
        Heading:   heading,
        Timestamp: event.CreatedAt,
    },
}
```

### DriveEndedEvent

Published after debounce timer fires AND drive passes micro-drive thresholds.

```go
events.DriveEndedEvent{
    VIN:     vin,
    DriveID: drive.id,
    Stats:   calculatedStats,
    EndedAt: drive.lastTimestamp,
}
```

Where `calculatedStats` is the `DriveStats` struct from section 8.

---

## 11. Metrics Interface

```go
// DetectorMetrics collects drive detection operational metrics.
// Defined in internal/drives/. Implemented by a Prometheus adapter or
// a noop for tests.
type DetectorMetrics interface {
    // IncDriveStarted increments the count of drives started.
    IncDriveStarted()

    // IncDriveEnded increments the count of drives ended normally.
    IncDriveEnded()

    // IncMicroDriveDiscarded increments the count of discarded micro-drives.
    IncMicroDriveDiscarded()

    // IncDebounceCancelled increments the count of debounce timers cancelled
    // (vehicle resumed driving before debounce elapsed).
    IncDebounceCancelled()

    // ObserveDriveDuration records the duration of a completed drive.
    ObserveDriveDuration(seconds float64)

    // ObserveDriveDistance records the distance of a completed drive.
    ObserveDriveDistance(miles float64)

    // SetActiveVehicles sets the gauge of vehicles currently in Driving state.
    SetActiveVehicles(count int)
}
```

A `NoopDetectorMetrics` struct is provided for tests.

---

## 12. No Store Interface Needed

**Decision:** The detector does NOT call any store methods.

The original store design (004-database-layer.md, section 5) defined
`VehicleReader` and `DriveWriter` interfaces in `internal/drives/` for the
detector to use. After further analysis, the detector does not need them:

- It does not need `VehicleReader.GetByVIN` because the VIN comes from the
  telemetry event. The detector does not need to look up the vehicle record
  to detect drives -- it works purely from telemetry field values.
- It does not need `DriveWriter` because it publishes events instead of
  writing directly. A separate store subscriber (future issue) handles
  persistence.

Those interfaces will still be defined when the store subscriber is
implemented. They will live in whatever package contains the subscriber
(likely `internal/store/` subscriber or a wiring package).

---

## 13. VIN Redaction Helper

The detector logs drive lifecycle events. Per CLAUDE.md security rules,
VINs must be redacted in production logs.

```go
// redactVIN returns the last 4 characters of a VIN, prefixed with "***".
// Used in log messages to comply with security policy.
func redactVIN(vin string) string {
    if len(vin) <= 4 {
        return vin
    }
    return "***" + vin[len(vin)-4:]
}
```

This helper lives in `internal/drives/` (not a shared util package) because
each package that logs VINs should have its own redaction. If multiple
packages need it, we can promote it to `internal/testutil/` or a new
`internal/vinutil/` package later.

---

## 14. Edge Cases

### Vehicle goes offline mid-drive

The drive remains in `StatusDriving` in memory. No telemetry events arrive,
so no state changes happen. When the vehicle reconnects and sends telemetry:

- If gear is still D/R: drive continues, route points accumulate.
- If gear is P: debounce starts as normal.

If the vehicle never reconnects, the drive state sits in memory indefinitely.
A future stale-drive reaper (not in this issue's scope) will handle cleanup
by checking `lastTimestamp` age.

### Speed = 0 in D gear

Still driving. Speed = 0 at a red light is normal. Only gear = P triggers
drive end consideration.

### Rapid P-D-P transitions

The 30s debounce handles this. If the vehicle shifts P-D within 30 seconds,
the debounce timer is cancelled and the drive continues. If the vehicle goes
P for 30+ seconds, the drive ends. A subsequent P-D-P sequence that lasts
< 2 minutes or < 0.1 miles is caught by micro-drive filtering.

### Location absent at drive start

Some telemetry batches may not include location (GPS not yet locked). The
drive starts with a zero-value Location. The first telemetry event that
includes location updates `lastLocation`. The `StartLocation` in
`DriveStats` will be `(0,0)` -- the store subscriber or a future enrichment
step can fill it in via reverse geocoding of the first route point.

Better: the state caches `lastLocation` from prior telemetry events
(section 4). If the drive-start event has no location but a previous event
did, use the cached location.

### Multiple vehicles simultaneously

Each vehicle has its own `vehicleState` in the `sync.Map`. The per-state
mutex ensures no cross-contamination. Two vehicles can be in different
states (one Idle, one Driving) with no interference.

### FSD counter reset mid-drive

If `lastFSDMiles < startFSDMiles`, clamp delta to 0:

```go
fsdMiles := drive.lastFSDMiles - drive.startFSDMiles
if fsdMiles < 0 {
    fsdMiles = 0
}
```

### Debounce timer fires after Stop() is called

`Stop()` cancels the parent context and sets a `stopped` flag. The timer
callback checks for this before processing. Additionally, `Publish` returns
`ErrBusClosed` if the bus is shut down, so even if the callback tries to
publish, the event is harmlessly dropped.

---

## 15. Dependency Graph

```
cmd/telemetry-server/
  main.go
    imports: internal/drives   (creates Detector, calls Start)
    imports: internal/events   (creates Bus, passes to Detector)
    imports: internal/config   (loads config, passes DrivesConfig)

internal/drives/
  imports: internal/events     (Bus interface, event types, Location, RoutePoint)
  imports: internal/config     (DrivesConfig)
  imports: internal/telemetry  (FieldName constants: FieldGear, FieldSpeed, etc.)
  imports: log/slog            (structured logging)
  imports: sync                (sync.Map, sync.Mutex)
  imports: time                (timers, durations)
  imports: math                (haversine calculation)
  does NOT import: internal/store (no DB dependency)
  does NOT import: internal/ws   (no WebSocket dependency)
```

### Who imports internal/drives/?

Only `cmd/telemetry-server/main.go`. The drives package exposes the
`Detector` struct and the `DetectorMetrics` interface. No other internal
package depends on it.

---

## 16. File-by-File Summary

| File | Contents | Est. Lines |
|------|----------|-----------|
| `detector.go` | `Detector` struct, `NewDetector`, `Start`, `Stop`, telemetry handler dispatch | ~90 |
| `state.go` | `vehicleState`, `activeDrive`, `DriveStatus` enum, `resetToIdle` | ~80 |
| `transitions.go` | `handleIdle`, `handleDriving`, `transitionToDriving`, `startDebounce`, `cancelDebounce`, `endDrive` | ~130 |
| `debounce.go` | `debounceCallback`, timer creation/cancellation helpers | ~50 |
| `stats.go` | `calculateStats`, `haversine`, `totalDistance`, `redactVIN` | ~80 |
| `metrics.go` | `DetectorMetrics` interface | ~30 |
| `noop_metrics.go` | `NoopDetectorMetrics` struct | ~20 |
| `errors.go` | Domain errors (if any -- currently none needed) | ~10 |

Total: ~490 lines of production code across 8 files. Well under 300 per file.

---

## 17. Required Changes to Existing Types

### events/drive_events.go -- DriveStats

Add FSD, charge, and route point fields:

```go
type DriveStats struct {
    Distance         float64       // miles
    Duration         time.Duration // wall-clock drive time
    AvgSpeed         float64       // mph
    MaxSpeed         float64       // mph
    EnergyDelta      float64       // kWh (positive = consumed)
    StartLocation    Location
    EndLocation      Location
    StartChargeLevel int           // NEW: SOC percent at drive start
    EndChargeLevel   int           // NEW: SOC percent at drive end
    FSDMiles         float64       // NEW: FSD miles this trip
    FSDPercentage    float64       // NEW: (FSDMiles / Distance) * 100
    RoutePoints      []RoutePoint  // NEW: full route for persistence
}
```

These additions are backward-compatible -- existing code that constructs
`DriveStats` will get zero values for the new fields.

### No other existing type changes needed

`VehicleTelemetryEvent`, `TelemetryValue`, `Location`, `RoutePoint`,
`DriveStartedEvent`, `DriveUpdatedEvent`, `DriveEndedEvent` are all
sufficient as-is.

---

## 18. Risks

1. **Memory growth on long drives.** A 4-hour highway drive at 1-second
   telemetry interval produces ~14,400 route points. Each `RoutePoint` is
   ~48 bytes, so ~690 KB per drive. This is fine for single-digit concurrent
   vehicles. If we support 100+ vehicles driving simultaneously, route point
   storage should be bounded or flushed periodically.
   **Mitigation:** Log a warning if route point count exceeds 10,000. Future
   optimization: flush points to the store subscriber via `DriveUpdatedEvent`
   batches and cap the in-memory slice.

2. **Debounce timer leak on process shutdown.** If the process exits while
   debounce timers are running, `time.AfterFunc` goroutines may reference
   freed state.
   **Mitigation:** `Stop()` cancels the context and stops all active timers.
   The timer callback checks context cancellation before proceeding.

3. **sync.Map never shrinks.** Vehicle state entries are never removed, even
   after the vehicle disconnects. For a server handling hundreds of unique
   VINs over weeks, entries accumulate.
   **Mitigation:** Future stale-drive reaper will also evict idle
   vehicleState entries that haven't received telemetry in 24+ hours.

4. **FieldGear value format.** We assume gear values are strings "P", "D",
   "R", "N". If Tesla sends different strings or uses an int enum, the
   transition logic will silently fail (no transitions detected).
   **Mitigation:** Log unknown gear values at warn level. The simulator
   (cmd/simulator) uses the same string format, so integration tests will
   catch format mismatches.

5. **FSD miles counter semantics.** We assume `SelfDrivingMilesSinceReset`
   is monotonically increasing within a drive. If Tesla resets it mid-drive,
   the delta will be negative (clamped to 0). If the field is simply absent
   from some telemetry batches, `lastFSDMiles` retains the last known value,
   which is correct.

---

## 19. Alternatives Considered

**Channel per vehicle instead of sync.Map + mutex:**
Each vehicle gets a dedicated goroutine and channel. Telemetry events are
routed to the vehicle's channel. Eliminates need for mutex because each
goroutine owns its state exclusively. Rejected because it creates a goroutine
per connected vehicle that sits idle most of the time (vehicles send telemetry
every 1-5 seconds, spending >99% of time waiting). The mutex approach is
simpler and has negligible contention.

**Debounce via event replay instead of timer:**
Instead of `time.AfterFunc`, record the P-shift timestamp and check elapsed
time on the next telemetry event. If 30 seconds have passed with gear still P,
end the drive. Rejected because this approach can delay drive-end detection
by up to one telemetry interval (1-5 seconds), AND if the vehicle goes
offline after shifting to P, the drive never ends (no subsequent telemetry
to trigger the check). The timer approach ends the drive exactly at the
debounce deadline regardless of telemetry cadence.

**Detector writes to store directly:**
Instead of publishing events, the detector calls `DriveWriter.Create()`,
`DriveWriter.Complete()`, etc. Rejected because it violates the event-driven
architecture: the store would become a synchronous dependency of the detector,
and adding new drive subscribers (e.g., push notifications, analytics)
would require modifying the detector. The event-based approach keeps the
detector focused on state machine logic only.

**Separate debounce goroutine with channel:**
Run a single goroutine that receives "check debounce" signals. Rejected
because it adds a goroutine + channel + select loop for something a
simple `time.AfterFunc` + mutex handles cleanly.
