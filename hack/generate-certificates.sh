#!/bin/bash

# generate-certificates.sh - Generate TLS certificate files
set -e

# Default values
DOMAIN="your.domain.com"
DAYS=365
OUTPUT_DIR="."

# Show help information
show_help() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  -d, --domain DOMAIN     Specify certificate domain (default: your.domain.com)"
    echo "  -o, --output DIR        Specify output directory (default: .)"
    echo "  -D, --days DAYS         Specify certificate validity days (default: 365)"
    echo "  -h, --help              Show this help message"
    echo ""
    echo "Examples:"
    echo "  $0 -d myapp.your.domain.com"
    echo "  $0 --domain api.your.domain.com --days 730"
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        -d|--domain)
            DOMAIN="$2"
            shift 2
            ;;
        -o|--output)
            OUTPUT_DIR="$2"
            shift 2
            ;;
        -D|--days)
            DAYS="$2"
            shift 2
            ;;
        -h|--help)
            show_help
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            show_help
            exit 1
            ;;
    esac
done

echo "Generating TLS certificates for domain: $DOMAIN"
echo "Validity period: $DAYS days"
echo "Output directory: $OUTPUT_DIR"

# Generate CA private key
echo "Generating CA private key..."
openssl genrsa -out "$OUTPUT_DIR/ca-privkey.pem" 2048

# Generate CA certificate with proper config
echo "Generating CA certificate..."
cat > "$OUTPUT_DIR/ca.conf" << EOF
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
keyUsage = critical, digitalSignature, keyEncipherment, keyCertSign
EOF

openssl req -new -x509 -sha256 \
    -key "$OUTPUT_DIR/ca-privkey.pem" \
    -config "$OUTPUT_DIR/ca.conf" \
    -extensions v3_ca \
    -days $DAYS \
    -out "$OUTPUT_DIR/ca-fullchain.pem"

# Generate server private key
echo "Generating server private key..."
openssl genrsa -out "$OUTPUT_DIR/privkey.pem" 2048

# Generate certificate signing request
echo "Generating certificate signing request..."
cat > "$OUTPUT_DIR/cert.conf" << EOF
[req]
default_bits = 2048
prompt = no
default_md = sha256
distinguished_name = dn

[dn]
C = US
ST = California
L = San Francisco
O = Server Certificate
CN=${DOMAIN}

[v3_req]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names

[alt_names]
DNS.1 = ${DOMAIN}
DNS.2 = *.${DOMAIN}
EOF

# Generate server certificate and sign it
echo "Generating server certificate and signing with CA..."
openssl req -new -sha256 \
    -key "$OUTPUT_DIR/privkey.pem" \
    -config "$OUTPUT_DIR/cert.conf" \
    -reqexts v3_req \
    -out "$OUTPUT_DIR/server.csr"

# Sign server certificate with CA using proper config
echo "Signing server certificate with CA..."
cat > "$OUTPUT_DIR/signing.conf" << EOF
[ca]
default_ca = CA_default

[CA_default]
dir = .
database = \$dir/index.txt
serial = \$dir/serial
private_key = $OUTPUT_DIR/ca-privkey.pem
certificate = $OUTPUT_DIR/ca-fullchain.pem
new_certs_dir = \$dir
default_days = $DAYS
default_md = sha256
policy = policy_anything
copy_extensions = copy

[policy_anything]
countryName = optional
stateOrProvinceName = optional
localityName = optional
organizationName = optional
organizationalUnitName = optional
commonName = supplied
emailAddress = optional

[server]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names

[alt_names]
DNS.1 = ${DOMAIN}
DNS.2 = *.${DOMAIN}
EOF

# Initialize index file and serial number
touch "$OUTPUT_DIR/index.txt"
echo '01' > "$OUTPUT_DIR/serial"

# Sign the certificate using the ca command instead of x509 directly
openssl ca -config "$OUTPUT_DIR/signing.conf" \
    -policy policy_anything \
    -extensions server \
    -in "$OUTPUT_DIR/server.csr" \
    -out "$OUTPUT_DIR/server.crt" \
    -batch

# Combine server certificate and CA certificate to fullchain.pem
cat "$OUTPUT_DIR/server.crt" "$OUTPUT_DIR/ca-fullchain.pem" > "$OUTPUT_DIR/fullchain.pem"

# Clean up ALL temporary files, keeping only the four main PEM files
rm -f "$OUTPUT_DIR/server.csr" "$OUTPUT_DIR/server.crt" "$OUTPUT_DIR/cert.conf" "$OUTPUT_DIR/ca.conf" "$OUTPUT_DIR/signing.conf" "$OUTPUT_DIR/index.txt" "$OUTPUT_DIR/serial" "$OUTPUT_DIR/ca.srl" "$OUTPUT_DIR/.rnd"

echo ""
echo "=========================================="
echo "TLS certificates generated successfully!"
echo "=========================================="
echo "Files created:"
echo "  Server private key: $OUTPUT_DIR/privkey.pem"
echo "  Full certificate chain: $OUTPUT_DIR/fullchain.pem"
echo "  CA private key: $OUTPUT_DIR/ca-privkey.pem"
echo "  CA certificate: $OUTPUT_DIR/ca-fullchain.pem"
echo ""
echo "Certificate details:"
openssl x509 -in "$OUTPUT_DIR/fullchain.pem" -text -noout | head -20
rm -f "$OUTPUT_DIR/server.csr" "$OUTPUT_DIR/server.crt" "$OUTPUT_DIR/cert.conf" "$OUTPUT_DIR/ca.conf" "$OUTPUT_DIR/signing.conf" "$OUTPUT_DIR/index.txt" "$OUTPUT_DIR/index.txt.attr" "$OUTPUT_DIR/index.txt.old" "$OUTPUT_DIR/serial" "$OUTPUT_DIR/serial.old" "$OUTPUT_DIR/ca.srl" "$OUTPUT_DIR/.rnd" "$OUTPUT_DIR/01.pem"

