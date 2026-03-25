#!/usr/bin/env bash
set -euo pipefail

# push-fleet-config.sh — Push fleet_telemetry_config to a Tesla vehicle.
#
# Configures a vehicle to stream telemetry to the MyRoboTaxi server via
# Tesla's Fleet API. Requires a valid Partner Auth Token and the target
# vehicle's VIN.
#
# Usage:
#   ./scripts/push-fleet-config.sh --vin <VIN> --token <AUTH_TOKEN> [options]
#
# Required:
#   --vin VIN          Vehicle Identification Number (17 characters)
#   --token TOKEN      Tesla Partner Auth Token (Bearer token)
#
# Options:
#   --ca-file PATH     CA certificate PEM file for vehicle TLS trust anchor.
#                      If omitted, "ca" is null (for publicly trusted certs like Let's Encrypt).
#   --hostname HOST    Telemetry server hostname (default: telemetry.myrobotaxi.app)
#   --port PORT        Telemetry server port (default: 443)
#   --interval SECS    Telemetry push interval in seconds (default: 5)
#   --api-base URL     Fleet API base URL (default: https://fleet-api.prd.na.vn.cloud.tesla.com)
#   --dry-run          Print the request without sending it
#   --help             Show this help message
#
# Environment variables (alternative to flags):
#   TESLA_AUTH_TOKEN           Partner Auth Token
#   TESLA_API_BASE             Fleet API base URL
#   FLEET_TELEMETRY_CA_FILE    CA certificate PEM file path
#
# Examples:
#   ./scripts/push-fleet-config.sh --vin 5YJ3E7EB2NF000001 --token eyJ...
#   ./scripts/push-fleet-config.sh --vin 5YJ3E7EB2NF000001 --token eyJ... --ca-file ./certs/ca.crt
#   TESLA_AUTH_TOKEN=eyJ... ./scripts/push-fleet-config.sh --vin 5YJ3E7EB2NF000001
#   ./scripts/push-fleet-config.sh --vin 5YJ3E7EB2NF000001 --token eyJ... --dry-run

readonly SCRIPT_NAME="$(basename "$0")"

# ─── Defaults ──────────────────────────────────────────────────────────
VIN=""
AUTH_TOKEN="${TESLA_AUTH_TOKEN:-}"
HOSTNAME="telemetry.myrobotaxi.app"
PORT=443
INTERVAL=5
API_BASE="${TESLA_API_BASE:-https://fleet-api.prd.na.vn.cloud.tesla.com}"
CA_FILE="${FLEET_TELEMETRY_CA_FILE:-}"
DRY_RUN=false

# ─── Telemetry fields ─────────────────────────────────────────────────
# These are the Tesla proto Field names that MyRoboTaxi needs. They must
# match the field names in Tesla's fleet_telemetry_config API. See:
# https://developer.tesla.com/docs/fleet-api/endpoints/vehicle-endpoints#fleet-telemetry-config
TELEMETRY_FIELDS=(
    "VehicleSpeed"
    "Location"
    "GpsHeading"
    "Gear"
    "Soc"
    "EstBatteryRange"
    "DetailedChargeState"
    "Odometer"
    "InsideTemp"
    "OutsideTemp"
    "DestinationName"
    "RouteLine"
    "SelfDrivingMilesSinceReset"
    "BatteryLevel"
    "IdealBatteryRange"
    "RatedRange"
    "EnergyRemaining"
    "PackVoltage"
    "PackCurrent"
    "VehicleName"
    "CarType"
    "Version"
    "Locked"
    "SentryMode"
    "OriginLocation"
    "DestinationLocation"
    "MilesToArrival"
    "MinutesToArrival"
    "LateralAcceleration"
    "LongitudinalAcceleration"
    "MilesSinceReset"
)

# ─── Helpers ───────────────────────────────────────────────────────────

usage() {
    sed -n '3,/^$/s/^# \?//p' "$0"
    exit 0
}

log()   { printf "[%s] %s\n" "$SCRIPT_NAME" "$*"; }
error() { printf "[%s] ERROR: %s\n" "$SCRIPT_NAME" "$*" >&2; }
die()   { error "$*"; exit 1; }

check_dependencies() {
    if ! command -v curl &>/dev/null; then
        die "curl is required but not installed."
    fi
    if ! command -v jq &>/dev/null; then
        die "jq is required but not installed. Install: brew install jq"
    fi
}

validate_vin() {
    local vin="$1"
    if [[ ${#vin} -ne 17 ]]; then
        die "VIN must be exactly 17 characters, got ${#vin}: $vin"
    fi
    if [[ ! "$vin" =~ ^[A-HJ-NPR-Z0-9]{17}$ ]]; then
        die "VIN contains invalid characters (I, O, Q not allowed): $vin"
    fi
}

validate_token() {
    local token="$1"
    if [[ -z "$token" ]]; then
        die "Auth token is required. Use --token or set TESLA_AUTH_TOKEN."
    fi
    if [[ ${#token} -lt 20 ]]; then
        die "Auth token looks too short. Verify your Partner Auth Token."
    fi
}

validate_ca_file() {
    local ca_file="$1"
    if [[ ! -f "$ca_file" ]]; then
        die "CA file not found: $ca_file"
    fi
    if ! openssl x509 -in "$ca_file" -noout 2>/dev/null; then
        die "CA file is not a valid PEM certificate: $ca_file"
    fi
}

# Build the JSON config payload.
build_config_payload() {
    local fields_json=""
    for field in "${TELEMETRY_FIELDS[@]}"; do
        if [[ -n "$fields_json" ]]; then
            fields_json+=","
        fi
        fields_json+="\"$field\":{\"interval_seconds\":$INTERVAL}"
    done

    # Build the ca value: JSON-escaped PEM string or null.
    local ca_value="null"
    if [[ -n "$CA_FILE" ]]; then
        ca_value="$(jq -Rs . < "$CA_FILE")"
    fi

    cat <<EOF
{
  "vins": ["$VIN"],
  "config": {
    "hostname": "$HOSTNAME",
    "port": $PORT,
    "ca": $ca_value,
    "fields": {$fields_json},
    "alert_types": ["service"],
    "exp": $(( $(date +%s) + 86400 * 30 ))
  }
}
EOF
}

# ─── Argument parsing ─────────────────────────────────────────────────

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --vin)
                VIN="${2:?--vin requires a value}"
                shift 2
                ;;
            --token)
                AUTH_TOKEN="${2:?--token requires a value}"
                shift 2
                ;;
            --ca-file)
                CA_FILE="${2:?--ca-file requires a value}"
                shift 2
                ;;
            --hostname)
                HOSTNAME="${2:?--hostname requires a value}"
                shift 2
                ;;
            --port)
                PORT="${2:?--port requires a value}"
                shift 2
                ;;
            --interval)
                INTERVAL="${2:?--interval requires a value}"
                if ! [[ "$INTERVAL" =~ ^[0-9]+$ ]] || [[ "$INTERVAL" -lt 1 ]]; then
                    die "--interval must be a positive integer"
                fi
                shift 2
                ;;
            --api-base)
                API_BASE="${2:?--api-base requires a value}"
                shift 2
                ;;
            --dry-run)
                DRY_RUN=true
                shift
                ;;
            --help|-h)
                usage
                ;;
            -*)
                die "Unknown option: $1. Use --help for usage."
                ;;
            *)
                die "Unexpected argument: $1"
                ;;
        esac
    done

    if [[ -z "$VIN" ]]; then
        die "VIN is required. Use --vin <VIN>."
    fi

    validate_vin "$VIN"
    validate_token "$AUTH_TOKEN"

    if [[ -n "$CA_FILE" ]]; then
        validate_ca_file "$CA_FILE"
    fi
}

# ─── Main ─────────────────────────────────────────────────────────────

main() {
    parse_args "$@"
    check_dependencies

    # Redact VIN in logs (show last 4 only).
    local vin_redacted="***${VIN: -4}"

    log "Vehicle: $vin_redacted"
    log "Hostname: $HOSTNAME:$PORT"
    log "Interval: ${INTERVAL}s"
    log "Fields: ${#TELEMETRY_FIELDS[@]}"
    log "API base: $API_BASE"
    if [[ -n "$CA_FILE" ]]; then
        log "CA cert: $CA_FILE"
    else
        log "CA cert: none (using publicly trusted cert)"
    fi

    local payload
    payload="$(build_config_payload)"

    local api_url="$API_BASE/api/1/vehicles/fleet_telemetry_config"

    if [[ "$DRY_RUN" == "true" ]]; then
        log "DRY RUN — would POST to: $api_url"
        echo "$payload" | jq .
        exit 0
    fi

    log "Pushing fleet_telemetry_config..."

    local http_code response
    response="$(mktemp)"
    trap "rm -f '$response'" EXIT

    http_code=$(curl -s -o "$response" -w "%{http_code}" \
        -X POST "$api_url" \
        -H "Authorization: Bearer $AUTH_TOKEN" \
        -H "Content-Type: application/json" \
        -d "$payload")

    log "HTTP status: $http_code"

    if [[ "$http_code" -ge 200 ]] && [[ "$http_code" -lt 300 ]]; then
        log "Fleet telemetry config pushed successfully."
        log "Response:"
        jq . "$response" 2>/dev/null || cat "$response"

        echo ""
        log "=== Next steps ==="
        log "1. Wait for vehicle to sync (synced: true in response)."
        log "   This can take minutes to hours depending on vehicle state."
        log "2. Verify config: GET $api_url"
        log "3. Vehicle must be online (not asleep) to sync."
        log "4. If synced: false persists, try waking the vehicle first."
    elif [[ "$http_code" -eq 401 ]]; then
        die "Authentication failed (401). Check your Partner Auth Token."
    elif [[ "$http_code" -eq 403 ]]; then
        error "Forbidden (403). Possible causes:"
        error "  - App not registered with Tesla (run /register first)"
        error "  - Vehicle owner has not paired virtual key"
        error "  - Virtual key URL: https://tesla.com/_ak/$HOSTNAME"
        jq . "$response" 2>/dev/null || cat "$response"
        exit 1
    elif [[ "$http_code" -eq 404 ]]; then
        die "Vehicle not found (404). Check VIN: $vin_redacted"
    elif [[ "$http_code" -eq 422 ]]; then
        error "Validation error (422). The config payload was rejected."
        jq . "$response" 2>/dev/null || cat "$response"
        exit 1
    elif [[ "$http_code" -eq 429 ]]; then
        die "Rate limited (429). Wait and try again."
    else
        error "Unexpected HTTP status: $http_code"
        jq . "$response" 2>/dev/null || cat "$response"
        exit 1
    fi
}

main "$@"
