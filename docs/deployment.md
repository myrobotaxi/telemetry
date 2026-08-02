# Deployment Guide

This document covers building the Docker image, running the stack locally with
docker compose, deploying to Fly.io, and setting up TLS certificates.

---

## Prerequisites

- Docker 24+ (or Docker Desktop)
- Go 1.23+ (for local builds and linting)
- A Supabase project (or local Postgres for development)
- TLS certificates for the Tesla mTLS port (see [TLS Setup](#tls-setup))

---

## Building the Docker Image

### Local build

```bash
docker build -t telemetry-server:local .
```

The multi-stage Dockerfile produces a ~20 MB Alpine image. The binary is a
fully static Go binary (CGO_ENABLED=0) so no libc is needed at runtime.

### Verify image size

```bash
docker images telemetry-server:local --format "{{.Size}}"
```

Target: under 30 MB. If the image grows beyond that, check that test files and
docs are excluded by `.dockerignore`.

### Passing build-time version info

```bash
docker build \
  --build-arg VERSION=$(git describe --tags --always --dirty) \
  --build-arg COMMIT=$(git rev-parse --short HEAD) \
  -t telemetry-server:$(git rev-parse --short HEAD) .
```

---

## Running Locally with Docker Compose

### First-time setup

1. Copy the example env file and fill in your secrets:

   ```bash
   cp .env.example .env
   # Edit .env — set DATABASE_URL and AUTH_SECRET at minimum
   ```

2. Generate dev TLS certificates (see [TLS Setup](#tls-setup)).

3. Start the full stack:

   ```bash
   docker compose up --build
   ```

   Services that start:
   | Service | Host Port | Purpose |
   |---|---|---|
   | telemetry-server | 8443 | Tesla vehicle mTLS WebSocket |
   | telemetry-server | 8080 | Browser client WebSocket |
   | telemetry-server | 9090 | Prometheus /metrics |
   | postgres | 5432 | Local dev database |
   | prometheus | 9091 | Prometheus UI |

4. Verify the server is healthy:

   ```bash
   curl http://localhost:8080/healthz   # liveness
   curl http://localhost:8080/readyz    # readiness (requires DB connection)
   ```

### Using Supabase instead of local Postgres

Set `DATABASE_URL` to your Supabase connection string in `.env` and comment
out (or remove) the `depends_on: postgres` block in `docker-compose.yml`.

### Stopping the stack

```bash
docker compose down           # stop containers
docker compose down -v        # stop + remove volumes (wipes local Postgres data)
```

---

## Running Integration Tests

The `docker-compose.test.yml` file spins up an isolated Postgres for CI
integration tests:

```bash
docker compose -f docker-compose.test.yml up --abort-on-container-exit
```

The `integration-tests` service exits with the Go test process exit code, so
CI can use the exit code directly.

---

## Deploying to Fly.io

Deployment to Fly.io is handled automatically by the CI pipeline (`ci.yml`).
On every push to `main` (after all CI jobs pass), the `deploy` job runs
`flyctl deploy --remote-only` using a `FLY_API_TOKEN` secret.

### Manual deploy (if needed)

1. Install the Fly CLI:

   ```bash
   curl -L https://fly.io/install.sh | sh
   fly auth login
   ```

2. Deploy:

   ```bash
   fly deploy --remote-only
   ```

   Fly.io reads `fly.toml`, builds the Dockerfile, and runs health checks
   before routing traffic to the new release.

### Secrets

Set secrets via the Fly CLI:

```bash
fly secrets set DATABASE_URL="postgres://..."
fly secrets set AUTH_SECRET="$(openssl rand -hex 32)"
fly secrets set LOG_FORMAT=json

# Push notifications (MYR-186). The .p8 is multi-line, so use the base64
# form — a single env line with no embedded newlines.
fly secrets set APNS_KEY_P8_B64="$(base64 -i AuthKey_XXXXXXXXXX.p8)"
fly secrets set APNS_KEY_ID="XXXXXXXXXX"
```

---

## Environment Variables Reference

| Variable | Required | Description |
|---|---|---|
| `DATABASE_URL` | Yes | PostgreSQL connection string (Supabase or local) |
| `AUTH_SECRET` | Yes | JWT signing secret for browser client auth (32+ bytes) |
| `LOG_FORMAT` | No | Set to `json` for structured logs; omit for text |
| `PORT` | No | Override client WS port (default from config: 8080) |

Secrets are **never** embedded in the Docker image or config files. They are
injected at runtime via environment variables.

### Push notifications (MYR-186)

| Variable | Required | Description |
|---|---|---|
| `APNS_KEY_P8` | No | The APNs `.p8` auth key as raw PKCS#8 PEM. **Empty → sending disabled** (see below). |
| `APNS_KEY_P8_B64` | No | The same PEM, base64-encoded — the container-friendly form. `APNS_KEY_P8` wins if both are set. |
| `APNS_KEY_ID` | No | Apple's 10-character key id, sent as the provider token's `kid`. **Empty → sending disabled.** |
| `APNS_TEAM_ID` | No | Apple developer team id (the token's `iss`). Defaults to `NFKX777598`. |
| `APNS_TOPIC` | No | iOS bundle id sent as `apns-topic`. Defaults to `app.myrobotaxi.ios`. |
| `PUSH_ENABLED` | No | Kill-switch, default `true`. **Fails fast** on a non-boolean (`off`, `no`, `""` are errors, not "enabled") — a switch you cannot trust is worse than none. Accepted values are exactly `strconv.ParseBool`'s. |
| `LIVE_ACTIVITY_TICKER_ENABLED` | No | Kill-switch for the Live Activity **ETA ticker** only (MYR-172), default `true`. Same fail-fast parsing as `PUSH_ENABLED`. See below. |

**No new variable for the Live Activity topic.** Apple addresses Live Activity
pushes at `{bundleId}.push-type.liveactivity`, and the server **derives** that by
appending the suffix to `APNS_TOPIC` — it also sets the matching
`apns-push-type: liveactivity` header, since Apple rejects either one alone with
`TopicDisallowed` (a `403` that reads like a credential problem and is not one).
Deriving rather than configuring is deliberate: a second topic variable is a
second thing to get wrong, and the two could then drift apart in an environment
file. The same APNs key, key id and team id serve both channels.

### The Live Activity ETA ticker (MYR-172)

**Contract:** [`contracts/rest-api.md`](contracts/rest-api.md) §7.21 (FR-9.3,
NFR-3.21); classification in
[`contracts/data-classification.md`](contracts/data-classification.md) §1.18
(NFR-3.9).

`LIVE_ACTIVITY_TICKER_ENABLED` controls the loop that re-pushes a rider's Live
Activity every **60–90 seconds** (75s ± 20% jitter) while their ride is
`accepted`, `arrived` or `enroute`, so the ETA on the lock screen keeps moving.
It exists because the ETA is the one thing on that card that goes wrong by
**sitting still**: the status only changes when something happens, but "arrives
at 4:12" stops being true the moment traffic does. The same loop sends the
**held `end`** for rides that completed five minutes ago (MYR-421) and sweeps
registrations untouched for 24 hours, hourly.

**It is a SECOND kill-switch rather than a reuse of `PUSH_ENABLED`, because the
two stop different things.** `PUSH_ENABLED=false` silences every notification
this service sends, Live Activity updates included. Setting
`LIVE_ACTIVITY_TICKER_ENABLED=false` stops **only the periodic ETA refresh** —
ride lifecycle transitions (accepted, arrived, enroute, completed, cancelled)
still update the Activity as they happen, and it is still ended and dismissed
correctly at the end of the ride.

**The loop itself keeps running when the switch is off (MYR-421).** A completed
ride's `end` is deliberately held for five minutes and sent by this loop, and
that end is the second half of a lifecycle transition rather than a refresh —
so stopping the loop would strand every completed card on every rider's lock
screen until ActivityKit's own multi-hour ceiling, which is precisely what the
sentence above promises does not happen. The 24-hour registration sweep keeps
running for the same reason: it is a `DELETE`, not a push.

**What turning it off degrades to, and why that is a safe place to stand.**
Every Activity update carries an `aps.stale-date` three minutes out, so once a
card stops being refreshed ActivityKit renders its own "as of X min ago"
treatment. A rider therefore sees a card that **says it is out of date** rather
than one that confidently shows a stale ETA — degraded and honest, not dark.
That is the degradation MYR-194's staleness policy was designed for, and it is
the intermediate state an operator actually wants while investigating APNs
throttling on a high-frequency push. The default is ON because the periodic
refresh IS the feature, not an optimisation.

| Symptom | Cause | Fix |
|---|---|---|
| Startup log `live activity ticker started` with `eta_refresh=false` | `LIVE_ACTIVITY_TICKER_ENABLED` is set false | Expected while the switch is off. The loop still runs — it carries the held completion `end` — and only the ETA refresh is skipped; unset the variable (or set `true`) and restart to resume refreshes |
| Startup log `live activity ticker not wired` | The registry or the notifier failed to build | Not a switch — the loop did not start at all. Check the store and APNs wiring in the lines above it |
| A completed card is still on the lock screen more than ~7 minutes after dropoff | The held `end` has not gone out | Look for `live activity end held` (the hold was recorded) followed by `live activity ticker: held ends sent`. `live activity: end not delivered; rows left live for the next pass` means APNs refused and the next pass will retry; `held-end list failed` means the read is failing |
| Lock-screen cards go grey/"as of N min ago" mid-ride, but status changes still land | The ticker is off, or its passes are failing | Check for `live activity ticker: list failed`; confirm the switch, then `fly deploy` (or restart) |
| Live Activity sends log `apns status 403 TopicDisallowed` | `APNS_TOPIC` is not the app's real bundle id | Fix `APNS_TOPIC` — the Live Activity topic is derived from it and cannot be set independently |

**Keyless is a supported state.** Without `APNS_KEY_P8`/`APNS_KEY_ID` the
service starts normally, `PUT`/`DELETE /api/push/devices` stay mounted,
registrations still persist, and every would-be notification is logged as
`push skipped` with `apns_configured=false`. That is deliberate: clients can
register their tokens before the secrets exist, so the first deploy carrying
them reaches phones immediately instead of waiting for every installed app to
relaunch. A key that is **present but unparseable fails startup**, because at
that point the operator believes push is on.

**Sandbox vs production is per-device, not per-deploy.** The gateway is chosen
from the `sandbox` flag each client sends at registration, so one deployment
serves TestFlight and App Store builds simultaneously. Nothing here needs
changing between them.

| Symptom | Cause | Fix |
|---|---|---|
| Startup log `push notifications not configured; sends will be skipped` | `APNS_KEY_P8` and/or `APNS_KEY_ID` unset | Set both secrets per the block above, then `fly deploy` (or restart) — the sender is built at startup |
| Startup fails: `push: build apns client: …` | The key is set but is not a valid PKCS#8 PEM ECDSA key | Re-export the `.p8` from the Apple Developer portal; if using the `_B64` form, check the base64 encoding round-trips |
| Sends log `apns status 403` | Wrong `APNS_KEY_ID`/`APNS_TEAM_ID`, or the key was revoked | Verify the key id and team id against the Apple Developer portal |
| Sends log `apns status 400` and devices vanish | Token/gateway mismatch — a sandbox token sent to production or vice versa | Check the client is sending the correct `sandbox` flag for its build |

---

## TLS Setup

### Local development (self-signed)

Generate a self-signed certificate and CA for local mTLS testing:

```bash
./scripts/generate-certs.sh
```

This creates:
- `certs/server.crt` — server certificate
- `certs/server.key` — server private key
- `certs/ca.crt` — CA certificate for client verification

The `docker-compose.yml` mounts `./certs:/certs:ro` into the container. The
config at `configs/default.json` references `/certs/server.crt` etc.

### Production (Tesla Fleet API)

Tesla requires a publicly trusted TLS certificate on port 443. Use Let's
Encrypt via certbot:

```bash
certbot certonly --standalone -d your-domain.example.com
```

After obtaining certs:

1. Mount them into the container or set them as Fly.io secrets.
2. Update `DATABASE_URL` and `AUTH_SECRET` via `fly secrets set`.
3. Re-push the Fleet Telemetry config to Tesla:

   ```bash
   ./scripts/push-fleet-config.sh
   ```

### Certificate renewal

Let's Encrypt certificates expire every 90 days. Automate renewal with a cron
job:

```bash
# renew and re-push fleet config
certbot renew --quiet && ./scripts/push-fleet-config.sh
```

Monitor expiry with the `cert_expiry_seconds` Prometheus gauge emitted by the
server. Alert when it drops below 30 days.

---

## Health Check Endpoints

| Endpoint | Port | Type | Returns 200 when |
|---|---|---|---|
| `/healthz` | 8080 | Liveness | Server process is running |
| `/readyz` | 8080 | Readiness | DB connected and event bus active |
| `/metrics` | 9090 | Metrics | Always (Prometheus scrape target) |

Fly.io's healthcheck polls `/healthz`. Kubernetes readiness probes should use
`/readyz` to gate traffic until the service is fully initialised.

---

## Monitoring

Prometheus scrapes `/metrics` on port 9090. Key metrics to alert on:

| Metric | Alert threshold |
|---|---|
| `telemetry_vehicles_connected` | Alert if drops to 0 during expected active hours |
| `ws_messages_dropped_total` | Alert if rate > 0 (slow clients) |
| `store_errors_total` | Alert on any errors |
| `cert_expiry_seconds` | Alert if < 30 days |

Import the Grafana dashboard from `deployments/grafana/` (Phase 2) for a
pre-built overview.
