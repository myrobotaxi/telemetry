# Vehicle Simulator

The simulator is a mock Tesla vehicle that sends real protobuf telemetry to the telemetry server over mTLS WebSocket. Use it to test the full pipeline without a real car.

## Prerequisites

### 1. Generate dev certificates

The simulator requires mTLS certificates. Generate them once:

```bash
mkdir -p certs

# Homebrew openssl@3 ≥ 3.6 ships without /opt/homebrew/etc/openssl@3/openssl.cnf
# and every `openssl req` call fails looking for it. Pointing OPENSSL_CONF at
# /dev/null tells openssl to use compiled-in defaults, which is all this
# recipe needs. macOS LibreSSL at /usr/bin/openssl isn't a drop-in
# replacement because it lacks `-copy_extensions`.
export OPENSSL_CONF=/dev/null

# CA
openssl ecparam -name prime256v1 -genkey -noout -out certs/ca.key
openssl req -new -x509 -key certs/ca.key -out certs/ca.crt -days 365 -subj "/CN=MyRoboTaxi Dev CA"

# Server cert (signed by CA)
openssl ecparam -name prime256v1 -genkey -noout -out certs/server.key
openssl req -new -key certs/server.key -out certs/server.csr -subj "/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
openssl x509 -req -in certs/server.csr -CA certs/ca.crt -CAkey certs/ca.key \
  -CAcreateserial -out certs/server.crt -days 365 -copy_extensions copyall

# Client cert (VIN in CN, signed by CA)
openssl ecparam -name prime256v1 -genkey -noout -out certs/client.key
openssl req -new -key certs/client.key -out certs/client.csr -subj "/CN=5YJ3SIM00001"
openssl x509 -req -in certs/client.csr -CA certs/ca.crt -CAkey certs/ca.key \
  -CAcreateserial -out certs/client.crt -days 365

# Cleanup
chmod 600 certs/*.key
rm -f certs/*.csr certs/*.srl
unset OPENSSL_CONF
```

The client certificate's Common Name (CN) is the VIN that the server will extract via mTLS — this is how Tesla vehicles identify themselves.

### 2. Start the telemetry server

```bash
DATABASE_URL="your-supabase-url" \
AUTH_SECRET="your-auth-secret" \
go run ./cmd/telemetry-server -config configs/dev.json
```

The dev config (`configs/dev.json`) binds the Tesla mTLS port to **8443** (no root required) and loads certs from `certs/`.

## Usage

```bash
# Single vehicle, highway scenario (defaults)
go run ./cmd/simulator --server wss://localhost:8443 --scenario highway-drive

# City driving
go run ./cmd/simulator --server wss://localhost:8443 --scenario city-drive

# Edge case testing (rapid gear changes)
go run ./cmd/simulator --server wss://localhost:8443 --scenario parking-lot

# Five simultaneous vehicles
go run ./cmd/simulator --server wss://localhost:8443 --vehicles 5

# Custom VIN prefix and send interval
go run ./cmd/simulator --server wss://localhost:8443 --vin TESTVIN --interval 500ms

# Custom cert paths
go run ./cmd/simulator --server wss://localhost:8443 \
  --cert /path/to/client.crt --key /path/to/client.key --ca /path/to/ca.crt
```

## CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--server` | *(required)* | WebSocket URL of the telemetry server |
| `--scenario` | `highway-drive` | Scenario: `highway-drive`, `city-drive`, `parking-lot` |
| `--vehicles` | `1` | Number of simultaneous vehicles |
| `--vin` | `5YJ3SIM` | VIN prefix (vehicles get sequential suffixes: 00001, 00002, ...) |
| `--interval` | `1s` | Telemetry send interval |
| `--cert` | `certs/client.crt` | Path to client TLS certificate |
| `--key` | `certs/client.key` | Path to client TLS key |
| `--ca` | `certs/ca.crt` | Path to CA certificate |

## Scenarios

### `highway-drive` (~30 minutes simulated)

Simulates a highway trip starting from downtown Dallas, TX (32.7767, -96.7970):

1. **Start parked** — gear P, speed 0, charge 88%
2. **Accelerate** — shift to D, ramp up to 65-75 mph
3. **Highway cruising** — maintain speed with small jitter, heading northeast
4. **Decelerate** — slow down approaching destination
5. **Park** — shift to P, speed 0

Charge drains ~0.1% per tick. Odometer advances based on speed. GPS position advances using spherical-Earth math.

### `city-drive` (~15 minutes simulated)

Simulates urban stop-and-go driving:

1. **Start parked** — gear P
2. **Multiple drive segments** — alternate between moving (15-35 mph) and stopped (gear P, 5-second stops)
3. **End parked** — gear P

Tests the drive detector's ability to handle frequent stops without ending the drive prematurely.

### `parking-lot` (brief)

Simulates rapid gear transitions for edge case testing:

1. Rapid P → D → R → P cycles
2. Minimal movement between shifts
3. Tests drive detection micro-drive filtering

## Telemetry Fields

Each message contains 9 protobuf fields using Tesla's encoding:

| Field | Proto Type | Example Value |
|-------|-----------|---------------|
| VehicleSpeed | `string_value` | `"65.2"` |
| Location | `location_value` | `{lat: 32.78, lng: -96.79}` |
| Heading | `string_value` | `"45.0"` |
| GearPosition | `shift_state` | `ShiftDrive` |
| Soc (charge %) | `string_value` | `"87"` |
| EstBatteryRange | `string_value` | `"218"` |
| InsideTemp | `string_value` | `"22"` |
| OutsideTemp | `string_value` | `"28"` |
| Odometer | `string_value` | `"12451.3"` |

## Verifying Data Flow

While the simulator is running, check the server is receiving data:

```bash
# Server logs (run server with debug logging)
go run ./cmd/telemetry-server -config configs/dev.json -log-level debug

# Check metrics
curl -s http://localhost:9090/metrics | grep telemetry
```

You should see `telemetry received` debug logs with VIN (redacted), field count, and latency for each message.

## Multiple Vehicles

When running with `--vehicles N`, each vehicle gets a unique VIN (e.g., `5YJ3SIM00001`, `5YJ3SIM00002`) and runs independently. All vehicles share the same client cert — in production, each vehicle has its own cert, but for simulation this is sufficient since the server uses the cert CN as the VIN.

To simulate multiple vehicles with different VINs in certs, generate separate client certs with different CNs and run multiple simulator instances.
