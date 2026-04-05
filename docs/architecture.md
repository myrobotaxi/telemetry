# System Architecture

## Overview

The Robo-Taxi Telemetry Server is a Go service that sits between Tesla vehicles and browser clients. It receives real-time protobuf telemetry from Tesla's Fleet Telemetry system, processes it into domain events, and broadcasts updates to connected browsers via WebSocket.

## Architecture Diagram

```
                          ┌─────────────────────────────────────────────────────┐
                          │              Telemetry Server (Go)                  │
                          │                                                     │
Tesla Vehicle ──mTLS/WSS──┤  ┌────────────┐    ┌───────────┐                   │
Tesla Vehicle ──mTLS/WSS──┤──│  Receiver   │───►│ Event Bus │──┐               │
Tesla Vehicle ──mTLS/WSS──┤  │ (protobuf)  │    │ (channel) │  │               │
                          │  └────────────┘    └───────────┘  │               │
                          │                                     │               │
                          │  ┌────────────┐                     │               │
                          │  │   Drive     │◄───────────────────┤               │
                          │  │  Detector   │                    │               │
                          │  │ (state m/c) │───►[Drive Events]──┤               │
                          │  └────────────┘                     │               │
                          │                                     │               │
                          │  ┌────────────┐                     │               │──WSS──► Browser
                          │  │  WebSocket  │◄───────────────────┤               │──WSS──► Browser
                          │  │  Broadcast  │                    │               │──WSS──► Browser
                          │  └────────────┘                     │               │
                          │                                     │               │
                          │  ┌────────────┐                     │               │
                          │  │   Store     │◄───────────────────┘               │
                          │  │ (pgx/SQL)   │──────► PostgreSQL (Supabase)       │
                          │  └────────────┘                                     │
                          │                                                     │
                          │  ┌──────────────────────────┐                       │
                          │  │ Observability             │                       │
                          │  │ Prometheus │ slog │ OTEL  │                       │
                          │  └──────────────────────────┘                       │
                          └─────────────────────────────────────────────────────┘
```

## Component Design

### 1. Telemetry Receiver (`internal/telemetry/`)

Accepts mTLS WebSocket connections from Tesla vehicles. Decodes protobuf payloads into typed Go structs and publishes domain events to the event bus.

**Responsibilities:**
- mTLS handshake and vehicle certificate validation
- Protobuf deserialization (using Tesla's `vehicle_data.proto` schema)
- VIN extraction from client certificate
- Connection lifecycle management (heartbeat, reconnection tracking)
- Rate limiting inbound messages per vehicle
- Publishing `VehicleTelemetryEvent` to the event bus

**Key types:**
```go
type Receiver struct {
    server     *http.Server
    bus        events.Bus
    tlsConfig  *tls.Config
    metrics    *ReceiverMetrics
}

type TelemetryPayload struct {
    VIN       string
    Timestamp time.Time
    Fields    map[string]TelemetryValue
}

type TelemetryValue struct {
    Name      string
    Value     any  // narrowed by field type
    Timestamp time.Time
}
```

### 2. Event Bus (`internal/events/`)

In-process pub/sub system using Go channels. All communication between components flows through the event bus — no direct dependencies between receiver, drive detector, WebSocket server, or store.

**Responsibilities:**
- Topic-based publish/subscribe
- Fan-out delivery (one event → multiple subscribers)
- Backpressure handling (buffered channels with configurable size)
- Graceful shutdown (drain pending events)

**Key types:**
```go
type Bus interface {
    Publish(ctx context.Context, topic string, event Event) error
    Subscribe(topic string, handler Handler) (Subscription, error)
    Close() error
}

type Event struct {
    ID        string
    Topic     string
    Timestamp time.Time
    Payload   any
}

type Handler func(ctx context.Context, event Event) error
```

**Topics:**
| Topic | Payload | Publisher | Subscribers |
|-------|---------|-----------|-------------|
| `telemetry.vehicle` | `VehicleTelemetryEvent` | Receiver | Drive Detector, WS Broadcast, Store |
| `telemetry.connectivity` | `ConnectivityEvent` | Receiver | WS Broadcast, Store |
| `drive.started` | `DriveStartedEvent` | Drive Detector | WS Broadcast, Store |
| `drive.updated` | `DriveUpdatedEvent` | Drive Detector | WS Broadcast, Store |
| `drive.ended` | `DriveEndedEvent` | Drive Detector | Store |

### 3. Drive Detector (`internal/drives/`)

State machine that tracks vehicle drive lifecycle. Subscribes to telemetry events and detects drive start/end transitions based on gear state changes.

**State machine:**
```
                    Gear=D
    ┌────────┐  ───────────►  ┌──────────┐
    │  Idle  │                │ Driving  │
    └────────┘  ◄───────────  └──────────┘
                    Gear=P
                 (+ min duration
                  + min distance)
```

**Responsibilities:**
- Per-vehicle state tracking (concurrent map)
- Drive start detection: `ShiftState` transitions to `D` or `R`
- Drive end detection: `ShiftState` transitions to `P` (with debounce for traffic stops)
- Route point accumulation during active drives
- Stats calculation at drive end (distance, duration, speeds, energy)
- Reverse geocoding of start/end locations (Mapbox API)
- Micro-drive filtering (< 2 min OR < 0.1 miles discarded)

**Key types:**
```go
type Detector struct {
    bus      events.Bus
    geocoder Geocoder
    states   sync.Map // map[string]*VehicleDriveState
}

type VehicleDriveState struct {
    Status       DriveStatus // Idle | Driving
    CurrentDrive *ActiveDrive
}

type ActiveDrive struct {
    StartTime      time.Time
    StartLocation  Location
    RoutePoints    []RoutePoint
    MaxSpeed       float64
    StartCharge    float64
    StartOdometer  float64
    LastUpdate     time.Time
}
```

### 4. WebSocket Broadcast (`internal/ws/`)

Authenticated WebSocket server for browser clients. Subscribes to telemetry and drive events, transforms them into client-friendly JSON messages, and pushes to connected browsers.

**Responsibilities:**
- JWT/session token authentication on connection
- Per-user vehicle authorization (only receive data for vehicles they own or are shared with)
- Subscribe to event bus topics and fan-out to connected clients
- Client connection lifecycle (heartbeat, reconnection)
- Backpressure handling (slow clients get dropped, not blocked)
- Message formatting (transform internal events to client SDK format)

**Protocol:**
```json
// Server → Client: Vehicle update
{
  "type": "vehicle_update",
  "vehicleId": "clx...",
  "fields": {
    "speed": 65.2,
    "latitude": 33.0903,
    "longitude": -96.8237,
    "chargeLevel": 78,
    "heading": 245
  },
  "timestamp": "2026-03-16T14:30:00Z"
}

// Server → Client: Drive event
{
  "type": "drive_started",
  "vehicleId": "clx...",
  "driveId": "drv_...",
  "startLocation": "Thompson Hotel, Dallas, TX",
  "timestamp": "2026-03-16T14:30:00Z"
}

// Server → Client: Heartbeat
{
  "type": "heartbeat",
  "timestamp": "2026-03-16T14:30:00Z"
}

// Client → Server: Auth
{
  "type": "auth",
  "token": "eyJ..."
}
```

### 5. Store (`internal/store/`)

Database persistence layer using pgx. Writes to the shared Supabase PostgreSQL database. Reads existing tables (Vehicle, Drive, User) and writes telemetry updates.

**Responsibilities:**
- Vehicle status updates (speed, location, charge, heading, temps)
- Drive record creation and completion
- Route point batch inserts
- Connection pooling (pgxpool)
- Graceful handling of shared schema (Prisma owns migrations for existing tables)

**Key types:**
```go
type VehicleRepo struct {
    pool *pgxpool.Pool
}

type DriveRepo struct {
    pool *pgxpool.Pool
}
```

### 6. Auth (`internal/auth/`)

Authentication and authorization for client WebSocket connections.

**Responsibilities:**
- JWT token validation (shared secret with NextAuth.js)
- Session token lookup in database
- Vehicle ownership/sharing authorization check
- Token refresh handling

### 7. SDK (`pkg/sdk/`)

Abstract interfaces that define the client contract. Both the Go WebSocket server implementation and future mobile SDK implementations conform to these interfaces.

**Key types:**
```go
// Connection represents a telemetry stream connection
type Connection interface {
    Connect(ctx context.Context, opts ConnectOptions) error
    Disconnect() error
    Status() ConnectionStatus
    OnStatusChange(handler StatusHandler)
}

// VehicleStream provides real-time vehicle telemetry
type VehicleStream interface {
    Subscribe(vehicleID string, handler UpdateHandler) (Subscription, error)
    Unsubscribe(sub Subscription) error
}

// UpdateHandler processes vehicle telemetry updates
type UpdateHandler func(update VehicleUpdate)

// VehicleUpdate is the canonical update format
type VehicleUpdate struct {
    VehicleID string
    Fields    map[string]any
    Timestamp time.Time
}

type ConnectionStatus string
const (
    StatusConnecting   ConnectionStatus = "connecting"
    StatusConnected    ConnectionStatus = "connected"
    StatusReconnecting ConnectionStatus = "reconnecting"
    StatusDisconnected ConnectionStatus = "disconnected"
)
```

## Data Flow (End-to-End)

```
1. Tesla vehicle pushes protobuf telemetry over mTLS WebSocket (port 443)
2. Receiver decodes protobuf → TelemetryPayload
3. Receiver publishes VehicleTelemetryEvent to event bus
4. Event bus fans out to subscribers:
   a. Drive Detector: updates per-vehicle state machine
      - If drive started → publishes DriveStartedEvent
      - If driving → publishes DriveUpdatedEvent (route points, stats)
      - If drive ended → publishes DriveEndedEvent (final stats)
   b. WS Broadcast: transforms to JSON, pushes to authorized browser clients
   c. Store: batch-writes vehicle status + drive records to PostgreSQL
5. Browser receives JSON via WebSocket → useVehicleStream hook updates React state
```

## Deployment Architecture

### Phase 1: Single Node (Fly.io)

```
┌─────────────────────────────┐
│  Fly.io                     │
│  ┌───────────────────────┐  │
│  │  telemetry-server     │  │     ┌──────────────┐
│  │  (single Go binary)   │──────►│  Supabase    │
│  │                       │  │     │  PostgreSQL  │
│  │  :443 (Tesla mTLS)    │  │     └──────────────┘
│  │  :8080 (Client WS)    │  │
│  │  :9090 (Prometheus)   │  │
│  └───────────────────────┘  │
└─────────────────────────────┘
```

### Phase 2: Production (Kubernetes)

```
┌─────────────────────────────────────┐
│  Kubernetes Cluster                 │
│  ┌─────────────┐  ┌──────────────┐ │
│  │ Tesla LB    │  │ Client LB    │ │
│  │ (mTLS term) │  │ (TLS term)   │ │
│  └──────┬──────┘  └──────┬───────┘ │
│         │                │          │
│  ┌──────▼────────────────▼───────┐  │
│  │  telemetry-server (N replicas)│  │
│  └───────────────┬───────────────┘  │
│                  │                   │
│  ┌───────────────▼───────────────┐  │
│  │  PostgreSQL (Supabase)        │  │
│  └───────────────────────────────┘  │
└─────────────────────────────────────┘
```

## Configuration

```json
{
  "server": {
    "tesla_port": 443,
    "client_port": 8080,
    "metrics_port": 9090
  },
  "tls": {
    "cert_file": "/certs/server.crt",
    "key_file": "/certs/server.key",
    "ca_file": "/certs/tesla-ca.pem"
  },
  "database": {
    "max_conns": 20,
    "min_conns": 5
  },
  "telemetry": {
    "max_vehicles": 100,
    "event_buffer_size": 1000,
    "batch_write_interval": "5s",
    "batch_write_size": 100
  },
  "drives": {
    "min_duration": "2m",
    "min_distance_miles": 0.1,
    "end_debounce": "30s",
    "geocode_timeout": "5s"
  },
  "websocket": {
    "heartbeat_interval": "15s",
    "write_timeout": "10s",
    "max_connections_per_user": 5,
    "read_limit": 4096
  },
  "auth": {
    "token_issuer": "myrobotaxi",
    "token_audience": "telemetry"
  }
}
```

## Implementation Phases

### Phase 1: Foundation (Weeks 1-2)
- [ ] Project scaffolding, Go module init, CI/CD
- [ ] Event bus implementation with tests
- [ ] Configuration loading and validation
- [ ] Database connection and repository layer
- [ ] Health check endpoints

### Phase 2: Tesla Integration (Weeks 2-3)
- [ ] mTLS certificate setup and management
- [ ] Protobuf code generation from Tesla's proto files
- [ ] Telemetry receiver with vehicle connection management
- [ ] Tesla Fleet API: fleet_telemetry_config endpoint integration
- [ ] Virtual key pairing flow documentation

### Phase 3: Real-Time Processing (Weeks 3-4)
- [ ] Drive detection state machine
- [ ] Reverse geocoding integration (Mapbox)
- [ ] Vehicle status persistence (batch writes)
- [ ] Drive record lifecycle (create, update, complete)

### Phase 4: Client WebSocket (Weeks 4-5)
- [ ] Authenticated WebSocket server
- [ ] Per-user vehicle authorization
- [ ] Event-to-JSON transformation and broadcast
- [ ] Client SDK interfaces (pkg/sdk)
- [ ] Integration with MyRoboTaxi frontend (set NEXT_PUBLIC_WS_URL)

### Phase 5: Hardening (Weeks 5-6)
- [ ] Load testing (k6 scripts)
- [ ] Security audit (mTLS, auth, input validation)
- [ ] Prometheus dashboards
- [ ] Graceful shutdown and connection draining
- [ ] Deployment automation (Docker, Helm)
