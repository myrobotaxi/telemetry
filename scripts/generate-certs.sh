#!/usr/bin/env bash
set -euo pipefail

# generate-certs.sh — Generate TLS certificates for Tesla Fleet Telemetry.
#
# Produces:
#   1. EC private key (secp256r1/prime256v1) — required by Tesla
#   2. Derived public key (host at .well-known endpoint for Tesla registration)
#   3. Self-signed server certificate (LOCAL DEV / testing only)
#   4. Test client certificate (for simulator / integration tests)
#
# Usage:
#   ./scripts/generate-certs.sh <domain> [options]
#
# Options:
#   --output-dir DIR   Output directory (default: ./certs)
#   --days N           Certificate validity in days (default: 365)
#   --force            Overwrite existing certificates
#   --client-only      Only generate the test client certificate
#   --help             Show this help message
#
# Examples:
#   ./scripts/generate-certs.sh myrobotaxi.app
#   ./scripts/generate-certs.sh myrobotaxi.app --output-dir /etc/certs --days 90
#   ./scripts/generate-certs.sh myrobotaxi.app --force

readonly SCRIPT_NAME="$(basename "$0")"
readonly REQUIRED_OPENSSL_VERSION="1.1.0"

# ─── Defaults ──────────────────────────────────────────────────────────
CERTS_DIR="./certs"
VALIDITY_DAYS=365
FORCE=false
CLIENT_ONLY=false
DOMAIN=""

# ─── Temp file cleanup ─────────────────────────────────────────────────
TEMP_FILES=()
cleanup() { rm -f "${TEMP_FILES[@]}"; }
trap cleanup EXIT

# ─── Helpers ───────────────────────────────────────────────────────────

usage() {
    sed -n '3,/^$/s/^# \?//p' "$0"
    exit 0
}

log()   { printf "[%s] %s\n" "$SCRIPT_NAME" "$*"; }
error() { printf "[%s] ERROR: %s\n" "$SCRIPT_NAME" "$*" >&2; }
die()   { error "$*"; exit 1; }

check_openssl() {
    if ! command -v openssl &>/dev/null; then
        die "openssl is not installed. Install it and try again."
    fi

    local version
    version="$(openssl version | awk '{print $2}')"
    log "Using openssl $version"
}

check_curve_support() {
    if ! openssl ecparam -name prime256v1 -check 2>/dev/null | grep -q "prime256v1"; then
        # Some openssl builds list it differently; try generating to confirm.
        if ! openssl ecparam -name prime256v1 -genkey -noout 2>/dev/null >/dev/null; then
            die "openssl does not support curve prime256v1 (secp256r1). Tesla requires this curve."
        fi
    fi
}

validate_domain() {
    local domain="$1"
    # Basic DNS-name validation: alphanumeric, hyphens, dots, no leading/trailing dot.
    if [[ ! "$domain" =~ ^[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?$ ]]; then
        die "Invalid domain name: $domain"
    fi
}

file_exists_guard() {
    local file="$1"
    local desc="$2"
    if [[ -f "$file" ]] && [[ "$FORCE" != "true" ]]; then
        die "$desc already exists at $file. Use --force to overwrite."
    fi
}

validate_key() {
    local key_file="$1"
    if ! openssl ec -in "$key_file" -check -noout 2>/dev/null; then
        die "Generated private key failed validation: $key_file"
    fi
    log "Private key validated OK"
}

validate_cert() {
    local cert_file="$1"
    local key_file="$2"
    local label="$3"

    # Check that the certificate can be read.
    if ! openssl x509 -in "$cert_file" -noout 2>/dev/null; then
        die "$label certificate is not a valid X.509 file: $cert_file"
    fi

    # Check that cert matches key (compare modulus hashes).
    local cert_hash key_hash
    cert_hash="$(openssl x509 -in "$cert_file" -pubkey -noout 2>/dev/null | openssl md5)"
    key_hash="$(openssl ec -in "$key_file" -pubout 2>/dev/null | openssl md5)"
    if [[ "$cert_hash" != "$key_hash" ]]; then
        die "$label certificate does not match its private key"
    fi

    # Print summary.
    local subject not_after
    subject="$(openssl x509 -in "$cert_file" -noout -subject 2>/dev/null | sed 's/^subject=//')"
    not_after="$(openssl x509 -in "$cert_file" -noout -enddate 2>/dev/null | sed 's/^notAfter=//')"
    log "$label certificate: subject=$subject, expires=$not_after"
}

validate_public_key() {
    local pub_file="$1"
    if ! openssl ec -pubin -in "$pub_file" -noout 2>/dev/null; then
        die "Generated public key failed validation: $pub_file"
    fi
    log "Public key validated OK"
}

# ─── Argument parsing ─────────────────────────────────────────────────

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --output-dir)
                CERTS_DIR="${2:?--output-dir requires a value}"
                shift 2
                ;;
            --days)
                VALIDITY_DAYS="${2:?--days requires a value}"
                if ! [[ "$VALIDITY_DAYS" =~ ^[0-9]+$ ]] || [[ "$VALIDITY_DAYS" -lt 1 ]]; then
                    die "--days must be a positive integer"
                fi
                shift 2
                ;;
            --force)
                FORCE=true
                shift
                ;;
            --client-only)
                CLIENT_ONLY=true
                shift
                ;;
            --help|-h)
                usage
                ;;
            -*)
                die "Unknown option: $1. Use --help for usage."
                ;;
            *)
                if [[ -z "$DOMAIN" ]]; then
                    DOMAIN="$1"
                else
                    die "Unexpected argument: $1"
                fi
                shift
                ;;
        esac
    done

    if [[ -z "$DOMAIN" ]]; then
        die "Domain is required. Usage: $SCRIPT_NAME <domain> [options]"
    fi

    validate_domain "$DOMAIN"
}

# ─── Certificate generation ───────────────────────────────────────────

generate_server_key() {
    local key_file="$CERTS_DIR/server.key"
    file_exists_guard "$key_file" "Server private key"

    log "Generating EC private key (prime256v1/secp256r1)..."
    openssl ecparam -name prime256v1 -genkey -noout -out "$key_file"
    chmod 600 "$key_file"
    validate_key "$key_file"
}

generate_public_key() {
    local key_file="$CERTS_DIR/server.key"
    local pub_file="$CERTS_DIR/public-key.pem"
    file_exists_guard "$pub_file" "Public key"

    log "Deriving public key from private key..."
    openssl ec -in "$key_file" -pubout -out "$pub_file" 2>/dev/null
    validate_public_key "$pub_file"
}

generate_server_cert() {
    local key_file="$CERTS_DIR/server.key"
    local cert_file="$CERTS_DIR/server.crt"
    file_exists_guard "$cert_file" "Server certificate"

    log "Generating self-signed server certificate (valid $VALIDITY_DAYS days)..."
    log "  WARNING: Self-signed certs are for LOCAL DEV ONLY."
    log "  For production, use Let's Encrypt or another trusted CA."

    # Create a temporary config for SAN support.
    local tmp_conf
    tmp_conf="$(mktemp)"
    TEMP_FILES+=("$tmp_conf")

    cat > "$tmp_conf" <<EOF
[req]
default_bits = 256
prompt = no
distinguished_name = dn
req_extensions = v3_req
x509_extensions = v3_ca

[dn]
CN = $DOMAIN

[v3_req]
subjectAltName = DNS:$DOMAIN

[v3_ca]
subjectAltName = DNS:$DOMAIN
basicConstraints = CA:FALSE
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
EOF

    openssl req -new -x509 \
        -key "$key_file" \
        -out "$cert_file" \
        -days "$VALIDITY_DAYS" \
        -config "$tmp_conf"

    validate_cert "$cert_file" "$key_file" "Server"
}

generate_client_cert() {
    local client_key="$CERTS_DIR/client.key"
    local client_cert="$CERTS_DIR/client.crt"
    file_exists_guard "$client_key" "Client private key"
    file_exists_guard "$client_cert" "Client certificate"

    log "Generating test client certificate (for simulator/tests)..."

    # Generate client key.
    openssl ecparam -name prime256v1 -genkey -noout -out "$client_key"
    chmod 600 "$client_key"

    # Self-sign a client cert with a test VIN in the CN.
    local tmp_conf
    tmp_conf="$(mktemp)"
    TEMP_FILES+=("$tmp_conf")

    cat > "$tmp_conf" <<EOF
[req]
default_bits = 256
prompt = no
distinguished_name = dn

[dn]
CN = 5YJ3E7EB2NF000001
O = Tesla Motors
OU = Fleet Telemetry Test
EOF

    openssl req -new -x509 \
        -key "$client_key" \
        -out "$client_cert" \
        -days "$VALIDITY_DAYS" \
        -config "$tmp_conf"

    validate_cert "$client_cert" "$client_key" "Client"
    log "Test client cert CN=5YJ3E7EB2NF000001 (simulated VIN)"
}

# ─── Main ─────────────────────────────────────────────────────────────

main() {
    parse_args "$@"

    log "Domain: $DOMAIN"
    log "Output: $CERTS_DIR"
    log "Validity: $VALIDITY_DAYS days"
    log "Force overwrite: $FORCE"

    check_openssl
    check_curve_support

    mkdir -p "$CERTS_DIR"

    if [[ "$CLIENT_ONLY" == "true" ]]; then
        # Only generate client cert (assumes server key already exists).
        generate_client_cert
    else
        generate_server_key
        generate_public_key
        generate_server_cert
        generate_client_cert
    fi

    # Add certs directory to .gitignore if not already there.
    local gitignore=".gitignore"
    if [[ -f "$gitignore" ]]; then
        if ! grep -q "^certs/" "$gitignore" 2>/dev/null; then
            echo "certs/" >> "$gitignore"
            log "Added certs/ to .gitignore"
        fi
    fi

    echo ""
    log "=== Generated files ==="
    ls -la "$CERTS_DIR/"

    echo ""
    log "=== Next steps ==="
    log "1. Host public key at:"
    log "   https://$DOMAIN/.well-known/appspecific/com.tesla.3p.public-key.pem"
    log "2. Register app at: https://developer.tesla.com"
    log "3. For production TLS, use Let's Encrypt:"
    log "   certbot certonly --standalone -d $DOMAIN"
    log "4. Push fleet telemetry config:"
    log "   ./scripts/push-fleet-config.sh <VIN> <AUTH_TOKEN>"
    log "5. Vehicle owners pair virtual key at:"
    log "   https://tesla.com/_ak/$DOMAIN"
}

main "$@"
