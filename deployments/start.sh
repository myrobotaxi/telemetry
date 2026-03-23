#!/bin/sh
# start.sh — Wrapper entrypoint for Railway (single-container) deployments.
#
# Starts tesla-http-proxy as a background sidecar, then runs the telemetry
# server in the foreground. When the telemetry server exits, the proxy is
# cleaned up automatically.
#
# Environment variables consumed by the proxy:
#   TESLA_KEY_FILE              — Path to the Tesla app private key (required)
#   TESLA_HTTP_PROXY_PORT       — Port for the proxy to listen on (default: 4443)
#   TESLA_HTTP_PROXY_TLS_CERT   — TLS cert for the proxy's HTTPS listener
#   TESLA_HTTP_PROXY_TLS_KEY    — TLS key for the proxy's HTTPS listener
#   TESLA_HTTP_PROXY_HOST       — Bind address (default: localhost)

set -e

PROXY_PORT="${TESLA_HTTP_PROXY_PORT:-4443}"

# Only start the proxy if the Tesla private key is available.
if [ -n "$TESLA_KEY_FILE" ] && [ -f "$TESLA_KEY_FILE" ]; then
    echo "Starting tesla-http-proxy on port ${PROXY_PORT}..."
    tesla-http-proxy \
        -key-file "$TESLA_KEY_FILE" \
        -port "$PROXY_PORT" \
        -host 127.0.0.1 \
        -cert "${TESLA_HTTP_PROXY_TLS_CERT:-/certs/server.crt}" \
        -tls-key "${TESLA_HTTP_PROXY_TLS_KEY:-/certs/server.key}" &
    PROXY_PID=$!

    # Ensure proxy is cleaned up when the main process exits.
    trap 'kill $PROXY_PID 2>/dev/null; wait $PROXY_PID 2>/dev/null' EXIT INT TERM
else
    echo "TESLA_KEY_FILE not set or file not found — skipping tesla-http-proxy."
fi

# Run the telemetry server in the foreground, forwarding all arguments.
exec telemetry-server "$@"
