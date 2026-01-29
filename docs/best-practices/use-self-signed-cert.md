# Best Practice: Accessing sandbox-manager via E2B Protocol with Self-Signed Certificates

This document provides a best practice for accessing sandbox-manager via self-signed certificates, including how to
create certificates through an optimized script, how to install certificates in the cluster, and how to configure the
E2B client.

## Prerequisites

1. `sandbox-manager` is installed in the cluster
2. Ensure the `sandbox-manager` ingress is active (has an entrypoint). You can confirm this using the following command.
   The Address column should not be empty:

```shell
$ kubectl get ingress sandbox-manager -n sandbox-system
```

## Step 1: Create Certificates

Create self-signed certificates using the script [generate-certificate.sh](../../hack/generate-certificates.sh). You can
view the script usage with the following command.

```shell
$ bash generate-certificates.sh --help

Usage: generate-certificates.sh [OPTIONS]

Options:
  -d, --domain DOMAIN     Specify certificate domain (default: your.domain.com)
  -o, --output DIR        Specify output directory (default: .)
  -D, --days DAYS         Specify certificate validity days (default: 365)
  -h, --help              Show this help message

Examples:
  generate-certificates.sh -d myapp.your.domain.com
  generate-certificates.sh --domain api.your.domain.com --days 730
```

After completing certificate generation, you will obtain the following files:

- fullchain.pem: Server certificate public key
- privkey.pem: Server certificate private key
- ca-fullchain.pem: CA certificate public key
- ca-privkey.pem: CA certificate private key

This script generates both single-domain (your.domain) and wildcard (*.your.domain) certificates, compatible with both
native E2B protocol and OpenKruise customized E2B protocol.

## Step 2: Install Certificates

Mount the server certificate to the cluster's Ingress using the following command:

```shell
$ kubectl create secret tls sandbox-manager-tls \
          --cert=fullchain.pem \
          --key=privkey.pem -n sandbox-system
```

Certificate activation may have some delay, depending on your ingress controller's efficiency.

## Step 3: Configure Client Trust

The client needs to set the environment variable `SSL_CERT_FILE` to the path of the CA public key file (
ca-fullchain.pem) generated in Step 1:

```shell
$ export SSL_CERT_FILE=/path/to/ca-fullchain.pem

# If you need to access via single domain using the customized SDK, you also need to configure the following environment variable
$ export REQUESTS_CA_BUNDLE=/path/to/ca-fullchain.pem
```