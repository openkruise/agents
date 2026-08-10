#!/bin/bash

# generate-certificates.sh - Generate a private CA and a multi-domain TLS server certificate.
set -euo pipefail

# Default values
DOMAINS=()
DAYS=365
OUTPUT_DIR="."
CA_KEY=""
CA_CERT=""

show_help() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  -d, --domain DOMAIN     Add a base domain; may be specified multiple times"
    echo "                          DOMAIN and *.DOMAIN will both be added"
    echo "  -o, --output DIR        Specify output directory (default: .)"
    echo "  -D, --days DAYS         Specify certificate validity days (default: 365)"
    echo "      --ca-key PATH       Reuse an existing CA private key"
    echo "      --ca-cert PATH      Reuse the matching existing CA certificate; it must"
    echo "                          be authorized to sign and valid for --days"
    echo "  -h, --help              Show this help message"
    echo ""
    echo "Examples:"
    echo "  $0 -d example1.com -d example2.com"
    echo "  $0 --domain example1.com --domain example2.com --days 730"
    echo "  $0 -d example.com --ca-key ca-key.pem --ca-cert ca-cert.pem"
}

add_domain() {
    local domain="$1"
    local existing

    # Remove a trailing dot, such as example.com.
    domain="${domain%.}"

    if [[ -z "$domain" || "$domain" == \*.* ]]; then
        echo "Invalid domain: $1" >&2
        echo "Pass the base domain, for example: -d example.com" >&2
        exit 1
    fi

    # Avoid duplicate domain entries.
    for existing in "${DOMAINS[@]:-}"; do
        if [[ "$existing" == "$domain" ]]; then
            return
        fi
    done

    DOMAINS+=("$domain")
}

# Parse command-line arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        -d|--domain)
            if [[ $# -lt 2 ]]; then
                echo "Missing value for $1" >&2
                exit 1
            fi

            add_domain "$2"
            shift 2
            ;;

        -o|--output)
            if [[ $# -lt 2 ]]; then
                echo "Missing value for $1" >&2
                exit 1
            fi

            OUTPUT_DIR="$2"
            shift 2
            ;;

        -D|--days)
            if [[ $# -lt 2 ]]; then
                echo "Missing value for $1" >&2
                exit 1
            fi

            DAYS="$2"
            shift 2
            ;;

        --ca-key)
            if [[ $# -lt 2 ]]; then
                echo "Missing value for $1" >&2
                exit 1
            fi

            CA_KEY="$2"
            shift 2
            ;;

        --ca-cert)
            if [[ $# -lt 2 ]]; then
                echo "Missing value for $1" >&2
                exit 1
            fi

            CA_CERT="$2"
            shift 2
            ;;

        -h|--help)
            show_help
            exit 0
            ;;

        *)
            echo "Unknown option: $1" >&2
            show_help
            exit 1
            ;;
    esac
done

# Keep the original default behavior when no domain is provided.
if [[ ${#DOMAINS[@]} -eq 0 ]]; then
    add_domain "your.domain.com"
fi

if ! [[ "$DAYS" =~ ^[1-9][0-9]*$ ]]; then
    echo "Invalid validity period: $DAYS" >&2
    exit 1
fi

if [[ -n "$CA_KEY" && -z "$CA_CERT" ]] || [[ -z "$CA_KEY" && -n "$CA_CERT" ]]; then
    echo "--ca-key and --ca-cert must be provided together" >&2
    exit 1
fi

REUSE_CA=false

if [[ -n "$CA_KEY" ]]; then
    REUSE_CA=true

    if [[ ! -f "$CA_KEY" || ! -r "$CA_KEY" ]]; then
        echo "CA private key is not a readable file: $CA_KEY" >&2
        exit 1
    fi

    if [[ ! -f "$CA_CERT" || ! -r "$CA_CERT" ]]; then
        echo "CA certificate is not a readable file: $CA_CERT" >&2
        exit 1
    fi

    if ! openssl x509 -in "$CA_CERT" -noout >/dev/null 2>&1; then
        echo "CA certificate is invalid: $CA_CERT" >&2
        exit 1
    fi

    if ! CA_BASIC_CONSTRAINTS="$(openssl x509 -in "$CA_CERT" -noout -ext basicConstraints 2>/dev/null)" ||
        [[ "$CA_BASIC_CONSTRAINTS" != *"CA:TRUE"* ]]; then
        echo "CA certificate cannot sign certificates: basicConstraints must contain CA:TRUE" >&2
        exit 1
    fi

    if ! CA_KEY_USAGE="$(openssl x509 -in "$CA_CERT" -noout -ext keyUsage 2>/dev/null)" ||
        [[ "$CA_KEY_USAGE" != *"Certificate Sign"* ]]; then
        echo "CA certificate cannot sign certificates: keyUsage must contain Certificate Sign" >&2
        exit 1
    fi

    VALIDITY_SECONDS=$((10#$DAYS * 24 * 60 * 60))

    if ! openssl x509 -in "$CA_CERT" -noout -checkend "$VALIDITY_SECONDS" >/dev/null 2>&1; then
        echo "CA certificate expires before the requested validity period of $DAYS days: $CA_CERT" >&2
        exit 1
    fi

    if ! cmp -s \
        <(openssl pkey -in "$CA_KEY" -pubout 2>/dev/null) \
        <(openssl x509 -in "$CA_CERT" -pubkey -noout 2>/dev/null); then
        echo "CA private key does not match CA certificate" >&2
        exit 1
    fi
fi

mkdir -p "$OUTPUT_DIR"

# Keep OpenSSL's per-signing database and issued-certificate copies isolated
# from the requested output directory. Each invocation gets a fresh database.
OPENSSL_WORK_DIR="$(mktemp -d)"
mkdir -p "$OPENSSL_WORK_DIR/newcerts"

# CN can only contain one domain.
# Modern clients validate Subject Alternative Name instead.
PRIMARY_DOMAIN="${DOMAINS[0]}"

# Dynamically output the OpenSSL alt_names section.
write_alt_names() {
    local index=1
    local domain

    for domain in "${DOMAINS[@]}"; do
        echo "DNS.${index} = ${domain}"
        index=$((index + 1))

        echo "DNS.${index} = *.${domain}"
        index=$((index + 1))
    done
}

cleanup() {
    rm -f \
        "$OUTPUT_DIR/server.csr" \
        "$OUTPUT_DIR/server.crt" \
        "$OUTPUT_DIR/server-clean.crt" \
        "$OUTPUT_DIR/cert.conf" \
        "$OUTPUT_DIR/ca.conf" \
        "$OUTPUT_DIR/ca.srl" \
        "$OUTPUT_DIR/.rnd" \
        "$OUTPUT_DIR/01.pem"
    rm -rf "$OPENSSL_WORK_DIR"
}

trap cleanup EXIT

echo "Generating TLS certificate for:"

for domain in "${DOMAINS[@]}"; do
    echo "  - ${domain}"
    echo "  - *.${domain}"
done

echo "Validity period: $DAYS days"
echo "Output directory: $OUTPUT_DIR"

# Generate a CA unless existing CA files were explicitly provided.
if [[ "$REUSE_CA" == false ]]; then
    CA_KEY="$OUTPUT_DIR/ca-privkey.pem"
    CA_CERT="$OUTPUT_DIR/ca-fullchain.pem"

    echo "Generating CA private key..."

    openssl genrsa \
        -out "$CA_KEY" \
        2048

    echo "Generating CA certificate..."

    cat > "$OUTPUT_DIR/ca.conf" <<EOF
[req]
default_bits = 2048
prompt = no
default_md = sha256
distinguished_name = dn
x509_extensions = v3_ca

[dn]
C = US
ST = California
L = San Francisco
O = Self-Signed CA
CN = Self-Signed CA

[v3_ca]
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid:always,issuer:always
basicConstraints = critical,CA:true
keyUsage = critical,digitalSignature,keyCertSign,cRLSign
EOF

    openssl req \
        -new \
        -x509 \
        -sha256 \
        -key "$CA_KEY" \
        -config "$OUTPUT_DIR/ca.conf" \
        -extensions v3_ca \
        -days "$DAYS" \
        -out "$CA_CERT"
else
    echo "Reusing CA private key: $CA_KEY"
    echo "Reusing CA certificate: $CA_CERT"
fi

# Generate server private key
echo "Generating server private key..."

openssl genrsa \
    -out "$OUTPUT_DIR/privkey.pem" \
    2048

# Generate CSR configuration
echo "Generating certificate signing request..."

{
    cat <<EOF
[req]
default_bits = 2048
prompt = no
default_md = sha256
distinguished_name = dn
req_extensions = v3_req

[dn]
C = US
ST = California
L = San Francisco
O = Server Certificate
CN = ${PRIMARY_DOMAIN}

[v3_req]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names

[alt_names]
EOF

    write_alt_names
} > "$OUTPUT_DIR/cert.conf"

openssl req \
    -new \
    -sha256 \
    -key "$OUTPUT_DIR/privkey.pem" \
    -config "$OUTPUT_DIR/cert.conf" \
    -out "$OUTPUT_DIR/server.csr"

# Generate signing configuration
echo "Signing server certificate with CA..."

{
    cat <<EOF
[ca]
default_ca = CA_default

[CA_default]
dir = $OPENSSL_WORK_DIR
database = \$dir/index.txt
serial = \$dir/serial
private_key = $CA_KEY
certificate = $CA_CERT
new_certs_dir = \$dir/newcerts
default_days = $DAYS
default_md = sha256
policy = policy_anything
copy_extensions = copy
unique_subject = no

[policy_anything]
countryName = optional
stateOrProvinceName = optional
localityName = optional
organizationName = optional
organizationalUnitName = optional
commonName = supplied
emailAddress = optional

[server]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names

[alt_names]
EOF

    write_alt_names
} > "$OPENSSL_WORK_DIR/signing.conf"

# Initialize OpenSSL CA database
: > "$OPENSSL_WORK_DIR/index.txt"

openssl ca \
    -config "$OPENSSL_WORK_DIR/signing.conf" \
    -policy policy_anything \
    -extensions server \
    -rand_serial \
    -in "$OUTPUT_DIR/server.csr" \
    -out "$OUTPUT_DIR/server.crt" \
    -batch

# Extract only the PEM certificate block
openssl x509 \
    -in "$OUTPUT_DIR/server.crt" \
    -out "$OUTPUT_DIR/server-clean.crt"

# Build the full certificate chain
cat \
    "$OUTPUT_DIR/server-clean.crt" \
    "$CA_CERT" \
    > "$OUTPUT_DIR/fullchain.pem"

# Set reasonable permissions
chmod 600 "$OUTPUT_DIR/privkey.pem"

chmod 644 "$OUTPUT_DIR/fullchain.pem"

if [[ "$REUSE_CA" == false ]]; then
    chmod 600 "$CA_KEY"
    chmod 644 "$CA_CERT"
fi

echo ""
echo "=========================================="
echo "TLS certificates generated successfully!"
echo "=========================================="
echo "Files created:"
echo "  Server private key:     $OUTPUT_DIR/privkey.pem"
echo "  Full certificate chain: $OUTPUT_DIR/fullchain.pem"

if [[ "$REUSE_CA" == true ]]; then
    echo "CA files reused:"
else
    echo "CA files created:"
fi

echo "  CA private key:         $CA_KEY"
echo "  CA certificate:         $CA_CERT"
echo ""
echo "Certificate subject:"

openssl x509 \
    -in "$OUTPUT_DIR/fullchain.pem" \
    -noout \
    -subject

echo ""
echo "Certificate SANs:"

openssl x509 \
    -in "$OUTPUT_DIR/fullchain.pem" \
    -noout \
    -ext subjectAltName
