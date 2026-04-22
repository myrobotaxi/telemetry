# ops CLI

`cmd/ops` is the developer CLI for Tesla Fleet API operations and raw telemetry inspection. It replaces the deploy-and-`fly logs`-grep loop used for field verification work (MYR-25/28/29 and similar) and is the interim UX for the future shadcn/ui web test bench.

## Install

```bash
go install ./cmd/ops
# or
go build -o ./bin/ops ./cmd/ops
```

`go install` puts it on your `PATH` as `ops`. The examples below assume either that or `./bin/ops`.

## Environment variables

| Variable | Required for | Notes |
|---|---|---|
| `DATABASE_URL` | every subcommand | Supabase Postgres connection string. PgBouncer mode (`:6543`) is auto-detected. |
| `AUTH_TESLA_ID` | `auth token`, `auth link`, `fleet-config push` | Tesla OAuth client id. |
| `AUTH_TESLA_SECRET` | same as above | Tesla OAuth client secret. |
| `TESLA_PROXY_URL` | `fleet-config push` | Base URL of the running `tesla-http-proxy` sidecar (e.g. `https://localhost:4443`). |
| `FLEET_TELEMETRY_HOSTNAME` | `fleet-config push` | Hostname vehicles connect to after config (e.g. `telemetry.myrobotaxi.app`). |
| `FLEET_TELEMETRY_PORT` | `fleet-config push` | Default `443`. |
| `FLEET_TELEMETRY_CA` | `fleet-config push` (prod) | PEM-encoded CA cert served with the telemetry endpoint. |
| `DEBUG_FIELDS_TOKEN` | `fields watch` | Shared secret for `/api/debug/fields`. Set identically on the server. In non-dev mode the server requires ≥32 chars and clients must present the token; under `--dev` it is optional. |

`.env.local` from the sibling Next.js app (`../my-robo-taxi/.env.local`) contains every secret you need except `DEBUG_FIELDS_TOKEN`. The fastest local setup:

```bash
set -a && source ../my-robo-taxi/.env.local && set +a
```

## Subcommands

Run `ops help` any time for the flag summary. Every subcommand prints JSON to stdout; progress/warning logs go to stderr so you can pipe through `jq`.

### `ops auth link --user-id <id> [--port 8765]`

Runs the full Tesla OAuth browser flow and writes fresh `access_token` + `refresh_token` to the DB. Use this when `ops auth token` fails with `401 login_required` (meaning the stored refresh_token has been revoked or expired — Tesla rotates aggressively).

**One-time setup on Tesla Developer portal:** add `http://localhost:8765/callback` to your Fleet API app's allowed redirect URIs. Tesla apps support multiple redirect URIs, so this sits next to your production web redirect with no conflict.

```bash
ops auth link --user-id clxy...
```

The CLI opens Tesla's login page in your browser, you approve the scopes, Tesla redirects back to `localhost:8765/callback`, the CLI swaps the code for tokens and persists them. Then:

```bash
ops auth token --user-id clxy...   # should now succeed
```

Flags:

- `--port` — local HTTP port the CLI listens on. Default `8765`. Must match the redirect URI registered on the Tesla app.
- `--scopes` — space-separated OAuth scopes. Default includes `openid`, `offline_access`, `vehicle_device_data`, `vehicle_cmds`, `vehicle_charging_cmds`.
- `--timeout` — how long to wait for the browser flow. Default `2m`.

PKCE (S256) is implemented per RFC 7636 — no client secret is sent in the authorize URL, and the code exchange is bound to a fresh verifier per flow.

### `ops auth token --user-id <id>`

Reads the user's Tesla OAuth token from the DB (`Account` table) and refreshes it if it will expire within one minute. Prints the access token, refresh token, and expiry:

```bash
ops auth token --user-id clxy... | jq
```

```json
{
  "userId": "clxy...",
  "accessToken": "eyJhbGciOi...",
  "refreshToken": "eyJhbGciOi...",
  "expiresAt": "2026-04-20T05:14:22Z",
  "refreshed": true
}
```

`refreshed: true` means the token was expired and was refreshed against `https://auth.tesla.com/oauth2/v3/token`. If `AUTH_TESLA_ID`/`AUTH_TESLA_SECRET` are not set, the command returns the existing (possibly expired) token with `refreshed: false` and a warning on stderr.

### `ops vehicles list --user-id <id>`

Lists every vehicle owned by the user:

```bash
ops vehicles list --user-id clxy... | jq
```

```json
[
  {
    "id": "clvx...",
    "vin": "5YJ3E7EB2NF000001",
    "name": "Red Taxi",
    "status": "parked",
    "chargeLevel": 78,
    "lastUpdated": "2026-04-20T04:12:33Z"
  }
]
```

Use this to grab the VIN and vehicle id before running any VIN-specific command below.

### `ops fleet-config show`

Prints the `DefaultFieldConfig` the server pushes to Tesla:

```bash
ops fleet-config show | jq '.TimeToFullCharge, .Location'
```

Useful for confirming a field + interval before pushing it.

### `ops fleet-config push --vin <vin> --user-id <id>`

Pushes `DefaultFieldConfig` to Tesla for one vehicle, via the `tesla-http-proxy`. Behavior mirrors the server's `POST /api/fleet-config/{vin}` endpoint (ownership check, auto-refresh, config exp set to 350 days):

```bash
ops fleet-config push --vin 5YJ3E7EB2NF000001 --user-id clxy... | jq
```

```json
{
  "vin": "5YJ3E7EB2NF000001",
  "userId": "clxy...",
  "tokenRefreshed": false,
  "updatedVehicles": 1,
  "skippedVehicles": null
}
```

If `skippedVehicles` is non-null, Tesla rejected the push — the map value explains why (common: `missing_key` means the vehicle has not been paired yet; run the virtual-key pairing flow).

### `ops fields snapshot --vin <vin>`

One-shot dump of the current `Vehicle` row as JSON — the values the Next.js app reads from the DB:

```bash
ops fields snapshot --vin 5YJ3E7EB2NF000001 | jq
```

Use this to confirm a persisted value (e.g. `destinationName` or `tripDistanceRemaining`) matches what the UI is showing, independent of whether the WebSocket is connected.

### `ops fields watch --vin <vin> [--server <url>] [--token <token>]`

Streams raw decoded protobuf fields from the server's `/api/debug/fields` WebSocket endpoint, one JSON frame per decoded Tesla payload. Every frame contains every field the vehicle sent — including fields the broadcast pipeline filters out — with Tesla proto field numbers preserved:

```bash
ops fields watch --vin 5YJ3E7EB2NF000001 --server ws://localhost:8080 | jq
```

```json
{
  "vin": "5YJ3E7EB2NF000001",
  "timestamp": "2026-04-20T04:15:00.123Z",
  "fields": {
    "TimeToFullCharge": { "value": 1.5, "protoField": 43, "type": "double" },
    "Soc":              { "value": 78.2, "protoField": 8,  "type": "double" },
    "Location":         { "value": { "Latitude": 37.77, "Longitude": -122.41 }, "protoField": 21, "type": "location" },
    "Odometer":         { "value": null, "protoField": 5, "type": "invalid", "invalid": true }
  }
}
```

- `--server` accepts `ws://`, `wss://`, `http://`, or `https://`. `http*` is auto-upgraded to `ws*`.
- Omit `--vin` to stream all vehicles (useful when inspecting a fleet).
- Auth: if the server has `DEBUG_FIELDS_TOKEN` set, pass the same value via `--token` or the env var. The CLI always uses the `X-Debug-Token` header (query-param form exists for browsers but shows up in access logs).

#### How the endpoint gets mounted

`/api/debug/fields` is mounted under **either** gate:

| Gate | How to enable | Token required from client? | Intended use |
|---|---|---|---|
| **`--dev`** | `go run ./cmd/telemetry-server --dev --config configs/dev-notls.json` | No (but honored if set) | Local laptop server + simulator |
| **`DEBUG_FIELDS_TOKEN`** | Set on the server (any mode). **In non-dev mode the token must be ≥32 chars** or startup fails. | Yes — client must present the same token | Production server, tailing real-Tesla frames |

Startup logs print which gate is active and whether the token is required. If neither gate is satisfied, the endpoint is not mounted and raw field publication is off (zero cost).

#### Streaming against production (the headline workflow)

Real Teslas only dial the production server, so that's where `ops fields watch` has to connect for actual field verification. Enable the endpoint on Fly once:

```bash
flyctl secrets set DEBUG_FIELDS_TOKEN="$(openssl rand -base64 32)" -a myrobotaxi-telemetry
# Fly redeploys. Save the token locally — you cannot read it back from Fly.
```

Then from your laptop:

```bash
export DEBUG_FIELDS_TOKEN='<the value you just generated>'
ops fields watch --vin 7SAYGDET7TA613795 --server wss://telemetry.myrobotaxi.app
```

Drive the car (or wake it up) and frames stream into your terminal in real time. Rotate the secret by re-running `flyctl secrets set` with a new value.

#### Streaming against a local server

For the simulator or local development, use `--dev` so no token dance is needed:

```bash
go run ./cmd/telemetry-server --dev --config configs/dev-notls.json
ops fields watch --vin <vin> --server ws://localhost:8080
```

`configs/dev-notls.json` leaves TLS empty so you don't need to generate local certs just to boot the server.

## End-to-end recipe: verifying a Tesla field empirically (MYR-25 style)

The workflow that motivated this tool. Example: confirm the units of `TimeToFullCharge`.

One-time setup — set the debug token on the production server:

```bash
flyctl secrets set DEBUG_FIELDS_TOKEN="$(openssl rand -base64 32)" -a myrobotaxi-telemetry
# Copy the value into a secret manager / password manager for reuse.
```

Then per verification session:

```bash
# 1. Confirm the fleet is asking Tesla to send the field at a reasonable interval.
ops fleet-config show | jq '.TimeToFullCharge'
ops fleet-config push --vin 5YJ3... --user-id clxy...   # only if the interval changed

# 2. Tail the live stream from production while the car is awake/driving/charging.
export DEBUG_FIELDS_TOKEN='<the value from the Fly secret>'
ops fields watch --vin 5YJ3... --server wss://telemetry.myrobotaxi.app \
  | jq -c 'select(.fields.TimeToFullCharge) | {t: .timestamp, v: .fields.TimeToFullCharge}'
```

Watch a few frames, compare against the in-car display, and you can conclude whether the field arrives in hours, minutes, seconds, etc. — no redeploy, no Fly log tail.

> **Why production, not local?** Real Teslas only dial the server hostname pinned in their on-board `fleet_telemetry_config`. That pointer only ever resolves to the Fly-deployed prod server. A local `--dev` server is for the simulator; it will never receive a frame from a real car.

## Troubleshooting

- **`DATABASE_URL is required`** — source `../my-robo-taxi/.env.local` (or set the var directly).
- **`vehicle owner mismatch`** on `fleet-config push` — the `userId` you passed does not own the VIN. Run `ops vehicles list --user-id <id>` to confirm.
- **Empty output from `fields watch`** — the vehicle isn't streaming to the server you connected to. Remember real Teslas only dial production; pointing `--server` at a local `--dev` server with no simulator running will always be silent. Check the server logs for a `vehicle connected` line, or confirm with `ops fields snapshot --vin <vin>` that `lastUpdated` is recent.
- **`/api/debug/fields` endpoint not found (404)** — the server was started without either gate. Pass `--dev` locally, or set `DEBUG_FIELDS_TOKEN` (≥32 chars) on the deployed server.
- **Server refuses to start with `DEBUG_FIELDS_TOKEN must be at least 32 chars`** — non-dev mode enforces a length floor so weak tokens can't reach prod. Generate a proper one: `openssl rand -base64 32`.
- **`unexpected client frame` debug logs on the server** — safe to ignore. The debug endpoint is server→client only; any frame the client sends is logged and discarded.
- **`unauthorized` on `fields watch`** — `DEBUG_FIELDS_TOKEN` on the server does not match `--token`/the env var. Both sides must agree (or both be empty).
- **`401 login_required` on `ops auth token`** — the stored `refresh_token` is dead. Run `ops auth link --user-id <id>` to refresh via the browser OAuth flow, then retry.
- **`listen on port 8765 ... address already in use` on `ops auth link`** — another process holds the port. Close it or pass `--port <free-port>` (and make sure that port is registered on the Tesla app's redirect URIs).
