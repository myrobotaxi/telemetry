---
name: Tesla GpsHeading encoding ambiguity
description: GpsHeading can arrive as stringValue, intValue, or floatValue — decoder handles all but stores differently depending on source type
type: reference
---

GpsHeading (field 23) has an encoding subtlety:

- Sent as `string_value` ("245"): decoder parses via `parseStringValue` -> `strconv.ParseFloat` -> stored as `FloatVal` (float64)
- Sent as `int_value` (245): decoder stores as `IntVal` (int64)
- Sent as `float_value` (245.0): decoder stores as `FloatVal` (float64)

This means downstream consumers must check BOTH `FloatVal` and `IntVal` for heading, or the simulator must be consistent about which encoding it uses.

**How to apply:** The simulator should use `string_value` encoding for heading to match real vehicle behavior (fields below enum 179 typically arrive as strings). Downstream consumers of heading should prefer checking `FloatVal` first since that's what string-encoded headings produce.
