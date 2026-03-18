#!/usr/bin/env bash
set -euo pipefail

# check-cert-expiry.sh — Check TLS certificate expiration dates.
#
# Inspects server and client certificates and reports days until expiry.
# Exits non-zero if any certificate expires within the warning threshold.
# Designed for use in CI pipelines and monitoring systems.
#
# Usage:
#   ./scripts/check-cert-expiry.sh [options]
#
# Options:
#   --certs-dir DIR     Certificate directory (default: ./certs)
#   --warn-days N       Warning threshold in days (default: 30)
#   --cert FILE         Check a specific certificate file instead of the directory
#   --json              Output results as JSON (for monitoring integration)
#   --help              Show this help message
#
# Exit codes:
#   0 — All certificates are valid and not expiring soon
#   1 — Error (missing files, invalid certs, etc.)
#   2 — One or more certificates expire within the warning threshold
#
# Examples:
#   ./scripts/check-cert-expiry.sh
#   ./scripts/check-cert-expiry.sh --warn-days 14 --json
#   ./scripts/check-cert-expiry.sh --cert /etc/ssl/server.crt

readonly SCRIPT_NAME="$(basename "$0")"

# ─── Defaults ──────────────────────────────────────────────────────────
CERTS_DIR="./certs"
WARN_DAYS=30
SPECIFIC_CERT=""
JSON_OUTPUT=false

# Tracking for exit code.
EXIT_CODE=0
declare -a RESULTS=()

# ─── Helpers ───────────────────────────────────────────────────────────

usage() {
    sed -n '3,/^$/s/^# \?//p' "$0"
    exit 0
}

log()   { printf "[%s] %s\n" "$SCRIPT_NAME" "$*"; }
error() { printf "[%s] ERROR: %s\n" "$SCRIPT_NAME" "$*" >&2; }
die()   { error "$*"; exit 1; }

# Check a single certificate file and report its expiry status.
# Sets EXIT_CODE=2 if the cert expires within WARN_DAYS.
check_cert() {
    local cert_file="$1"
    local label="${2:-$(basename "$cert_file")}"

    if [[ ! -f "$cert_file" ]]; then
        error "Certificate file not found: $cert_file"
        EXIT_CODE=1
        return
    fi

    # Parse expiry date.
    local not_after_str
    not_after_str="$(openssl x509 -in "$cert_file" -noout -enddate 2>/dev/null)" || {
        error "Failed to read certificate: $cert_file (not a valid X.509 file?)"
        EXIT_CODE=1
        return
    }
    not_after_str="${not_after_str#notAfter=}"

    # Convert to epoch seconds.
    local not_after_epoch now_epoch days_remaining
    if date --version &>/dev/null 2>&1; then
        # GNU date (Linux).
        not_after_epoch="$(date -d "$not_after_str" +%s)"
    else
        # BSD date (macOS).
        not_after_epoch="$(date -j -f "%b %d %T %Y %Z" "$not_after_str" +%s 2>/dev/null)" || \
        not_after_epoch="$(date -j -f "%b  %d %T %Y %Z" "$not_after_str" +%s 2>/dev/null)" || {
            error "Failed to parse expiry date for $cert_file: $not_after_str"
            EXIT_CODE=1
            return
        }
    fi
    now_epoch="$(date +%s)"
    days_remaining=$(( (not_after_epoch - now_epoch) / 86400 ))

    # Extract subject for display.
    local subject
    subject="$(openssl x509 -in "$cert_file" -noout -subject 2>/dev/null | sed 's/^subject=//' | sed 's/^ *//')"

    # Determine status.
    local status="ok"
    if [[ "$days_remaining" -lt 0 ]]; then
        status="expired"
        EXIT_CODE=2
    elif [[ "$days_remaining" -le "$WARN_DAYS" ]]; then
        status="warning"
        EXIT_CODE=2
    fi

    # Store result for JSON output (use jq to safely escape values).
    RESULTS+=("$(jq -n --arg file "$cert_file" --arg label "$label" \
        --arg subject "$subject" --arg expires "$not_after_str" \
        --argjson days "$days_remaining" --arg status "$status" \
        '{file:$file, label:$label, subject:$subject, expires:$expires, days_remaining:$days, status:$status}')")

    # Human-readable output.
    if [[ "$JSON_OUTPUT" != "true" ]]; then
        case "$status" in
            ok)
                log "$label: OK ($days_remaining days remaining, expires $not_after_str)"
                ;;
            warning)
                log "$label: WARNING - expires in $days_remaining days ($not_after_str)"
                log "  Subject: $subject"
                log "  Action: Renew certificate before expiry!"
                ;;
            expired)
                log "$label: EXPIRED ($days_remaining days ago, was $not_after_str)"
                log "  Subject: $subject"
                log "  Action: Certificate must be renewed immediately!"
                ;;
        esac
    fi
}

# ─── Argument parsing ─────────────────────────────────────────────────

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --certs-dir)
                CERTS_DIR="${2:?--certs-dir requires a value}"
                shift 2
                ;;
            --warn-days)
                WARN_DAYS="${2:?--warn-days requires a value}"
                if ! [[ "$WARN_DAYS" =~ ^[0-9]+$ ]]; then
                    die "--warn-days must be a non-negative integer"
                fi
                shift 2
                ;;
            --cert)
                SPECIFIC_CERT="${2:?--cert requires a value}"
                shift 2
                ;;
            --json)
                JSON_OUTPUT=true
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
}

# ─── Main ─────────────────────────────────────────────────────────────

main() {
    parse_args "$@"

    if ! command -v openssl &>/dev/null; then
        die "openssl is not installed."
    fi

    if [[ -n "$SPECIFIC_CERT" ]]; then
        # Check a single cert.
        check_cert "$SPECIFIC_CERT" "$(basename "$SPECIFIC_CERT")"
    else
        # Check all certs in the directory.
        if [[ ! -d "$CERTS_DIR" ]]; then
            die "Certificate directory not found: $CERTS_DIR"
        fi

        local found=false
        for cert_file in "$CERTS_DIR"/*.crt "$CERTS_DIR"/*.pem; do
            [[ -f "$cert_file" ]] || continue

            # Skip non-certificate PEM files (private keys, public keys).
            if ! openssl x509 -in "$cert_file" -noout 2>/dev/null; then
                continue
            fi

            found=true
            check_cert "$cert_file" "$(basename "$cert_file")"
        done

        if [[ "$found" == "false" ]]; then
            die "No certificate files found in $CERTS_DIR"
        fi
    fi

    # JSON output.
    if [[ "$JSON_OUTPUT" == "true" ]]; then
        local json="["
        local first=true
        for result in "${RESULTS[@]}"; do
            if [[ "$first" == "true" ]]; then
                first=false
            else
                json+=","
            fi
            json+="$result"
        done
        json+="]"
        echo "$json" | jq .
    fi

    # Summary in human-readable mode.
    if [[ "$JSON_OUTPUT" != "true" ]]; then
        echo ""
        if [[ "$EXIT_CODE" -eq 0 ]]; then
            log "All certificates OK (warning threshold: $WARN_DAYS days)."
        elif [[ "$EXIT_CODE" -eq 2 ]]; then
            log "One or more certificates need attention (threshold: $WARN_DAYS days)."
        fi
    fi

    exit "$EXIT_CODE"
}

main "$@"
