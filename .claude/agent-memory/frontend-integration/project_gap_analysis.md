---
name: Gap analysis — simulator to frontend real-time updates
description: Full gap analysis identifying every broken link between the vehicle simulator, telemetry server, and MyRoboTaxi frontend; produced 2026-03-19
type: project
---

## Summary of findings (2026-03-19)

Six symptoms reported: map marker not moving, no route line, ETA stuck, battery stuck,
odometer/temps not updating, vehicle status from seed data only.

Root causes identified:

1. **Message envelope mismatch (CRITICAL)** — the server wraps payload in a nested JSON object but
   the frontend may be reading a double-nested structure depending on how json.RawMessage serializes.
   Needs verification with a live message dump.

2. **`vehicle.status` never updated by telemetry** — the frontend derives isDriving from
   `vehicle.status === 'driving'`, but the simulator never sends a `status` field. The server has
   no logic to derive status from gear/speed and inject it into the fields map.

3. **Route line never populated** — `routePoints` comes from `Drive.routePoints`, not from
   vehicle telemetry. HomeScreen passes `currentDrive?.routePoints` to VehicleMap, but the drive
   record is loaded once server-side and never updated in real time from WebSocket messages.
   The `drive_started` / `drive_ended` WS messages exist but the frontend does not handle them —
   `useVehicleStream` only processes `vehicle_update` type messages.

4. **ETA stuck** — `etaMinutes` maps from internal field "minutesToArrival" → client field
   "minutesToArrival" (no rename in field_mapping.go). But the Vehicle interface uses `etaMinutes`,
   not `minutesToArrival`. The field name is WRONG — field_mapping.go does not rename
   "minutesToArrival" → "etaMinutes", so the update lands on a non-existent Vehicle key.

5. **Battery/chargeLevel** — the field chain is correct (soc → chargeLevel). If battery is
   truly stuck, the most likely cause is that the WS update is not reaching the frontend at all
   (see item 1 or 2 re: vehicleId matching).

6. **Odometer and temps** — field chains verified as correct. If they are not updating, same
   root cause as battery — the update isn't being applied, likely a vehicleId mismatch or
   the `existing` check in handleUpdate returning early.

7. **vehicleId matching** — `handleUpdate` in use-vehicle-stream.ts does:
   `const existing = prev.get(update.vehicleId); if (!existing) return prev;`
   If the vehicleId in the WS message does not exactly match the id in the initialVehicles array,
   ALL field updates are silently discarded. This is the most likely cause of nothing updating.

**Why:** Analyzed 2026-03-19 by reading all 8 files end-to-end.

**How to apply:** When debugging or fixing this integration, address in this order:
1. Confirm vehicleId matching (log update.vehicleId vs vehicle.id from seed data)
2. Fix the "minutesToArrival" → "etaMinutes" rename in field_mapping.go
3. Add "status" derivation in the broadcaster (gear D + speed > 0 → "driving", else "parked")
4. Handle drive_started / drive_ended in useVehicleStream to update routePoints
