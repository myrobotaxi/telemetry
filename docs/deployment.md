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
