---
name: Tesla shift state behavior
description: Real Tesla shift state transition patterns including SNA state for sleep, and the fact that N is skipped in normal P-to-D transitions
type: reference
---

Tesla ShiftState has a non-obvious "SNA" (Signal Not Available) state that differs from Park:

- **SNA** = car is asleep or drive inverter is in standby. Not the same as "P".
- **P** = car is awake and in Park.
- Normal drive start: SNA -> P (wake) -> D (drive). There is NO intermediate N state.
- Normal drive end: D -> P (park). N is never used in normal driving.
- Sleep after parking: P -> SNA (after 10-15 minutes idle).
- N is only used for towing or service situations.
- R is used for reverse (parallel parking, etc.), transitions are D -> R -> D.

**How to apply:** When building the simulator or drive detector, do not expect N between P and D. The SNA state indicates a sleeping vehicle and should be treated differently from P in drive detection logic.
