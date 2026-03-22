# Simulator Telemetry Reference

Tesla-specific field encoding, realistic value ranges, and timing guidance for the vehicle simulator. This document is the authoritative reference for issue #22.

## 1. Protobuf Field Mapping

The simulator must construct `tpb.Payload` messages with `tpb.Datum` entries. Each datum has a `Key` (the `tpb.Field` enum) and a `Value` (a `tpb.Value` oneof).

### Field Enum Values and Value Types

The simulator should use the **typed proto values** (not string encoding) since it is acting as a modern-firmware vehicle. This is what a real 2024.38+ vehicle sends.

| Our Field Name | Proto Enum | Enum Int | Value Constructor | Decoded As |
|---|---|---|---|---|
| `speed` | `Field_VehicleSpeed` | 4 | `Value_StringValue` or `Value_FloatValue` | `FloatVal` |
| `location` | `Field_Location` | 21 | `Value_LocationValue{LocationValue{lat, lng}}` | `LocationVal` |
| `heading` | `Field_GpsHeading` | 23 | `Value_StringValue` or `Value_IntValue` | `FloatVal` (parsed) or `IntVal` |
| `gear` | `Field_Gear` | 10 | `Value_ShiftStateValue` | `StringVal` ("P"/"R"/"N"/"D") |
| `soc` | `Field_Soc` | 8 | `Value_StringValue` or `Value_FloatValue` | `FloatVal` |
| `estimatedRange` | `Field_EstBatteryRange` | 40 | `Value_StringValue` or `Value_FloatValue` | `FloatVal` |
| `chargeState` | `Field_DetailedChargeState` | 179 | `Value_DetailedChargeStateValue` | `StringVal` |
| `odometer` | `Field_Odometer` | 5 | `Value_StringValue` or `Value_DoubleValue` | `FloatVal` |
| `insideTemp` | `Field_InsideTemp` | 85 | `Value_StringValue` or `Value_FloatValue` | `FloatVal` |
| `outsideTemp` | `Field_OutsideTemp` | 86 | `Value_StringValue` or `Value_FloatValue` | `FloatVal` |
| `destinationName` | `Field_DestinationName` | 163 | `Value_StringValue` | `StringVal` |
| `routeLine` | `Field_RouteLine` | 108 | `Value_StringValue` | `StringVal` |
| `batteryLevel` | `Field_BatteryLevel` | 42 | `Value_StringValue` or `Value_IntValue` | `FloatVal` or `IntVal` |
| `energyRemaining` | `Field_EnergyRemaining` | 158 | `Value_StringValue` or `Value_FloatValue` | `FloatVal` |
| `packVoltage` | `Field_PackVoltage` | 6 | `Value_StringValue` or `Value_FloatValue` | `FloatVal` |
| `packCurrent` | `Field_PackCurrent` | 7 | `Value_StringValue` or `Value_FloatValue` | `FloatVal` |
| `vehicleName` | `Field_VehicleName` | 64 | `Value_StringValue` | `StringVal` |
| `carType` | `Field_CarType` | 113 | `Value_CarTypeValue` | `StringVal` |
| `version` | `Field_Version` | 68 | `Value_StringValue` | `StringVal` |
| `locked` | `Field_Locked` | 59 | `Value_BooleanValue` | `BoolVal` |
| `sentryMode` | `Field_SentryMode` | 65 | `Value_SentryModeStateValue` | `StringVal` |
| `originLocation` | `Field_OriginLocation` | 111 | `Value_LocationValue` | `LocationVal` |
| `destinationLocation` | `Field_DestinationLocation` | 112 | `Value_LocationValue` | `LocationVal` |
| `milesToArrival` | `Field_MilesToArrival` | 109 | `Value_StringValue` or `Value_FloatValue` | `FloatVal` |
| `minutesToArrival` | `Field_MinutesToArrival` | 110 | `Value_StringValue` or `Value_FloatValue` | `FloatVal` |
| `lateralAcceleration` | `Field_LateralAcceleration` | 98 | `Value_StringValue` or `Value_FloatValue` | `FloatVal` |
| `longitudinalAcceleration` | `Field_LongitudinalAcceleration` | 99 | `Value_StringValue` or `Value_FloatValue` | `FloatVal` |

### Encoding Recommendation for the Simulator

Use `Value_StringValue` for numeric fields below enum value 179 (this is what most real Teslas send). Use typed enum values for DetailedChargeState (179), CarType (113 uses `Value_CarTypeValue`), SentryMode (65 uses `Value_SentryModeStateValue`), Gear (10 uses `Value_ShiftStateValue`), and Locked (59 uses `Value_BooleanValue`).

The decoder handles both, but for maximum realism the simulator should match what real vehicles do: **string values for numeric fields, typed enums for enum fields**.

### Payload Structure

```go
payload := &tpb.Payload{
    Vin:       "5YJ3E7EB2NF000001",
    CreatedAt: timestamppb.Now(),
    Data: []*tpb.Datum{
        {Key: tpb.Field_VehicleSpeed, Value: &tpb.Value{Value: &tpb.Value_StringValue{StringValue: "65.2"}}},
        {Key: tpb.Field_Location, Value: &tpb.Value{Value: &tpb.Value_LocationValue{
            LocationValue: &tpb.LocationValue{Latitude: 33.0903, Longitude: -96.8237},
        }}},
        {Key: tpb.Field_Gear, Value: &tpb.Value{Value: &tpb.Value_ShiftStateValue{ShiftStateValue: tpb.ShiftState_ShiftStateD}}},
        // ...
    },
}
raw, _ := proto.Marshal(payload)
// Send raw bytes over WebSocket as binary message
```

### Important: The Simulator Sends Raw Protobuf, Not JSON

The receiver (`internal/telemetry/receiver.go`) reads binary WebSocket messages and calls `proto.Unmarshal`. The simulator must send `proto.Marshal`ed `tpb.Payload` bytes, not JSON.

## 2. Realistic Value Ranges

### Speed (mph)

Tesla reports speed in mph as a float. The decoder normalizes all variants to `float64`.

| Scenario | Range | Typical | Notes |
|---|---|---|---|
| Parked | 0.0 | 0.0 | Gear = P |
| City driving | 0-45 | 25-35 | Frequent stops, acceleration/deceleration |
| Suburban | 30-55 | 40-50 | Less stop-and-go |
| Highway cruising | 60-80 | 68-72 | Autopilot typically holds 70-75 |
| Highway acceleration | 0-70 | ramp | 0-60 in ~4.5s for Model Y LR, ~3.5s for Performance |
| Deceleration (regen) | gradual | -2 to -5 mph/s | Regenerative braking is smoother than friction |
| Hard braking | sharp | -10 to -20 mph/s | Emergency stop |

**Acceleration curves:** Tesla acceleration is not linear. Use an ease-in curve:
- 0-30 mph: aggressive (~2.5s for Performance, ~3.5s for LR)
- 30-60 mph: still strong but tapering
- 60-80 mph: much more gradual

A simple approach: `speed += acceleration * dt * (1 - speed/maxSpeed)` where `acceleration` is ~15 mph/s for Performance, ~10 mph/s for LR.

**Deceleration:** Regen braking decelerates at ~3-5 mph/s. Model it as `speed -= decel * dt` with decel = 4.0.

### Battery / State of Charge (SoC)

| Field | Range | Unit | Notes |
|---|---|---|---|
| Soc | 0-100 | percent | Typically 20-90 in normal use |
| BatteryLevel | 0-100 | percent | Same as Soc, sometimes 1% different |
| EstBatteryRange | 0-330 | miles | Model Y LR: ~310 at 100%, ~280 at 90% |
| IdealBatteryRange | 0-330 | miles | Slightly higher than EstBatteryRange |
| RatedRange | 0-330 | miles | EPA rated, higher than actual |
| EnergyRemaining | 0-75 | kWh | Model Y LR: ~75 kWh pack |
| PackVoltage | 300-410 | volts | ~340V at 20%, ~400V at 100% |
| PackCurrent | -500 to 500 | amps | Negative = discharging, positive = charging |

**Drain rates:**
- Highway 70mph: ~280 Wh/mi -> at 75 kWh pack, ~267 mi range -> roughly 0.37% SoC per mile, or **1% per 2.7 miles**
- City 30mph: ~230 Wh/mi -> ~326 mi range -> roughly 0.31% SoC per mile, or **1% per 3.3 miles**
- Climate control adds ~1-3 kW load (AC in Texas heat), increases drain ~10-15%
- In very hot weather (100F+), expect ~300 Wh/mi highway

**For the simulator:** At a 2-second telemetry interval and 70mph highway speed, the car travels ~0.039 miles per tick. SoC drops ~0.014% per tick. Apply this continuously: `soc -= 0.014` every 2 seconds. Accumulate fractional drain rather than rounding.

**EstBatteryRange** should be roughly `soc * 2.8` (highway) or `soc * 3.3` (city). It fluctuates based on recent driving efficiency.

### Temperature (Celsius)

Tesla reports temperatures in Celsius, not Fahrenheit.

| Field | Range | Typical | Notes |
|---|---|---|---|
| InsideTemp | 15-50 | 20-23 when climate on | Celsius |
| OutsideTemp | -20 to 50 | varies | Dallas TX summer: 30-40C (86-104F) |

**Dallas, TX temperature scenarios:**
- Summer day: OutsideTemp 33-38C (91-100F), InsideTemp 21-22C with AC
- Spring/fall: OutsideTemp 18-28C (64-82F), InsideTemp 21-22C
- Winter: OutsideTemp 2-12C (36-54F), InsideTemp 22-23C with heat

**During a drive:** InsideTemp may start high (cabin soak if parked in sun: 50-60C), then drop to setpoint within 5-10 minutes. Model as exponential decay toward 22C.

### Odometer (miles)

A reasonable starting odometer for a 2023-2024 Model Y used as a robotaxi: **15,000 to 45,000 miles**. Use something like 28,456.7 as a default. Odometer increments in real-time as the vehicle moves.

Increment: `odometer += speed_mph * (interval_seconds / 3600.0)`

### Heading (degrees)

Tesla's GpsHeading is 0-359, where 0 = North, 90 = East, 180 = South, 270 = West.

**Straight driving:** Heading changes by 0-2 degrees per tick (road curvature, GPS jitter).

**Turns:**
- Normal turn: heading changes 80-100 degrees over 4-8 seconds (10-25 degrees per second)
- Highway curve: heading changes 5-30 degrees over 5-15 seconds
- U-turn: heading changes ~180 degrees over 5-10 seconds

**Calculating heading from GPS points:**
```
bearing = atan2(sin(dLon) * cos(lat2), cos(lat1) * sin(lat2) - sin(lat1) * cos(lat2) * cos(dLon))
heading = (bearing * 180/pi + 360) % 360
```

### Lateral and Longitudinal Acceleration (m/s^2)

| Scenario | Lateral | Longitudinal | Notes |
|---|---|---|---|
| Straight highway | -0.1 to 0.1 | -0.2 to 0.2 | Near zero |
| Lane change | -1.5 to 1.5 | ~0 | Briefly spikes |
| Normal turn | -2.0 to 2.0 | -0.5 to 0.5 | |
| Hard acceleration | ~0 | 3.0-5.0 | Model Y Performance launch |
| Hard braking | ~0 | -6.0 to -8.0 | Emergency |
| Regen braking | ~0 | -1.0 to -2.5 | Normal decel |

### Gear / Shift State

The decoder converts `ShiftState` enum to single-letter strings: "P", "R", "N", "D", "Invalid", "SNA".

**SNA (Signal Not Available):** The vehicle sends `ShiftState_ShiftStateSNA` when the car is asleep or the drive inverter is in standby. This is the state you see when the car is parked and has gone to sleep. It is different from "P" -- "P" means the car is awake and in Park.

**Transition patterns for a typical drive:**
1. Vehicle wakes up: SNA -> P (may take a few seconds)
2. Driver starts drive: P -> D (single transition, immediate)
3. During drive: stays D
4. Brief reverse: D -> R -> D (e.g., parallel parking)
5. Arrive and park: D -> P
6. Vehicle sleeps (after 10-15 min idle): P -> SNA

**Important:** There is no N between P and D. Tesla skips N in normal driving. N is only used when towing or in specific service situations.

### Charge State

The simulator should use `DetailedChargeStateValue` enum (field 179). Valid values:

| Enum | String | When |
|---|---|---|
| `DetailedChargeStateDisconnected` | "Disconnected" | Normal driving, not plugged in |
| `DetailedChargeStateCharging` | "Charging" | Actively charging |
| `DetailedChargeStateComplete` | "Complete" | Reached charge limit |
| `DetailedChargeStateStopped` | "Stopped" | User stopped charging before limit |
| `DetailedChargeStateNoPower` | "NoPower" | Plugged in but no power |
| `DetailedChargeStateStarting` | "Starting" | Briefly, while negotiating with charger |

**During a drive scenario:** Always "Disconnected". Only changes when the car is parked and plugged in.

### Locked / Sentry Mode

- `Locked`: true when doors are locked (always true during autonomous/robotaxi operation)
- `SentryMode`: use `SentryModeState_SentryModeStateOff` during drives, `SentryModeState_SentryModeStateArmed` when parked

### Navigation Fields

| Field | Example | When Present |
|---|---|---|
| `DestinationName` | "Dallas/Fort Worth International Airport" | When navigation is active |
| `RouteLine` | encoded polyline string | When navigation is active |
| `OriginLocation` | `{lat: 33.0903, lng: -96.8237}` | When navigation is active |
| `DestinationLocation` | `{lat: 32.8998, lng: -97.0403}` | When navigation is active |
| `MilesToArrival` | 24.7 | Decreases during drive |
| `MinutesToArrival` | 28.0 | Decreases during drive |

These fields are empty/absent when no navigation route is active.

## 3. GPS Route Realism (Dallas, TX)

### Starting Coordinates

Good starting points in the Dallas area:

| Location | Latitude | Longitude | Description |
|---|---|---|---|
| Frisco (home base) | 33.0903 | -96.8237 | Northern suburb, near Toyota HQ |
| Downtown Dallas | 32.7767 | -96.7970 | Urban core |
| DFW Airport | 32.8998 | -97.0403 | Major destination |
| Plano | 33.0198 | -96.6989 | Tech corridor |
| Richardson | 32.9483 | -96.7299 | Telecom corridor |
| McKinney | 33.1972 | -96.6150 | Northern suburb |

### Highway Route: Frisco to DFW Airport

A realistic 30-minute highway drive on the Dallas North Tollway and SH-121:

**Waypoints (approximate):**
1. Start: Frisco - (33.0903, -96.8237) heading ~200 (SSW)
2. Dallas North Tollway south: (33.0500, -96.8200) heading ~195
3. Merge to SH-121: (33.0200, -96.8400) heading ~220 (SW)
4. SH-121 west: (32.9800, -96.8800) heading ~240
5. Approach DFW: (32.9300, -96.9500) heading ~230
6. DFW Terminal: (32.8998, -97.0403) heading ~210

### GPS Coordinate Change Rates

At Dallas latitudes (~33 N):
- 1 degree latitude = ~69 miles = ~111 km
- 1 degree longitude = ~58 miles = ~93 km (cos(33) factor)

**Per-second movement at various speeds:**

| Speed (mph) | Lat change/sec | Lon change/sec | Notes |
|---|---|---|---|
| 30 | ~0.000121 | ~0.000144 | City speed (due south) |
| 45 | ~0.000181 | ~0.000216 | Suburban |
| 70 | ~0.000282 | ~0.000336 | Highway |
| 80 | ~0.000322 | ~0.000384 | Fast highway |

These are maximums when traveling purely in that axis. For diagonal travel, decompose by heading:
```
delta_lat = speed_deg_per_sec * cos(heading_radians)
delta_lon = speed_deg_per_sec * sin(heading_radians) / cos(latitude_radians)
```

Where `speed_deg_per_sec = speed_mph / 69.0 / 3600.0` (converting mph to degrees-lat per second).

### GPS Jitter

Real GPS has noise of approximately +/-3-5 meters, which is roughly +/-0.00003 degrees. Add small random noise to lat/lng each tick for realism. Do NOT make it too large or the heading calculation will be noisy.

## 4. Telemetry Timing

### Emission Interval

Fleet telemetry config sets the interval. Typical configurations:
- **1 second** — high resolution, used for active drive monitoring
- **2 seconds** — good balance of resolution and bandwidth (recommended for simulator default)
- **5 seconds** — low-bandwidth mode, acceptable for charge monitoring
- **30 seconds** — sleep/idle mode

### Batching Behavior

The real vehicle batches telemetry in ~500ms windows. A single WebSocket message contains one `Payload` with multiple `Datum` entries (one per field). The simulator should send **one Payload per tick** containing ALL fields.

Not every field changes every tick. Tesla's real emission rule: a field is emitted only when BOTH conditions are met: (1) the configured interval has elapsed AND (2) the value has changed since last emission. For simplicity, the simulator can send all fields every tick, but for more realism it could skip unchanged fields.

### Sleep/Wake Transitions

**Going to sleep (after parking):**
1. Vehicle parks (Gear = P), charge state = Disconnected
2. 5-15 minutes of idle: vehicle sends telemetry at normal rate with speed=0, gear=P
3. Vehicle enters sleep: telemetry stops entirely (WebSocket connection may stay open or close)
4. If connection stays open: vehicle sends nothing, or very occasional heartbeats
5. If connection closes: server sees disconnect event

**Waking up:**
1. App interaction or scheduled departure triggers wake
2. WebSocket connection re-established (or first message after long silence)
3. First payload: Gear=SNA (briefly), then Gear=P within 1-2 seconds
4. Telemetry resumes at normal rate

**For the simulator:** Model sleep as simply stopping telemetry sends. Model wake as resuming sends, with the first few messages having Gear=P and speed=0.

### Vehicle Startup Sequence (beginning of a drive)

Realistic message sequence at 2-second intervals:

```
T=0s:  Gear=P, Speed=0, Soc=85.0, ChargeState=Disconnected, Locked=true
T=2s:  Gear=P, Speed=0, Soc=85.0, Locked=false  (driver unlocks)
T=4s:  Gear=D, Speed=0, Soc=85.0  (shifts to drive)
T=6s:  Gear=D, Speed=5.2, Soc=85.0  (starts moving)
T=8s:  Gear=D, Speed=18.4, Soc=85.0  (accelerating out of parking)
T=10s: Gear=D, Speed=32.1, Soc=84.9  (entering road)
...
```

### Vehicle Stop Sequence (end of a drive)

```
T=0s:  Gear=D, Speed=35.2, Soc=72.1
T=2s:  Gear=D, Speed=22.8, Soc=72.1  (decelerating)
T=4s:  Gear=D, Speed=8.3, Soc=72.1   (approaching parking spot)
T=6s:  Gear=D, Speed=1.2, Soc=72.1   (creeping)
T=8s:  Gear=D, Speed=0.0, Soc=72.1   (stopped)
T=10s: Gear=P, Speed=0.0, Soc=72.1   (shifted to park)
T=12s: Gear=P, Speed=0.0, Soc=72.1, Locked=true  (doors lock)
```

## 5. Simulator Scenarios

### Scenario: Highway Drive (Frisco to DFW)

- Duration: ~25-30 minutes
- Start: Frisco (33.0903, -96.8237), Heading 200
- End: DFW Airport (32.8998, -97.0403)
- SoC start: 85%, SoC end: ~76%
- Odometer start: 28456.7, adds ~30 miles
- Speed profile: 0 -> 40 (residential) -> 70 (tollway) -> 65 (exit) -> 0
- ChargeState: Disconnected throughout
- InsideTemp: starts at 35C (hot cabin), drops to 22C over 5 min
- OutsideTemp: 34C (Dallas summer)

### Scenario: City Drive (Downtown errand)

- Duration: ~15 minutes
- Start: Downtown Dallas (32.7767, -96.7970)
- End: Deep Ellum (32.7838, -96.7829)
- SoC start: 65%, SoC end: ~63%
- Speed profile: lots of 0-30-0 cycles (traffic lights)
- Heading: changes frequently (turns)

### Scenario: Charging Session

- Duration: ~45 minutes (Supercharger)
- Location: fixed (no movement)
- Gear: P throughout
- SoC: 20% -> 80% (Supercharger curve: fast to 50%, slower to 80%)
- ChargeState: Starting -> Charging -> Complete
- PackCurrent: +200A initially, tapering to +50A near 80%
- PackVoltage: 340V -> 390V as charge builds
- Speed: 0 throughout
- InsideTemp: may fluctuate (preconditioning cabin)

### Scenario: Sleep/Wake

- Phase 1: Parked, idle, Gear=P, telemetry at 2s intervals for 2 min
- Phase 2: Sleep - stop sending telemetry for configurable duration
- Phase 3: Wake - resume telemetry, brief Gear=P, then Gear=D for a short drive

## 6. VIN Format

Tesla VINs follow this pattern: `5YJ3E7EB2NF000001`

For the simulator, use VINs that are clearly fake:
- `5YJ3E7EB2NF000001` through `5YJ3E7EB2NF000099` for Model Y
- The "N" in position 10 indicates 2022 model year
- The "3E7" indicates Model Y Long Range AWD

The receiver extracts VINs from the mTLS client certificate CN field. Since the simulator bypasses mTLS, it puts the VIN directly in the Payload's `vin` field and the receiver uses that.

## 7. Key Implementation Notes

1. **Wire format is protobuf binary, not JSON.** Use `proto.Marshal(payload)` and send as WebSocket binary message type.

2. **The receiver rate-limits at 10 messages/second per VIN** (see `defaultMaxMessagesPerSec` in receiver.go). A 2-second interval is well within this.

3. **Max WebSocket message size is 1 MiB** (see `maxMessageSize` in receiver.go). A typical payload with 15-20 fields is under 1 KB.

4. **The `is_resend` field on Payload** should be `false` for normal simulator messages. Set to `true` only when simulating reliable_ack retransmission.

5. **GpsHeading sent as string parses to float, not int.** When the decoder receives `stringVal("245")`, it parses to `float64(245.0)` via `parseStringValue`, not to an int. When sent as `intVal(245)`, the decoder stores it as `IntVal`. The simulator should be consistent -- pick one encoding and stick with it. Recommend `stringVal` for maximum realism.

6. **Multiple payloads per second are fine** but will be rate-limited above 10/sec. For a 2-second interval, the simulator sends 0.5 msg/sec, which is well under the limit.
