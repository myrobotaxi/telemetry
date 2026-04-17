# Data Classification Contract

**Status:** Draft — v1
**Target artifact:** Classification reference table
**Owner:** `sdk-architect` agent + `security` agent

## Purpose

Labels every persisted field with a classification tier — **P0**, **P1**, or **P2** — driving logging redaction rules, encryption-at-rest boundaries, access-log requirements, and role-mask visibility. This contract is consulted by `contract-guard` on every PR that adds or modifies a persisted field.

## Classification tiers (per NFR-3.9)

- **P0 (Public)** — may appear in logs, no encryption required. Examples: VIN last-4, vehicle name, vehicle model.
- **P1 (Sensitive, encrypted at rest)** — AES-256-GCM column-level encryption. Never in logs. Examples: GPS coordinates, destination data, OAuth tokens.
- **P2 (Sensitive + access-logged)** — P1 requirements plus every read/write writes an access-log entry. Reserved for future fields (e.g., payment info, health data).

## Anchored requirements

- **NFR-3.9** — tier definitions
- **NFR-3.22** — TLS in transit for all connections
- **NFR-3.23** — AES-256-GCM column-level encryption for P1 fields (OAuth tokens, GPS coordinates, destination coordinates, route points)
- **NFR-3.24** — encryption key stored as Fly.io secret (`ENCRYPTION_KEY`)
- **NFR-3.25** — encryption transparent to SDK (server store layer only)
- **NFR-3.26** — key rotation strategy (separate contract doc)

---

## 1. Per-field classification tables

Every column in every persisted table is listed below. The **Tier** column is the authoritative classification. The **Encrypt** column indicates whether AES-256-GCM column-level encryption is required at rest. The **Log-safe** column indicates whether the value may appear in structured logs, error messages, or crash reports.

### 1.1 User table (Prisma-owned)

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `name` | `String?` | P1 | No | No | PII — user's display name |
| `email` | `String?` | P1 | No | No | PII — user's email address (FR-11.2) |
| `emailVerified` | `DateTime?` | P0 | No | Yes | Timestamp only, no PII |
| `image` | `String?` | P1 | No | No | User avatar URL — links to identity |
| `createdAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |
| `updatedAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |

> **Note:** The User table is Prisma-owned (NextAuth). The telemetry server reads `userId` as a foreign key but does not directly query the User table. Encryption of User columns is the responsibility of the Next.js app layer. Classifications here establish the contract for any future telemetry-server access.

### 1.2 Account table (Prisma-owned — NextAuth)

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `userId` | `String` | P0 | No | Yes | FK to User — opaque identifier |
| `type` | `String` | P0 | No | Yes | OAuth account type descriptor |
| `provider` | `String` | P0 | No | Yes | OAuth provider name (e.g., `tesla`) |
| `providerAccountId` | `String` | P0 | No | Yes | Opaque, provider-scoped ID (Tesla returns non-correlatable ID). Reclassify to P1 if a future provider exposes cross-service correlatable IDs |
| `refresh_token` | `Text?` | P1 | **Yes** | No | OAuth credential — NFR-3.23 |
| `access_token` | `Text?` | P1 | **Yes** | No | OAuth credential — NFR-3.23 |
| `expires_at` | `Int?` | P0 | No | Yes | Token expiry epoch — no secret material |
| `token_type` | `String?` | P0 | No | Yes | OAuth token type descriptor |
| `scope` | `String?` | P0 | No | Yes | OAuth scope string — public metadata |
| `id_token` | `Text?` | P1 | **Yes** | No | Contains user identity claims (JWT) |
| `session_state` | `String?` | P0 | No | Yes | OAuth session state parameter |

> **Note:** The telemetry server reads `access_token` and `refresh_token` via `AccountRepo.GetTeslaToken()` and writes refreshed tokens via `AccountRepo.UpdateTeslaToken()`. AES-256-GCM encryption for these columns is applied in the store layer per NFR-3.23/NFR-3.25.

### 1.3 Vehicle table

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `userId` | `String` | P0 | No | Yes | FK to User — opaque identifier |
| `teslaVehicleId` | `String?` | P0 | No | Yes | Tesla-assigned vehicle ID — opaque |
| `vin` | `String?` | P0 | No | **Last-4 only** | Publicly visible on vehicle exterior; P1 encryption would be overkill for a value stamped on the car. Risk is mitigated by mandatory `redactVIN()` redaction to `***XXXX` in all logs (see §2.1 VIN redaction rule) |
| `name` | `String` | P0 | No | Yes | User-assigned vehicle name |
| `model` | `String` | P0 | No | Yes | Vehicle model (e.g., "Model 3") |
| `year` | `Int` | P0 | No | Yes | Model year |
| `color` | `String` | P0 | No | Yes | Vehicle color |
| `licensePlate` | `String` | P1 | No | No | Can be used to look up registered owner — PII |
| `chargeLevel` | `Int` | P0 | No | Yes | Battery percentage — not identifying |
| `chargeState` | `String` (enum) | P0 | No | Yes | Charge state enum (`Disconnected`, `Charging`, `Complete`, …) — not identifying. Tesla proto field 2. Added by MYR-11 (v1 charge atomic group). |
| `estimatedRange` | `Int` | P0 | No | Yes | Range in miles — not identifying |
| `timeToFull` | `Float` | P0 | No | Yes | **Hours (decimal)** to full charge at current rate — not identifying. Tesla proto field 43 (`TimeToFullCharge`, double). Unit per `tesla-fleet-telemetry-sme` skill + legacy Tesla REST API; empirical verification tracked as `websocket-protocol.md` §10 DV-17. Added by MYR-11 (v1 charge atomic group). |
| `status` | `VehicleStatus` | P0 | No | Yes | Enum: driving/parked/charging/offline/in_service |
| `speed` | `Int` | P0 | No | Yes | Speed in mph — not identifying without GPS |
| `gearPosition` | `String?` | P0 | No | Yes | Gear state — not identifying |
| `heading` | `Int` | P0 | No | Yes | Compass heading — not identifying without GPS |
| `locationName` | `String` | P1 | No | No | Reverse-geocoded place name — reveals location |
| `locationAddress` | `String` | P1 | No | No | Street address — reveals location |
| `latitude` | `Float` | P1 | **Yes** | No | GPS coordinate — NFR-3.23 |
| `longitude` | `Float` | P1 | **Yes** | No | GPS coordinate — NFR-3.23 |
| `interiorTemp` | `Int` | P0 | No | Yes | Cabin temperature — not identifying |
| `exteriorTemp` | `Int` | P0 | No | Yes | Ambient temperature — not identifying |
| `odometerMiles` | `Int` | P0 | No | Yes | Odometer reading — not identifying |
| `fsdMilesSinceReset` | `Float` | P0 | No | Yes | FSD miles driven since last reset — not identifying |
| `virtualKeyPaired` | `Boolean` | P0 | No | Yes | Pairing status flag |
| `setupStatus` | `SetupStatus` | P0 | No | Yes | Enum: setup lifecycle state — **Prisma-owned**, not currently accessed by the telemetry server |
| `destinationName` | `String?` | P1 | No | No | Reveals travel intent/plans |
| `destinationAddress` | `String?` | P1 | No | No | Reveals travel intent/plans |
| `destinationLatitude` | `Float?` | P1 | **Yes** | No | GPS coordinate — NFR-3.23 |
| `destinationLongitude` | `Float?` | P1 | **Yes** | No | GPS coordinate — NFR-3.23 |
| `originLatitude` | `Float?` | P1 | **Yes** | No | GPS coordinate — NFR-3.23 |
| `originLongitude` | `Float?` | P1 | **Yes** | No | GPS coordinate — NFR-3.23 |
| `etaMinutes` | `Int?` | P0 | No | Yes | Time estimate — not identifying without destination |
| `tripDistanceMiles` | `Float?` | P0 | No | Yes | Distance value — not identifying |
| `tripDistanceRemaining` | `Float?` | P0 | No | Yes | Distance value — not identifying |
| `navRouteCoordinates` | `Json?` | P1 | **Yes** | No | Full route polyline — reveals travel patterns. NFR-3.23 |
| `lastUpdated` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |
| `createdAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |
| `updatedAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |

### 1.4 Drive table

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `vehicleId` | `String` | P0 | No | Yes | FK to Vehicle — opaque identifier |
| `date` | `String` | P0 | No | Yes | Date string — not identifying on its own |
| `startTime` | `String` | P0 | No | Yes | ISO 8601 timestamp — not identifying on its own |
| `endTime` | `String` | P0 | No | Yes | ISO 8601 timestamp — not identifying on its own |
| `startLocation` | `String` | P1 | No | No | Reverse-geocoded place name — reveals home/work |
| `startAddress` | `String` | P1 | No | No | Street address — reveals home/work |
| `endLocation` | `String` | P1 | No | No | Reverse-geocoded place name — reveals destinations |
| `endAddress` | `String` | P1 | No | No | Street address — reveals destinations |
| `distanceMiles` | `Float` | P0 | No | Yes | Aggregate stat — not identifying |
| `durationMinutes` | `Int` | P0 | No | Yes | Aggregate stat — not identifying |
| `avgSpeedMph` | `Float` | P0 | No | Yes | Aggregate stat — not identifying |
| `maxSpeedMph` | `Float` | P0 | No | Yes | Aggregate stat — not identifying |
| `energyUsedKwh` | `Float` | P0 | No | Yes | Aggregate stat — not identifying |
| `startChargeLevel` | `Int` | P0 | No | Yes | Battery percentage — not identifying |
| `endChargeLevel` | `Int` | P0 | No | Yes | Battery percentage — not identifying |
| `fsdMiles` | `Float` | P0 | No | Yes | FSD distance — not identifying |
| `fsdPercentage` | `Float` | P0 | No | Yes | FSD ratio — not identifying |
| `interventions` | `Int` | P0 | No | Yes | Count — not identifying |
| `routePoints` | `Json` | P1 | **Yes** | No | Full GPS trail (lat/lng/speed/heading/timestamp per point) — reveals travel patterns. NFR-3.23 |
| `createdAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |

### 1.5 Drive route point (embedded in `Drive.routePoints` JSONB)

Each element in the `routePoints` array is a `RoutePointRecord`:

| JSON key | Type | Tier | Encrypt | Log-safe | Rationale |
|----------|------|------|---------|----------|-----------|
| `lat` | `Float` | P1 | **Yes** (parent column) | No | GPS coordinate |
| `lng` | `Float` | P1 | **Yes** (parent column) | No | GPS coordinate |
| `speed` | `Float` | P0 | No (in P1 column) | No* | Not identifying, but encrypted with parent |
| `heading` | `Float` | P0 | No (in P1 column) | No* | Not identifying, but encrypted with parent |
| `timestamp` | `String` | P0 | No (in P1 column) | No* | Not identifying, but encrypted with parent |

> \*These sub-fields are P0 in isolation but are stored inside the P1 `routePoints` JSONB column, so they are encrypted at rest as a unit. They MUST NOT be logged because extracting them from context would require logging the entire route point including lat/lng.

### 1.6 Invite table (Prisma-owned)

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `vehicleId` | `String` | P0 | No | Yes | FK to Vehicle — opaque identifier |
| `senderId` | `String` | P0 | No | Yes | FK to User — opaque identifier |
| `label` | `String` | P0 | No | Yes | Display label for the invite |
| `email` | `String` | P1 | No | No | PII — invitee's email address |
| `status` | `InviteStatus` | P0 | No | Yes | Enum: pending/accepted |
| `permission` | `InvitePermission` | P0 | No | Yes | Enum: live/live_history |
| `sentDate` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |
| `acceptedDate` | `DateTime?` | P0 | No | Yes | Non-sensitive timestamp |
| `lastSeen` | `DateTime?` | P0 | No | Yes | Non-sensitive timestamp |
| `isOnline` | `Boolean` | P0 | No | Yes | Presence flag |
| `createdAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |
| `updatedAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |

> **Note:** The Invite table is Prisma-owned. The telemetry server does not currently read or write this table, but classifications are established for `contract-guard` enforcement when access is added (FR-5.x sharing features).

### 1.7 TripStop table (Prisma-owned)

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `vehicleId` | `String` | P0 | No | Yes | FK to Vehicle — opaque identifier |
| `name` | `String` | P1 | No | No | Place name — reveals location/travel intent |
| `address` | `String` | P1 | No | No | Street address — reveals location |
| `type` | `StopType` | P0 | No | Yes | Enum: charging/waypoint |

### 1.8 Settings table (Prisma-owned)

| Column | Type | Tier | Encrypt | Log-safe | Rationale |
|--------|------|------|---------|----------|-----------|
| `id` | `String` (cuid) | P0 | No | Yes | Opaque internal identifier |
| `userId` | `String` | P0 | No | Yes | FK to User — opaque identifier |
| `teslaLinked` | `Boolean` | P0 | No | Yes | Feature flag |
| `teslaVehicleName` | `String?` | P0 | No | Yes | Vehicle name — same as Vehicle.name |
| `virtualKeyPaired` | `Boolean` | P0 | No | Yes | Feature flag |
| `keyPairingDeferredAt` | `DateTime?` | P0 | No | Yes | Non-sensitive timestamp |
| `keyPairingReminderCount` | `Int` | P0 | No | Yes | Counter |
| `notifyDriveStarted` | `Boolean` | P0 | No | Yes | Preference flag |
| `notifyDriveCompleted` | `Boolean` | P0 | No | Yes | Preference flag |
| `notifyChargingComplete` | `Boolean` | P0 | No | Yes | Preference flag |
| `notifyViewerJoined` | `Boolean` | P0 | No | Yes | Preference flag |
| `createdAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |
| `updatedAt` | `DateTime` | P0 | No | Yes | Non-sensitive timestamp |

---

## 2. Redaction rules by tier

These rules apply to all structured log output (`slog`), error messages (`fmt.Errorf`), crash reports, and Prometheus metric labels.

### 2.1 P0 (Public) — may appear in logs

P0 values may be included in structured log fields, error messages, and metric labels with the following exceptions:

- **VIN**: Although classified P0 (VINs are publicly visible on vehicle exteriors), VINs MUST be redacted to `***XXXX` (last 4 characters) in all log output and error messages. This is a defense-in-depth measure because VINs link to location data (P1). Use `redactVIN()` — already implemented in `internal/store/errors.go`, `internal/drives/stats.go`, and `internal/telemetry/vin.go`.
- **User IDs, Vehicle IDs, Drive IDs**: These opaque identifiers are log-safe. Log the full value for debugging.

### 2.2 P1 (Sensitive) — never in logs

P1 values MUST NOT appear in:

- **Structured log fields** — never pass a P1 value to `slog.String()`, `slog.Float64()`, `slog.Any()`, or any slog attribute.
- **Error messages** — never include a P1 value in `fmt.Errorf()` format strings. Use opaque identifiers to correlate (e.g., drive ID, vehicle ID).
- **Crash reports / stack traces** — P1 values must not be stored in local variables that appear in panic dumps. Prefer passing by pointer to minimize stack exposure.
- **Prometheus metric labels** — never use GPS coordinates, addresses, tokens, or email addresses as metric label values.
- **HTTP response bodies for errors** — never echo P1 values back in error responses.

**Specific redaction rules for P1 fields:**

| P1 field category | Redaction behavior |
|-------------------|--------------------|
| GPS coordinates (`latitude`, `longitude`, `destinationLatitude`, etc.) | Omit entirely from logs. Never round/truncate as a substitute for redaction. |
| Route data (`navRouteCoordinates`, `routePoints`) | Omit entirely. Log only the point count: `slog.Int("route_points", len(points))` |
| OAuth tokens (`access_token`, `refresh_token`, `id_token`) | Omit entirely. Never log even partial token strings. |
| Email addresses (`User.email`, `Invite.email`) | Omit entirely. Use the associated user ID or invite ID instead. |
| Location names/addresses (`locationName`, `locationAddress`, `startLocation`, `startAddress`, `endLocation`, `endAddress`, `destinationName`, `destinationAddress`) | Omit entirely. Log the associated drive ID or vehicle ID instead. |
| License plate (`licensePlate`) | Omit entirely. |
| User identity (`User.name`, `User.image`) | Omit entirely. Use user ID instead. |

### 2.3 P2 (Access-logged) — P1 rules plus audit trail

No P2 fields exist in v1. When P2 fields are introduced:

- All P1 redaction rules apply.
- Every read or write of a P2 column MUST emit an audit log entry containing: timestamp, actor user ID, operation (read/write), column name, and target record ID.
- Audit log entries themselves are classified P0 (they contain only opaque IDs, not the actual P2 values).

---

## 3. Encryption scope mapping

Per NFR-3.23, AES-256-GCM column-level encryption is applied to the following columns. Encryption/decryption is performed in the server's store layer (NFR-3.25) — the SDK never sees ciphertext.

### 3.1 Encrypted columns

| Table | Column | Data type | Encrypted type | Notes |
|-------|--------|-----------|----------------|-------|
| `Account` | `access_token` | `Text` | `Text` (base64 ciphertext) | Tesla OAuth access token |
| `Account` | `refresh_token` | `Text` | `Text` (base64 ciphertext) | Tesla OAuth refresh token |
| `Account` | `id_token` | `Text` | `Text` (base64 ciphertext) | OpenID Connect ID token |
| `Vehicle` | `latitude` | `Float` | `Text` (base64 ciphertext) | Current GPS latitude |
| `Vehicle` | `longitude` | `Float` | `Text` (base64 ciphertext) | Current GPS longitude |
| `Vehicle` | `destinationLatitude` | `Float` | `Text` (base64 ciphertext) | Nav destination latitude |
| `Vehicle` | `destinationLongitude` | `Float` | `Text` (base64 ciphertext) | Nav destination longitude |
| `Vehicle` | `originLatitude` | `Float` | `Text` (base64 ciphertext) | Nav origin latitude |
| `Vehicle` | `originLongitude` | `Float` | `Text` (base64 ciphertext) | Nav origin longitude |
| `Vehicle` | `navRouteCoordinates` | `Json` | `Text` (base64 ciphertext) | Route polyline coordinate array |
| `Drive` | `routePoints` | `Json` | `Text` (base64 ciphertext) | Full GPS trail for drive playback |

### 3.2 P1 columns NOT encrypted (rationale)

These P1 columns are sensitive and must never appear in logs, but are NOT encrypted at rest because they are human-readable strings that do not carry coordinate-precision location data or credential material. They benefit from database-level encryption (Supabase encrypts at the disk level) but do not require application-level AES-256-GCM:

| Table | Column | Rationale for no app-level encryption |
|-------|--------|---------------------------------------|
| `User` | `name` | Prisma-owned; disk encryption sufficient for display names |
| `User` | `email` | Prisma-owned; disk encryption sufficient; not queried by telemetry server |
| `User` | `image` | URL to avatar; disk encryption sufficient |
| `Vehicle` | `licensePlate` | Prisma-owned; disk encryption sufficient; not queried by telemetry server |
| `Vehicle` | `locationName` | Derived from GPS (already encrypted); reverse-geocoded label |
| `Vehicle` | `locationAddress` | Derived from GPS (already encrypted); reverse-geocoded address |
| `Vehicle` | `destinationName` | User-entered or Tesla-provided name; not coordinate data |
| `Vehicle` | `destinationAddress` | User-entered or Tesla-provided address; not coordinate data |
| `Drive` | `startLocation` | Reverse-geocoded from encrypted coordinates |
| `Drive` | `startAddress` | Reverse-geocoded from encrypted coordinates |
| `Drive` | `endLocation` | Reverse-geocoded from encrypted coordinates |
| `Drive` | `endAddress` | Reverse-geocoded from encrypted coordinates |
| `Invite` | `email` | Prisma-owned; disk encryption sufficient |
| `TripStop` | `name` | Prisma-owned; disk encryption sufficient |
| `TripStop` | `address` | Prisma-owned; disk encryption sufficient |

> **Design decision:** Application-level encryption is reserved for columns where a database breach would expose precise geolocation trails or credential material. Human-readable location strings (place names, addresses) are protected by Supabase disk-level encryption and the P1 log-redaction rules. If threat modeling changes (e.g., multi-tenant deployment, regulatory requirements), these columns can be promoted to app-level encryption by adding them to the AES-256-GCM scope.

### 3.3 Encryption implementation contract

- **Algorithm:** AES-256-GCM (authenticated encryption with associated data)
- **Key source:** `ENCRYPTION_KEY` environment variable (Fly.io secret, NFR-3.24)
- **Nonce:** 12-byte random nonce prepended to each ciphertext
- **Encoding:** `base64(nonce || ciphertext || tag)` stored as `Text` in PostgreSQL
- **Transparency:** Encrypt on write, decrypt on read, entirely within the store layer (NFR-3.25). The SDK, WebSocket broadcaster, and REST API handlers operate on plaintext values.
- **Key rotation:** Documented in a separate contract doc per NFR-3.26

---

## 4. New-field classification checklist

When adding a new persisted field in any table (via Prisma schema change, migration, or new Go struct field), complete the following steps before the PR can merge:

### Step 1: Determine the tier

Answer these questions about the new field:

1. **Does this field contain or derive from GPS coordinates?** If yes → **P1**, encrypt at rest.
2. **Does this field contain credential material (tokens, passwords, API keys)?** If yes → **P1**, encrypt at rest.
3. **Does this field contain PII (name, email, phone, address, photo)?** If yes → **P1**, no app-level encryption required (disk encryption sufficient unless it contains precise coordinates).
4. **Does this field reveal travel patterns, location history, or intent?** If yes → **P1**.
5. **Does this field require audit-logged access for compliance?** If yes → **P2**.
6. **None of the above?** → **P0**.

### Step 2: Add the classification to this document

1. Open `docs/contracts/data-classification.md`.
2. Add a row to the appropriate table in Section 1.
3. Fill in: Column name, Type, Tier, Encrypt (Yes/No), Log-safe (Yes/No), Rationale.

### Step 3: If P1 with encryption — update the encryption scope

1. Add the column to Section 3.1 (Encrypted columns table).
2. Implement encrypt-on-write and decrypt-on-read in the store layer.
3. Verify the column type in PostgreSQL is `Text` (to hold base64 ciphertext).
4. Add a unit test that round-trips a value through encrypt/decrypt.

### Step 4: If P1 without encryption — document the rationale

1. Add the column to Section 3.2 (P1 not encrypted table) with a rationale.

### Step 5: Verify log safety

1. Search the codebase for any `slog.*` or `fmt.Errorf` call that references the new field.
2. If the field is P1, confirm it is never logged. Add a `// P1 — never log` comment at the field declaration.
3. If the field is P0 with the VIN exception, confirm `redactVIN()` is used.

### Step 6: Update related contract docs

1. If the field is in an atomic group, update `vehicle-state-schema.md` with the group membership.
2. If the field changes the data lifecycle, update `data-lifecycle.md`.
3. If the field is exposed over WebSocket or REST, update the corresponding protocol doc.

### Step 7: contract-guard validation

The `contract-guard` CI gate checks:

- Every column in `internal/store/types.go` structs has a corresponding row in this document.
- Every column with `Encrypt: Yes` has encrypt/decrypt calls in the store layer.
- No P1 field name appears in `slog.*()` calls outside of test files.

If `contract-guard` fails, the PR is blocked until classifications are added.

---

## 5. contract-guard rule description

The `contract-guard` agent/CI check enforces the following rules derived from this document:

### Rule CG-DC-1: Classification completeness

**Trigger:** Any PR that adds or modifies a column in `internal/store/types.go` (Go structs), `internal/store/queries.go` (SQL column lists), `internal/store/db_test.go` (test schema), or `prisma/schema.prisma` (in the partner repo).

**Check:** Every column name present in the Go struct or SQL query MUST have a corresponding row in Section 1 of this document. Missing classifications block merge.

**Scope note:** This rule validates against Go structs (the subset of columns the telemetry server uses). Prisma-only tables (User, Account, Invite, TripStop, Settings) are documented in this contract for completeness but are validated against the Prisma schema in the partner frontend repo — not by this rule. Columns in this doc that don't appear in Go structs are annotated "Prisma-owned" and are not enforced by the telemetry server's contract-guard.

**Fix:** Follow the new-field checklist (Section 4).

### Rule CG-DC-2: P1 log safety

**Trigger:** Any PR that modifies Go files in `internal/`.

**Check:** Scan all `slog.String()`, `slog.Float64()`, `slog.Any()`, `slog.Int()`, and `fmt.Errorf()` calls. If any argument references a field name classified P1 in this document (e.g., `latitude`, `longitude`, `access_token`, `email`, `destinationName`, `locationName`, `locationAddress`, `startLocation`, `startAddress`, `endLocation`, `endAddress`, `routePoints`, `navRouteCoordinates`), the PR is blocked.

**Exception:** Test files (`*_test.go`) are exempt from this check.

**Fix:** Remove the P1 value from the log/error statement. Use an opaque identifier (vehicle ID, drive ID, user ID) for correlation instead.

### Rule CG-DC-3: VIN redaction

**Trigger:** Any PR that logs a VIN value.

**Check:** Every `slog.String("vin", ...)` call MUST use `redactVIN(vin)` as the value, not the raw VIN. Raw VIN values in log statements block merge.

**Fix:** Wrap the VIN with `redactVIN()` before passing to the logger.

### Rule CG-DC-4: Encryption coverage

**Trigger:** Any PR that adds a new column to the "Encrypted columns" table in Section 3.1.

**Check:** The store layer MUST contain corresponding encrypt-on-write and decrypt-on-read calls for the new column. The PR must include both the encryption implementation and the classification update.

**Fix:** Implement AES-256-GCM encrypt/decrypt in the store layer and add a round-trip test.

---

## 6. Classification summary

### By tier

| Tier | Count | Description |
|------|-------|-------------|
| P0 | 85 | Public — timestamps, opaque IDs, aggregate stats, feature flags, enums |
| P1 | 26 | Sensitive — GPS coordinates, location names/addresses, OAuth tokens, PII, route data |
| P2 | 0 | Access-logged — reserved for future use |

> **Count audit trail.** The P0 count was bumped from 83 → 85 by [MYR-11](https://linear.app/myrobotaxi/issue/MYR-11) when it added `Vehicle.chargeState` (Tesla proto field 2, enum) and `Vehicle.timeToFull` (Tesla proto field 43, `Float` **hours (decimal)**) to the v1 charge atomic group. Both fields are P0 because they describe charge state, not identity or location. See §1.3 Vehicle table and `vehicle-state-schema.md` §2.2 for the wire contract. The `timeToFull` unit is tracked for empirical verification as `websocket-protocol.md` §10 DV-17 ([MYR-25](https://linear.app/myrobotaxi/issue/MYR-25)). Future count changes MUST add a one-line entry here so the total is auditable without `git blame`.

### P1 fields requiring AES-256-GCM encryption (11 columns)

1. `Account.access_token`
2. `Account.refresh_token`
3. `Account.id_token`
4. `Vehicle.latitude`
5. `Vehicle.longitude`
6. `Vehicle.destinationLatitude`
7. `Vehicle.destinationLongitude`
8. `Vehicle.originLatitude`
9. `Vehicle.originLongitude`
10. `Vehicle.navRouteCoordinates`
11. `Drive.routePoints`

### P1 fields with log-redaction only (no app-level encryption, 15 columns)

1. `User.name`
2. `User.email`
3. `User.image`
4. `Vehicle.licensePlate`
5. `Vehicle.locationName`
6. `Vehicle.locationAddress`
7. `Vehicle.destinationName`
8. `Vehicle.destinationAddress`
9. `Drive.startLocation`
10. `Drive.startAddress`
11. `Drive.endLocation`
12. `Drive.endAddress`
13. `Invite.email`
14. `TripStop.name`
15. `TripStop.address`
