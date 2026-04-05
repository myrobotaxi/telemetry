#!/usr/bin/env bash
set -euo pipefail

# generate-certs.sh — Generate TLS certificates for Tesla Fleet Telemetry.
#
# Produces:
#   1. CA key + cert (EC secp256r1, self-signed, 10 years) — required by Tesla mTLS
#   2. Derived public key (host at .well-known endpoint for Tesla registration)
#   3. Server key + cert (RSA 2048, CA-signed) — proven to work with Tesla vehicles
#   4. Test client certificate (for simulator / integration tests)
#
# Usage:
#   ./scripts/generate-certs.sh <domain> [options]
#
# Options:
#   --output-dir DIR   Output directory (default: ./certs)
#   --days N           Server certificate validity in days (default: 365)
#   --force            Overwrite existing certificates
#   --client-only      Only generate the test client certificate
#   --help             Show this help message
#
# Examples:
#   ./scripts/generate-certs.sh telemetry.myrobotaxi.app
#   ./scripts/generate-certs.sh telemetry.myrobotaxi.app --output-dir /etc/certs --days 90
#   ./scripts/generate-certs.sh telemetry.myrobotaxi.app --force

readonly SCRIPT_NAME="$(basename "$0")"
readonly REQUIRED_OPENSSL_VERSION="1.1.0"

# CA validity is always 10 years — not configurable to prevent accidental rotation.
readonly CA_VALIDITY_DAYS=3650

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

# validate_ec_key validates an EC private key file.
validate_ec_key() {
    local key_file="$1"
    if ! openssl ec -in "$key_file" -check -noout 2>/dev/null; then
        die "Generated EC private key failed validation: $key_file"
    fi
    log "EC private key validated OK: $key_file"
}

# validate_rsa_key validates an RSA private key file.
validate_rsa_key() {
    local key_file="$1"
    if ! openssl rsa -in "$key_file" -check -noout 2>/dev/null; then
        die "Generated RSA private key failed validation: $key_file"
    fi
    log "RSA private key validated OK: $key_file"
}

# validate_cert checks cert readability, cert/key match, and prints a summary.
# Uses openssl pkey for key matching so it works for both EC and RSA keys.
validate_cert() {
    local cert_file="$1"
    local key_file="$2"
    local label="$3"

    # Check that the certificate can be read.
    if ! openssl x509 -in "$cert_file" -noout 2>/dev/null; then
        die "$label certificate is not a valid X.509 file: $cert_file"
    fi

    # Check that cert matches key using openssl pkey (works for EC and RSA).
    local cert_hash key_hash
    cert_hash="$(openssl x509 -in "$cert_file" -pubkey -noout 2>/dev/null | openssl md5)"
    key_hash="$(openssl pkey -in "$key_file" -pubout 2>/dev/null | openssl md5)"
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

# generate_ca creates the CA key and self-signed cert.
#
# The CA key is EC (secp256r1) because Tesla uses the CA's public key for app
# registration at the .well-known endpoint. The cert is valid for 10 years.
#
# The CA is only generated if ca.key does not exist (unless --force), because
# regenerating the CA invalidates every server cert already signed by it.
generate_ca() {
    local ca_key="$CERTS_DIR/ca.key"
    local ca_cert="$CERTS_DIR/ca.crt"

    if [[ -f "$ca_key" ]] && [[ "$FORCE" != "true" ]]; then
        log "CA key already exists at $ca_key — skipping CA generation."
        log "  (Use --force to regenerate. WARNING: this invalidates all signed server certs.)"
        return 0
    fi

    log "Generating CA EC private key (prime256v1/secp256r1, 10-year validity)..."
    openssl ecparam -name prime256v1 -genkey -noout -out "$ca_key"
    chmod 600 "$ca_key"
    validate_ec_key "$ca_key"

    local tmp_conf
    tmp_conf="$(mktemp)"
    TEMP_FILES+=("$tmp_conf")

    cat > "$tmp_conf" <<EOF
[req]
prompt = no
distinguished_name = dn

[dn]
CN = $DOMAIN CA

[v3_ca]
basicConstraints = critical, CA:TRUE, pathlen:0
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
EOF

    log "Generating self-signed CA certificate (valid ${CA_VALIDITY_DAYS} days)..."
    openssl req -new -x509 \
        -key "$ca_key" \
        -out "$ca_cert" \
        -days "$CA_VALIDITY_DAYS" \
        -config "$tmp_conf" \
        -extensions v3_ca

    validate_cert "$ca_cert" "$ca_key" "CA"
}

# generate_public_key derives the EC public key from the CA key.
#
# Tesla uses this key at .well-known/appspecific/com.tesla.3p.public-key.pem
# for app registration. It must come from the same EC key used as the CA key.
generate_public_key() {
    local ca_key="$CERTS_DIR/ca.key"
    local pub_file="$CERTS_DIR/public-key.pem"
    file_exists_guard "$pub_file" "Public key"

    log "Deriving public key from CA private key..."
    openssl ec -in "$ca_key" -pubout -out "$pub_file" 2>/dev/null
    validate_public_key "$pub_file"
}

# generate_server_cert creates an RSA 2048-bit server key and a CA-signed cert.
#
# RSA 2048 is used (not EC) because RSA is proven to work with Tesla vehicles
# for TLS handshake. The CA signs the cert so that Tesla can verify the chain
# using the ca.crt PEM supplied in the fleet_telemetry_config "ca" field.
generate_server_cert() {
    local ca_key="$CERTS_DIR/ca.key"
    local ca_cert="$CERTS_DIR/ca.crt"
    local server_key="$CERTS_DIR/server.key"
    local server_cert="$CERTS_DIR/server.crt"
    local server_csr

    if [[ ! -f "$ca_key" ]] || [[ ! -f "$ca_cert" ]]; then
        die "CA key/cert not found. Run without --client-only first to generate the CA."
    fi

    file_exists_guard "$server_key" "Server private key"
    file_exists_guard "$server_cert" "Server certificate"

    log "Generating RSA 2048-bit server private key..."
    openssl genrsa -out "$server_key" 2048 2>/dev/null
    chmod 600 "$server_key"
    validate_rsa_key "$server_key"

    # Build a temporary config for SAN and extensions.
    local tmp_conf
    tmp_conf="$(mktemp)"
    TEMP_FILES+=("$tmp_conf")

    cat > "$tmp_conf" <<EOF
[req]
prompt = no
distinguished_name = dn
req_extensions = v3_req

[dn]
CN = $DOMAIN

[v3_req]
subjectAltName = DNS:$DOMAIN

[v3_server]
basicConstraints = CA:FALSE
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:$DOMAIN
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid, issuer
EOF

    # Generate CSR.
    server_csr="$(mktemp)"
    TEMP_FILES+=("$server_csr")

    log "Generating server CSR..."
    openssl req -new \
        -key "$server_key" \
        -out "$server_csr" \
        -config "$tmp_conf"

    log "Signing server certificate with CA (valid $VALIDITY_DAYS days)..."
    openssl x509 -req \
        -in "$server_csr" \
        -CA "$ca_cert" \
        -CAkey "$ca_key" \
        -CAcreateserial \
        -out "$server_cert" \
        -days "$VALIDITY_DAYS" \
        -extfile "$tmp_conf" \
        -extensions v3_server 2>/dev/null

    validate_cert "$server_cert" "$server_key" "Server"
}

# generate_client_cert creates a self-signed EC client cert for local testing.
# This is used by the simulator and integration tests only — no CA involvement.
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
    log "Server cert validity: $VALIDITY_DAYS days"
    log "CA cert validity: $CA_VALIDITY_DAYS days (fixed)"
    log "Force overwrite: $FORCE"

    check_openssl
    check_curve_support

    mkdir -p "$CERTS_DIR"

    if [[ "$CLIENT_ONLY" == "true" ]]; then
        # Only generate client cert (assumes CA and server certs already exist).
        generate_client_cert
    else
        generate_ca
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
    log "3. In fleet_telemetry_config, set the 'ca' field to the contents of:"
    log "   $CERTS_DIR/ca.crt"
    log "   This is the CA cert that signed the server cert — Tesla uses it"
    log "   to verify the server during the mTLS handshake."
    log "4. Push fleet telemetry config:"
    log "   ./scripts/push-fleet-config.sh <VIN> <AUTH_TOKEN>"
    log "5. Vehicle owners pair virtual key at:"
    log "   https://tesla.com/_ak/$DOMAIN"
}

main "$@"
