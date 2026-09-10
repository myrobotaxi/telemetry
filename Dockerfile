# Stage 1a: Build tesla-http-proxy in an isolated builder.
# Separate stage avoids polluting the telemetry server's go.mod.
# Clone + build because vehicle-command's go.mod has replace directives,
# which prevents `go install ...@latest` from working.
FROM golang:1.27-alpine AS proxy-builder

RUN apk add --no-cache git
WORKDIR /build
# Pin to a specific tag for reproducible builds.
# Update this when upgrading the proxy.
ARG VEHICLE_COMMAND_VERSION=v0.4.1
RUN git clone --depth 1 --branch ${VEHICLE_COMMAND_VERSION} https://github.com/teslamotors/vehicle-command.git .
RUN CGO_ENABLED=0 GOOS=linux go build -o /tesla-http-proxy ./cmd/tesla-http-proxy

# Stage 1b: Build telemetry-server
FROM golang:1.27-alpine AS builder

# Allow Go to download the toolchain version declared in go.mod.
ENV GOTOOLCHAIN=auto

RUN apk add --no-cache ca-certificates

WORKDIR /app

# Download dependencies first for better layer caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy the full source tree and build a static binary.
COPY . .

# Build-time version info (optional — pass via --build-arg).
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
        -ldflags="-s -w \
          -X main.version=${VERSION} \
          -X main.commit=${COMMIT} \
          -X main.date=${BUILD_DATE}" \
        -o /telemetry-server \
        ./cmd/telemetry-server

# Operator binaries for the MYR-433 / MYR-447 seal-then-purge sequence. They
# ship in the image because the secrets they need (DATABASE_URL,
# ENCRYPTION_KEY) exist only as Fly app secrets — running them anywhere else
# would mean exporting the key, which is the custody the purge exists to
# protect. Run via `fly ssh console`.
RUN CGO_ENABLED=0 GOOS=linux go build -o /backfill-account-tokens ./cmd/backfill-account-tokens \
 && CGO_ENABLED=0 GOOS=linux go build -o /backfill-vehicle-gps ./cmd/backfill-vehicle-gps \
 && CGO_ENABLED=0 GOOS=linux go build -o /backfill-route-blobs ./cmd/backfill-route-blobs \
 && CGO_ENABLED=0 GOOS=linux go build -o /backfill-location-labels ./cmd/backfill-location-labels \
 && CGO_ENABLED=0 GOOS=linux go build -o /purge-plaintext-columns ./cmd/purge-plaintext-columns \
 && CGO_ENABLED=0 GOOS=linux go build -o /scrub-passenger-fields ./cmd/scrub-passenger-fields

# Stage 2: Runtime
# Minimal Alpine image — only ca-certificates and the static binary.
FROM alpine:3.24

# ca-certificates is required at runtime so the server can open TLS
# connections to Supabase (PostgreSQL) and other external services.
RUN apk add --no-cache ca-certificates

# Run as an unprivileged user; never run as root in production.
RUN adduser -D -u 1000 appuser

COPY --from=builder /telemetry-server /usr/local/bin/telemetry-server
COPY --from=builder /backfill-account-tokens /usr/local/bin/backfill-account-tokens
COPY --from=builder /backfill-vehicle-gps /usr/local/bin/backfill-vehicle-gps
COPY --from=builder /backfill-route-blobs /usr/local/bin/backfill-route-blobs
COPY --from=builder /backfill-location-labels /usr/local/bin/backfill-location-labels
COPY --from=builder /purge-plaintext-columns /usr/local/bin/purge-plaintext-columns
COPY --from=builder /scrub-passenger-fields /usr/local/bin/scrub-passenger-fields
COPY --from=proxy-builder /tesla-http-proxy /usr/local/bin/tesla-http-proxy

# Entrypoint wrapper that starts the proxy sidecar alongside the telemetry server.
COPY deployments/start.sh /usr/local/bin/start.sh

# Operational config (no secrets — secrets arrive via env vars at runtime).
# Default: fly.json (TLS paths set — app handles mTLS on Fly.io).
# Override for other targets: docker build --build-arg CONFIG_FILE=default.json
ARG CONFIG_FILE=fly.json
COPY configs/${CONFIG_FILE} /etc/telemetry/config.json

USER appuser

# 443  — Tesla vehicle mTLS WebSocket
# 8080 — Browser client WebSocket
# 4443 — Tesla HTTP proxy (internal only — never expose publicly)
# 9090 — Prometheus /metrics
EXPOSE 443 8080 4443 9090

# start.sh conditionally launches tesla-http-proxy (when TESLA_KEY_FILE is set)
# then execs the telemetry server. Safe as default — gracefully skips the proxy
# when the key file is absent.
ENTRYPOINT ["start.sh"]
CMD ["-config", "/etc/telemetry/config.json"]
