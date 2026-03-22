# Stage 1: Build
FROM golang:1.23-alpine AS builder

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

# Stage 2: Runtime
# Minimal Alpine image — only ca-certificates and the static binary.
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
