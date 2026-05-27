#!/bin/bash

# generate-certificates.sh - Generate a private CA and a multi-domain TLS server certificate.
set -euo pipefail

# Default values
DOMAINS=()
DAYS=365
OUTPUT_DIR="."

show_help() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  -d, --domain DOMAIN     Add a base domain; may be specified multiple times"
    echo "                          DOMAIN and *.DOMAIN will both be added"
    echo "  -o, --output DIR        Specify output directory (default: .)"
    echo "  -D, --days DAYS         Specify certificate validity days (default: 365)"
    echo "  -h, --help              Show this help message"
    echo ""
    echo "Examples:"
    echo "  $0 -d example1.com -d example2.com"
    echo "  $0 --domain example1.com --domain example2.com --days 730"
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

mkdir -p "$OUTPUT_DIR"

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
        "$OUTPUT_DIR/signing.conf" \
        "$OUTPUT_DIR/index.txt" \
        "$OUTPUT_DIR/index.txt.attr" \
        "$OUTPUT_DIR/index.txt.old" \
        "$OUTPUT_DIR/serial" \
        "$OUTPUT_DIR/serial.old" \
        "$OUTPUT_DIR/ca.srl" \
        "$OUTPUT_DIR/.rnd" \
        "$OUTPUT_DIR/01.pem"
}

trap cleanup EXIT

echo "Generating TLS certificate for:"

for domain in "${DOMAINS[@]}"; do
    echo "  - ${domain}"
    echo "  - *.${domain}"
done

echo "Validity period: $DAYS days"
echo "Output directory: $OUTPUT_DIR"

# Generate CA private key
echo "Generating CA private key..."

openssl genrsa \
    -out "$OUTPUT_DIR/ca-privkey.pem" \
    2048

# Generate CA certificate configuration
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
    -key "$OUTPUT_DIR/ca-privkey.pem" \
    -config "$OUTPUT_DIR/ca.conf" \
    -extensions v3_ca \
    -days "$DAYS" \
    -out "$OUTPUT_DIR/ca-fullchain.pem"

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
dir = $OUTPUT_DIR
database = \$dir/index.txt
serial = \$dir/serial
private_key = $OUTPUT_DIR/ca-privkey.pem
certificate = $OUTPUT_DIR/ca-fullchain.pem
new_certs_dir = \$dir
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
} > "$OUTPUT_DIR/signing.conf"

# Initialize OpenSSL CA database
: > "$OUTPUT_DIR/index.txt"
echo "01" > "$OUTPUT_DIR/serial"

openssl ca \
    -config "$OUTPUT_DIR/signing.conf" \
    -policy policy_anything \
    -extensions server \
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
    "$OUTPUT_DIR/ca-fullchain.pem" \
    > "$OUTPUT_DIR/fullchain.pem"

# Set reasonable permissions
chmod 600 \
    "$OUTPUT_DIR/privkey.pem" \
    "$OUTPUT_DIR/ca-privkey.pem"

chmod 644 \
    "$OUTPUT_DIR/fullchain.pem" \
    "$OUTPUT_DIR/ca-fullchain.pem"

echo ""
echo "=========================================="
echo "TLS certificates generated successfully!"
echo "=========================================="
echo "Files created:"
echo "  Server private key:     $OUTPUT_DIR/privkey.pem"
echo "  Full certificate chain: $OUTPUT_DIR/fullchain.pem"
echo "  CA private key:         $OUTPUT_DIR/ca-privkey.pem"
echo "  CA certificate:         $OUTPUT_DIR/ca-fullchain.pem"
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