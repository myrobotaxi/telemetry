# Design: Database Connection Layer (Issue #4)

**Date:** 2026-03-17
**Status:** Proposed
**Author:** Architect agent

---

## 1. Package Structure

```
internal/store/
  db.go                 -- DB type: pgxpool wrapper, health check, graceful close
  vehicle_repo.go       -- VehicleRepo struct + methods
  drive_repo.go         -- DriveRepo struct + methods
  queries.go            -- SQL query constants (all parameterized)
  errors.go             -- Domain error variables
  metrics.go            -- StoreMetrics interface
  noop_metrics.go       -- No-op metrics implementation for tests
  migrations/
    000001_telemetry_points.up.sql
    000001_telemetry_points.down.sql
```

All files should stay under 300 lines. SQL constants are extracted to `queries.go`
to keep the repo files focused on Go logic. One major type per file.

---

## 2. DB Pool Wrapper

**Decision:** Thin wrapper around `pgxpool.Pool` -- not exposing the pool directly.

The wrapper exists for three reasons:
1. Health check method (`Ping`) that the `/readyz` endpoint can call.
2. Graceful close that waits for in-flight queries.
3. Transaction factory method so repos do not import `pgxpool` directly.

```go
// DB manages the PostgreSQL connection pool and provides health checks.
type DB struct {
    pool    *pgxpool.Pool
    logger  *slog.Logger
    metrics StoreMetrics
}

// NewDB connects to PostgreSQL, validates the connection, and returns a DB.
// It fails fast if the database is unreachable at startup.
func NewDB(ctx context.Context, cfg config.DatabaseConfig, logger *slog.Logger, metrics StoreMetrics) (*DB, error)

// Ping tests the database connection. Used by the /readyz health check.
func (db *DB) Ping(ctx context.Context) error

// Close gracefully closes the connection pool.
func (db *DB) Close()

// Pool returns the underlying pgxpool.Pool for use by repositories.
// This is the only way repos get access to the pool.
func (db *DB) Pool() *pgxpool.Pool
```

**Why a wrapper instead of passing `*pgxpool.Pool` directly:**
- Repos need the pool, but cmd/ and health-check code should not deal with pgxpool
  types. The wrapper provides a clean boundary.
- Metrics (connection pool stats) are collected in one place.
- The Close semantics might need coordination later (drain event handlers before
  closing the pool).

**Why not a full abstraction layer / DBTX interface:**
- The repos are the only consumers of the pool. Adding a DBTX interface at this
  stage would be YAGNI. If we later need to mock the DB for unit tests, we define
  narrow interfaces at the consumer site (e.g., the event handler that calls the repo).

---

## 3. Repository Definitions

### VehicleRepo

```go
// VehicleRepo reads and writes vehicle records in the Prisma-owned "Vehicle" table.
// It never creates or deletes vehicles -- that is the Next.js app's responsibility.
type VehicleRepo struct {
    pool *pgxpool.Pool
}

func NewVehicleRepo(pool *pgxpool.Pool) *VehicleRepo

// GetByVIN returns the vehicle with the given VIN.
// Returns ErrVehicleNotFound if no vehicle has that VIN.
func (r *VehicleRepo) GetByVIN(ctx context.Context, vin string) (Vehicle, error)

// GetByID returns the vehicle with the given Prisma cuid.
// Returns ErrVehicleNotFound if no vehicle has that ID.
func (r *VehicleRepo) GetByID(ctx context.Context, id string) (Vehicle, error)

// UpdateTelemetry performs a batch update of real-time telemetry fields for
// one vehicle. Only non-nil fields in the update are written. This is the
// hot path -- called on every telemetry event.
func (r *VehicleRepo) UpdateTelemetry(ctx context.Context, vin string, update VehicleUpdate) error

// UpdateStatus sets the vehicle's status enum (driving, parked, charging, offline).
func (r *VehicleRepo) UpdateStatus(ctx context.Context, vin string, status VehicleStatus) error
```

### DriveRepo

```go
// DriveRepo manages drive records in the Prisma-owned "Drive" table.
type DriveRepo struct {
    pool *pgxpool.Pool
}

func NewDriveRepo(pool *pgxpool.Pool) *DriveRepo

// Create inserts a new drive record when a drive starts.
// The drive is created with placeholder end-time fields that will be
// filled in when the drive completes.
func (r *DriveRepo) Create(ctx context.Context, drive DriveRecord) error

// AppendRoutePoints appends route points to the drive's routePoints JSON
// array. Uses jsonb_concat to avoid read-modify-write.
func (r *DriveRepo) AppendRoutePoints(ctx context.Context, driveID string, points []RoutePointRecord) error

// Complete updates a drive with its final stats when the drive ends.
func (r *DriveRepo) Complete(ctx context.Context, driveID string, stats DriveCompletion) error

// GetByID returns a single drive by its ID.
// Returns ErrDriveNotFound if no drive has that ID.
func (r *DriveRepo) GetByID(ctx context.Context, id string) (DriveRecord, error)
```

---

## 4. Domain Types (internal/store/)

These are the Go structs that map to the PostgreSQL tables. They are NOT the
same as the event payload types in `internal/events/`. The repos translate
between event types and these storage types at the boundary.

```go
// VehicleStatus mirrors the Prisma "VehicleStatus" enum.
type VehicleStatus string

const (
    VehicleStatusDriving   VehicleStatus = "driving"
    VehicleStatusParked    VehicleStatus = "parked"
    VehicleStatusCharging  VehicleStatus = "charging"
    VehicleStatusOffline   VehicleStatus = "offline"
    VehicleStatusInService VehicleStatus = "in_service"
)

// Vehicle is a read model of the Prisma "Vehicle" table.
// Only the fields the telemetry server needs are included.
type Vehicle struct {
    ID             string
    UserID         string
    VIN            string
    Name           string
    Status         VehicleStatus
    ChargeLevel    int
    EstimatedRange int
    Speed          int
    GearPosition   *string  // nullable
    Heading        int
    Latitude       float64
    Longitude      float64
    InteriorTemp   int
    ExteriorTemp   int
    OdometerMiles  int
    LastUpdated    time.Time
}

// VehicleUpdate holds the subset of vehicle fields that can change from
// a single telemetry event. Nil pointer fields are not written.
type VehicleUpdate struct {
    Speed          *int
    ChargeLevel    *int
    EstimatedRange *int
    GearPosition   *string
    Heading        *int
    Latitude       *float64
    Longitude      *float64
    InteriorTemp   *int
    ExteriorTemp   *int
    OdometerMiles  *int
    LocationName   *string
    LocationAddr   *string
    LastUpdated    time.Time // always set
}

// DriveRecord maps to the Prisma "Drive" table.
type DriveRecord struct {
    ID               string
    VehicleID        string
    Date             string  // "2026-03-17" -- matches Prisma's String type
    StartTime        string  // ISO 8601 -- matches Prisma's String type
    EndTime          string
    StartLocation    string
    StartAddress     string
    EndLocation      string
    EndAddress       string
    DistanceMiles    float64
    DurationMinutes  int
    AvgSpeedMph      float64
    MaxSpeedMph      float64
    EnergyUsedKwh    float64
    StartChargeLevel int
    EndChargeLevel   int
    FsdMiles         float64
    FsdPercentage    float64
    Interventions    int
    RoutePoints      json.RawMessage // JSONB -- pass through as raw JSON
    CreatedAt        time.Time
}

// DriveCompletion holds the final values written when a drive ends.
type DriveCompletion struct {
    EndTime          string
    EndLocation      string
    EndAddress       string
    DistanceMiles    float64
    DurationMinutes  int
    AvgSpeedMph      float64
    MaxSpeedMph      float64
    EnergyUsedKwh    float64
    EndChargeLevel   int
    FsdMiles         float64
    FsdPercentage    float64
    Interventions    int
}

// RoutePointRecord is a single GPS point stored inside the Drive.routePoints
// JSONB array. Matches the existing JSON format used by the Next.js app.
type RoutePointRecord struct {
    Latitude  float64 `json:"lat"`
    Longitude float64 `json:"lng"`
    Speed     float64 `json:"speed"`
    Heading   float64 `json:"heading"`
    Timestamp string  `json:"timestamp"` // ISO 8601
}
```

---

## 5. Interface Definitions (Consumer Site)

Per CLAUDE.md: "Interfaces at consumer site. Define interfaces where they are
used, not where they are implemented."

The store repos are concrete structs. Interfaces are defined by the packages
that CONSUME them. Here is where each interface lives:

### internal/drives/ (Drive Detector)

The drive detector needs to create drives and update vehicle status when
drives start/end.

```go
// VehicleReader looks up vehicles by VIN. Defined in internal/drives/.
type VehicleReader interface {
    GetByVIN(ctx context.Context, vin string) (store.Vehicle, error)
}

// DriveWriter persists drive lifecycle events. Defined in internal/drives/.
type DriveWriter interface {
    Create(ctx context.Context, drive store.DriveRecord) error
    AppendRoutePoints(ctx context.Context, driveID string, points []store.RoutePointRecord) error
    Complete(ctx context.Context, driveID string, stats store.DriveCompletion) error
}
```

### internal/ws/ (WebSocket Broadcast)

The WebSocket server needs to look up vehicles to check authorization (does
this user own this VIN?) and to send the current state on connect.

```go
// VehicleLookup provides vehicle info for authorization and initial state.
// Defined in internal/ws/.
type VehicleLookup interface {
    GetByVIN(ctx context.Context, vin string) (store.Vehicle, error)
    GetByID(ctx context.Context, id string) (store.Vehicle, error)
}
```

### Event handler (internal/store/ -- subscriber adapter)

The store package itself subscribes to the event bus. It contains a
`Subscriber` (or `EventHandler`) struct that wires events to repo calls.
This struct lives in `internal/store/` and is NOT exposed via interface --
it is wired in `cmd/telemetry-server/main.go`.

```go
// Subscriber listens to event bus topics and persists data via repos.
// Lives in internal/store/subscriber.go (separate file, added later when
// implementing the event-to-store wiring in a future issue).
type Subscriber struct {
    vehicles *VehicleRepo
    drives   *DriveRepo
    bus      events.Bus
    logger   *slog.Logger
}
```

This file is NOT part of Issue #4 scope. It will come when we wire the
event bus to the store. Mentioned here for completeness of the architecture.

---

## 6. New Tables

**Decision:** No new tables in this issue.

**Rationale:**
- The Prisma `Drive` table already has a `routePoints JSONB` column. The
  Next.js frontend reads this column and renders routes from it.
- Introducing a separate `telemetry_points` relational table would mean:
  (a) duplicating data between the JSONB column and the new table, or
  (b) changing the frontend to join against a new table -- violating the
  contract with the Next.js app.
- The JSONB approach is fine for the data volumes we expect (a drive
  produces ~1-2 route points per second, so a 1-hour drive is ~3600-7200
  points -- well within JSONB performance limits with jsonb_concat).
- If we later need relational queries on telemetry points (e.g., geospatial
  indexing), we can add a table then without breaking the current design.

**Migration files:**
- `000001_telemetry_points.up.sql` -- EMPTY placeholder. We reserve the
  migration number but include only a comment explaining that the telemetry
  server uses Prisma-owned tables and does not create its own tables yet.
- This keeps the golang-migrate infrastructure in place so future issues
  can add tables without bootstrapping the migration system.

Actually, we DO need one migration: an index on `"Vehicle"."vin"` for the
`GetByVIN` lookup. BUT -- Prisma already adds a `@unique` constraint on
`vin`, which PostgreSQL implements with a unique index. So no migration
needed for that either.

**Final answer: no migration files in this issue.** Create the `migrations/`
directory with a `.gitkeep` so the directory exists for future migrations.

---

## 7. Transaction Handling

**Decision:** Method-level transactions using pgx's `pool.BeginTx`, scoped
within a single repo method. No cross-repo transaction support yet.

**Rationale:**
- The only write operations in this issue are:
  1. `UpdateTelemetry` -- single UPDATE statement, no transaction needed.
  2. `UpdateStatus` -- single UPDATE statement, no transaction needed.
  3. `Create` -- single INSERT, no transaction needed.
  4. `AppendRoutePoints` -- single UPDATE with jsonb_concat, no transaction needed.
  5. `Complete` -- single UPDATE, no transaction needed.

- The cross-repo case (completing a drive = update Drive + set Vehicle
  status to "parked") will be handled by the event handler (`Subscriber`),
  not by the repos. The event handler will call `DriveRepo.Complete()` and
  `VehicleRepo.UpdateStatus()` in sequence. If the second call fails, we
  have an inconsistent state, but:
  - This is recoverable: the next telemetry event will correct the vehicle
    status.
  - Adding a cross-repo transaction requires either (a) passing a `pgx.Tx`
    through the interface, which pollutes the consumer interfaces, or (b)
    a unit-of-work pattern, which is premature complexity.

- If cross-repo atomicity becomes necessary, we add a `WithTx` pattern
  later:

  ```go
  // Future: if we need cross-repo transactions
  func (db *DB) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
      tx, err := db.pool.Begin(ctx)
      if err != nil { return fmt.Errorf("begin tx: %w", err) }
      defer tx.Rollback(ctx)
      if err := fn(tx); err != nil { return err }
      return tx.Commit(ctx)
  }
  ```

  Repos would accept a `Querier` interface (`QueryRow`, `Exec`, `Query`)
  that both `pgxpool.Pool` and `pgx.Tx` satisfy. But this is NOT needed
  now and should not be built until a real use case demands it.

---

## 8. SQL Query Design

All queries live in `queries.go` as package-level `const` strings. Queries
use PostgreSQL quoted identifiers because Prisma generates camelCase column
names.

Key consideration: **Prisma uses quoted camelCase column names.** Every SQL
query must use `"columnName"` syntax, NOT `column_name`. The table names
are also quoted PascalCase: `"Vehicle"`, `"Drive"`.

Example queries (for reference -- exact SQL will be in implementation):

```sql
-- GetByVIN
SELECT "id", "userId", "vin", "name", "status", "chargeLevel",
       "estimatedRange", "speed", "gearPosition", "heading",
       "latitude", "longitude", "interiorTemp", "exteriorTemp",
       "odometerMiles", "lastUpdated"
FROM "Vehicle"
WHERE "vin" = $1

-- UpdateTelemetry (dynamic -- built at runtime based on non-nil fields)
-- Uses a builder pattern to construct SET clauses only for provided fields.

-- AppendRoutePoints
UPDATE "Drive"
SET "routePoints" = "routePoints" || $2::jsonb
WHERE "id" = $1
```

**Dynamic UPDATE for UpdateTelemetry:** Since telemetry events contain a
variable subset of fields, `UpdateTelemetry` must build its SET clause
dynamically. This is the one case where we use a query builder (a simple
local function, NOT an external library). The builder appends
`"columnName" = $N` for each non-nil field and collects the args slice.

---

## 9. Error Types

```go
package store

import "errors"

var (
    ErrVehicleNotFound = errors.New("vehicle not found")
    ErrDriveNotFound   = errors.New("drive not found")
    ErrDatabaseClosed  = errors.New("database connection closed")
)
```

All repo methods wrap errors with context:
```go
return Vehicle{}, fmt.Errorf("VehicleRepo.GetByVIN(%s): %w", vin, ErrVehicleNotFound)
```

VIN values in error messages: in production the caller may redact before
logging, but the store layer returns the full VIN in errors so that callers
can match with `errors.Is()`. The logging layer (event handler) is
responsible for redaction.

---

## 10. Metrics

```go
// StoreMetrics collects database operation metrics.
type StoreMetrics interface {
    // ObserveQueryDuration records the time taken for a database query.
    ObserveQueryDuration(operation string, seconds float64)

    // IncQueryError increments the count of failed database queries.
    IncQueryError(operation string)

    // SetPoolStats updates connection pool gauge metrics.
    SetPoolStats(acquired, idle, total int32)
}
```

Operations are named strings like `"vehicle.get_by_vin"`,
`"vehicle.update_telemetry"`, `"drive.create"`, `"drive.complete"`, etc.

A `NoopStoreMetrics` struct is provided for tests.

Pool stats are collected periodically (e.g., every 15s) by `DB` using
`pool.Stat()`.

---

## 11. Dependency Graph

```
cmd/telemetry-server/
  main.go
    imports: internal/store   (creates DB, repos)
    imports: internal/events  (creates bus)
    imports: internal/config  (loads config)

internal/store/
  imports: internal/config    (DatabaseConfig for NewDB)
  imports: pgxpool, pgx       (implementation)
  imports: log/slog            (structured logging)
  does NOT import: internal/events (no coupling to event bus)

internal/drives/  (future consumer)
  imports: internal/store     (types: Vehicle, DriveRecord, etc.)
  defines: VehicleReader, DriveWriter interfaces (consumer site)

internal/ws/  (future consumer)
  imports: internal/store     (types: Vehicle)
  defines: VehicleLookup interface (consumer site)
```

The store package has NO dependency on the events package. The wiring
between events and store happens in a `Subscriber` struct (future issue)
or directly in `main.go`. This prevents circular dependencies.

---

## 12. Risks

1. **Prisma schema changes break our queries.** The Next.js app can add
   columns or rename fields via Prisma migrations. Our SELECT queries will
   break if a column is renamed or removed. **Mitigation:** Integration
   tests run against the real schema. We should pin the Prisma migration
   version we tested against and add a CI step that detects schema drift.

2. **Dynamic UPDATE query builder has SQL injection potential.** Column
   names are hardcoded constants, not user input, so this is safe. But the
   builder must NEVER accept column names from outside the package.
   **Mitigation:** The builder uses a fixed allowlist of column names
   mapped to struct fields.

3. **JSONB append (`||`) performance on large routePoints arrays.** For
   very long drives (4+ hours), the JSONB array could grow to 15,000+
   elements. The `||` operator rewrites the entire JSONB value on each
   append. **Mitigation:** Batch route point appends (accumulate N points,
   flush once). The event handler's batch interval (from config:
   `BatchWriteInterval`) naturally provides this batching. If JSONB
   performance becomes an issue, we add a relational `telemetry_point`
   table later.

4. **Prisma's camelCase quoting is fragile.** Every query must use double
   quotes around identifiers. Missing a quote turns `"chargeLevel"` into
   `chargelevel`, which PostgreSQL lowercases and fails to find.
   **Mitigation:** All queries in `queries.go` with careful review. Tests
   catch this immediately.

---

## 13. Alternatives Considered

**Full DBTX interface (pool + tx share interface):**
Rejected. Adds complexity without a current consumer. We can add this when
we need cross-repo transactions.

**Separate telemetry_point table:**
Rejected. The existing `routePoints` JSONB column is the contract with the
Next.js frontend. Duplicating data introduces sync issues. We can add a
table later if we need geospatial queries.

**Exposing pgxpool.Pool directly (no wrapper):**
Rejected. The wrapper is minimal (3 methods) and provides a clean seam for
health checks and metrics. Without it, cmd/main.go and health handlers
would import pgxpool directly.

**Repository interfaces IN the store package:**
Rejected per CLAUDE.md rule: "Interfaces at consumer site." The store
package exports concrete structs. Consumers define the narrow interfaces
they need.

**Embedding DB in repos instead of passing pool:**
Rejected. Repos need the pool, not the wrapper. Passing the pool directly
is simpler and avoids coupling repos to the wrapper's lifecycle methods.

---

## 14. File-by-File Summary for go-engineer

| File | Contents | Est. Lines |
|------|----------|-----------|
| `db.go` | `DB` struct, `NewDB`, `Ping`, `Close`, `Pool`, pool stats collector | ~80 |
| `vehicle_repo.go` | `VehicleRepo`, `NewVehicleRepo`, `GetByVIN`, `GetByID`, `UpdateTelemetry`, `UpdateStatus` | ~120 |
| `drive_repo.go` | `DriveRepo`, `NewDriveRepo`, `Create`, `AppendRoutePoints`, `Complete`, `GetByID` | ~120 |
| `queries.go` | All SQL constants, dynamic update builder helper | ~100 |
| `errors.go` | `ErrVehicleNotFound`, `ErrDriveNotFound`, `ErrDatabaseClosed` | ~15 |
| `types.go` | `Vehicle`, `VehicleUpdate`, `VehicleStatus`, `DriveRecord`, `DriveCompletion`, `RoutePointRecord` | ~100 |
| `metrics.go` | `StoreMetrics` interface | ~25 |
| `noop_metrics.go` | `NoopStoreMetrics` struct | ~25 |
| `migrations/.gitkeep` | Empty placeholder | 0 |
