# Stage 1: Build
# golang:1.25-alpine does not exist yet; use 1.23-alpine which matches the
# Go toolchain declared in go.mod (go 1.25.0 sets the minimum, not the image).
FROM golang:1.23-alpine AS builder

# ca-certificates is needed at build time so go mod download can reach HTTPS
# endpoints and so the final COPY of certs works in multi-platform builds.
RUN apk add --no-cache ca-certificates

WORKDIR /app

# Download dependencies first for better layer caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy the full source tree and build a static binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
        -ldflags="-s -w" \
        -o /telemetry-server \
        ./cmd/telemetry-server

# Stage 2: Runtime
# Minimal Alpine image — no shell, no package manager, no build tools.
FROM alpine:3.20

# ca-certificates is required at runtime so the server can open TLS
# connections to Supabase (PostgreSQL) and other external services.
RUN apk add --no-cache ca-certificates

# Run as an unprivileged user; never run as root in production.
RUN adduser -D -u 1000 appuser

COPY --from=builder /telemetry-server /usr/local/bin/telemetry-server

# Operational config (no secrets — secrets arrive via env vars at runtime).
COPY configs/default.json /etc/telemetry/config.json

USER appuser

# 443  — Tesla vehicle mTLS WebSocket
# 8080 — Browser client WebSocket
# 9090 — Prometheus /metrics
EXPOSE 443 8080 9090

ENTRYPOINT ["telemetry-server"]
CMD ["-config", "/etc/telemetry/config.json"]
