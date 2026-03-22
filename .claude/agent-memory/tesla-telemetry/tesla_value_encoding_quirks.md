---
name: Tesla value encoding quirks
description: Tesla Fleet Telemetry proto Value oneof uses many type variants and the actual type depends on firmware version — decoder must handle all of them
type: reference
---

Tesla's vehicle_data.proto Value message is a oneof with 54+ type variants. Key quirks discovered during decoder implementation:

1. **Numeric fields as strings**: VehicleSpeed, Odometer, Soc, InsideTemp, OutsideTemp, EstBatteryRange, GpsHeading, and most numeric fields are sent as `string_value` ("65.2") on older firmware. Newer firmware may send them as `float_value`, `double_value`, `int_value`, or `long_value`. The decoder must try all variants.

2. **Location fields are special**: Location, OriginLocation, DestinationLocation use a dedicated `location_value` oneof variant with nested lat/lng (not string or float). This is different from every other field.

3. **Gear uses ShiftState enum**: The `Gear` field uses `shift_state_value` (a typed enum: P/R/N/D/Invalid/SNA). Older firmware may send it as `string_value`. Never sent as a numeric type.

4. **Two charge state enums**: `DetailedChargeState` (field 179) uses `detailed_charge_state_value` on firmware 2024.38+. Older firmware uses the deprecated `charging_value` (ChargingState enum). Both produce the same logical values (Disconnected, Charging, Complete, etc.).

5. **Invalid flag**: Any datum can have its value set to `invalid: true` instead of a real value. This means the vehicle explicitly reports the field as unavailable.

6. **Fields 179+ are always typed**: According to Tesla's proto comments, fields from DetailedChargeState (179) onward are "always returned typed" — meaning they use the proper enum/typed oneof variant, not string. Fields below 179 may use strings.

7. **SelfDrivingMilesSinceReset (259)** is only available on HW4 vehicles with firmware 2025.44.25.5+.

8. **Route/navigation fields (107-112, 163)** require firmware 2024.26+.

**How to apply:** When adding support for new Tesla fields, check the field number to determine whether it will arrive as a string or typed value. Always implement string fallback for fields below 179.
