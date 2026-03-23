#!/bin/sh
# start.sh — Entrypoint wrapper for containerized deployments (Fly.io, etc).
#
# 1. Decodes base64-encoded secrets to files (for platforms that only support
#    env var secrets, not mounted files).
# 2. Starts tesla-http-proxy as a background sidecar.
# 3. Runs the telemetry server in the foreground.
#
# Base64-encoded secrets (set these as env vars):
#   TESLA_KEY_FILE_B64          — Tesla app private key (EC prime256v1 PEM)
#   TLS_CERT_B64                — Server TLS certificate PEM
#   TLS_KEY_B64                 — Server TLS private key PEM
#   TLS_CA_B64                  — Tesla CA certificate PEM (for mTLS client verification)
#
# File path overrides (if you mount secrets as files instead):
#   TESLA_KEY_FILE              — Path to Tesla app private key
#   TLS_CERT_FILE               — Path to server TLS certificate
#   TLS_KEY_FILE                — Path to server TLS key
#   TLS_CA_FILE                 — Path to Tesla CA certificate
#
# Proxy settings:
#   TESLA_HTTP_PROXY_PORT       — Proxy listen port (default: 4443)

set -e

CERTS_DIR="/tmp/certs"
mkdir -p "$CERTS_DIR"

# --- Decode base64 secrets to files ---

if [ -n "$TESLA_KEY_FILE_B64" ] && [ -z "$TESLA_KEY_FILE" ]; then
    echo "$TESLA_KEY_FILE_B64" | base64 -d > "$CERTS_DIR/tesla-key.pem"
    chmod 600 "$CERTS_DIR/tesla-key.pem"
    export TESLA_KEY_FILE="$CERTS_DIR/tesla-key.pem"
    echo "Decoded TESLA_KEY_FILE_B64 → $TESLA_KEY_FILE"
fi

if [ -n "$TLS_CERT_B64" ] && [ -z "$TLS_CERT_FILE" ]; then
    echo "$TLS_CERT_B64" | base64 -d > "$CERTS_DIR/server.crt"
    export TLS_CERT_FILE="$CERTS_DIR/server.crt"
    echo "Decoded TLS_CERT_B64 → $TLS_CERT_FILE"
fi

if [ -n "$TLS_KEY_B64" ] && [ -z "$TLS_KEY_FILE" ]; then
    echo "$TLS_KEY_B64" | base64 -d > "$CERTS_DIR/server.key"
    chmod 600 "$CERTS_DIR/server.key"
    export TLS_KEY_FILE="$CERTS_DIR/server.key"
    echo "Decoded TLS_KEY_B64 → $TLS_KEY_FILE"
fi

if [ -n "$TLS_CA_B64" ] && [ -z "$TLS_CA_FILE" ]; then
    echo "$TLS_CA_B64" | base64 -d > "$CERTS_DIR/tesla-ca.pem"
    export TLS_CA_FILE="$CERTS_DIR/tesla-ca.pem"
    echo "Decoded TLS_CA_B64 → $TLS_CA_FILE"
fi

# --- Start tesla-http-proxy sidecar ---

PROXY_PORT="${TESLA_HTTP_PROXY_PORT:-4443}"

if [ -n "$TESLA_KEY_FILE" ] && [ -f "$TESLA_KEY_FILE" ]; then
    # The proxy needs a TLS cert for its HTTPS listener.
    # Reuse the server cert if available, otherwise skip.
    PROXY_CERT="${TESLA_HTTP_PROXY_TLS_CERT:-${TLS_CERT_FILE:-}}"
    PROXY_KEY="${TESLA_HTTP_PROXY_TLS_KEY:-${TLS_KEY_FILE:-}}"

    if [ -n "$PROXY_CERT" ] && [ -n "$PROXY_KEY" ]; then
        echo "Starting tesla-http-proxy on port ${PROXY_PORT}..."
        tesla-http-proxy \
            -key-file "$TESLA_KEY_FILE" \
            -port "$PROXY_PORT" \
            -host 127.0.0.1 \
            -cert "$PROXY_CERT" \
            -tls-key "$PROXY_KEY" &
        PROXY_PID=$!
        trap 'kill $PROXY_PID 2>/dev/null; wait $PROXY_PID 2>/dev/null' EXIT INT TERM
    else
        echo "TLS cert/key not available — skipping tesla-http-proxy."
    fi
else
    echo "TESLA_KEY_FILE not set or file not found — skipping tesla-http-proxy."
fi

# --- Run telemetry server ---

exec telemetry-server "$@"
