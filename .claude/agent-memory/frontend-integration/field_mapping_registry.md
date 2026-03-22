---
name: Field mapping registry
description: Authoritative mapping from simulator ScenarioState fields through proto → internal name → client field name, with gap annotations
type: reference
---

## Full chain: ScenarioState → proto → internal → client field

| ScenarioState field | proto Field enum | Internal FieldName | field_mapping.go rename | Client field | Status |
|---|---|---|---|---|---|
| Speed | Field_VehicleSpeed | "speed" | (pass-through) | speed | OK |
| Latitude / Longitude | Field_Location | "location" | split to latitude + longitude | latitude, longitude | OK |
| Heading | Field_GpsHeading | "heading" | (pass-through) | heading | OK |
| GearPosition | Field_Gear | "gear" | "gearPosition" | gearPosition | OK — mapped |
| ChargeLevel | Field_Soc | "soc" | "chargeLevel" | chargeLevel | OK — mapped |
| EstimatedRange | Field_EstBatteryRange | "estimatedRange" | (pass-through) | estimatedRange | OK |
| InteriorTemp | Field_InsideTemp | "insideTemp" | "interiorTemp" | interiorTemp | OK — mapped |
| ExteriorTemp | Field_OutsideTemp | "outsideTemp" | "exteriorTemp" | exteriorTemp | OK — mapped |
| OdometerMiles | Field_Odometer | "odometer" | "odometerMiles" | odometerMiles | OK — mapped |

## Fields the frontend needs that the simulator never generates

| Vehicle interface field | Notes |
|---|---|
| status | Requires derived logic ("driving"/"parked") — never in telemetry stream |
| lastUpdated | Requires server to set this from payload.CreatedAt timestamp |
| destinationName | Field_DestinationName exists in fieldMap but simulator does not emit it |
| destinationAddress | Not in fieldMap; not in simulator |
| etaMinutes | Mapped from Field_MinutesToArrival (internal: "minutesToArrival") — simulator does not emit |
| tripDistanceMiles | Not in fieldMap or simulator |
| tripDistanceRemaining | Not in fieldMap or simulator |
| stops | Not in fieldMap or simulator — static data only |
| routePoints | Drive.routePoints — comes from the Drive record, not vehicle telemetry |
| fsdMilesToday | Mapped from Field_SelfDrivingMilesSinceReset (internal: "fsdMilesSinceReset") — simulator does not emit |
| locationName / locationAddress | Reverse geocoding, not in simulator |

## Gap: field_mapping.go missing "estimatedRange" pass-through note

The comment in field_mapping.go says `// speed, heading, estimatedRange, location (handled separately)` —
estimatedRange passes through unchanged from internal name "estimatedRange" which matches Vehicle.estimatedRange. Correct.

## Gap: "gear" → "gearPosition" rename verified

field_mapping.go maps `"gear"` → `"gearPosition"`. Vehicle.gearPosition expects type GearPosition ('P'|'R'|'N'|'D').
The converter (converters.go convertShiftState) returns a string like "P", "D", etc.
The frontend spreads fields directly onto Vehicle with `{ ...existing, ...update.fields }` — no runtime validation of GearPosition type.
The spread will work but TypeScript types are bypassed at runtime.
