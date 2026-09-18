#!/usr/bin/env bash
# Copyright 2026 The Kruise Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

# Do not enable xtrace: this script handles certificate material.

NS="${PEER_MTLS_NAMESPACE:-sandbox-system}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ASSETS_DIR="$PROJECT_ROOT/test/e2b/assets/peer-mtls"
RUNTIME_NAME="agentruntime.sandbox.agents.kruise.io"
FIXTURE_DIR="${PEER_MTLS_FIXTURE_DIR:-/tmp/peer-mtls-e2e}"

usage() {
    echo "Usage: $0 secrets|junit [junit.xml]" >&2
    exit 1
}

issue_leaf() {
    local dir="$1" name="$2" ca_crt="$3" ca_key="$4" ext="$5"
    openssl req -newkey rsa:2048 -nodes \
        -keyout "$dir/${name}.key" -out "$dir/${name}.csr" \
        -subj "/CN=${name}" >/dev/null 2>&1
    openssl x509 -req -days 2 -sha256 \
        -in "$dir/${name}.csr" -CA "$ca_crt" -CAkey "$ca_key" -CAcreateserial \
        -out "$dir/${name}.crt" -extfile "$ext" >/dev/null 2>&1
}

create_tls_secret() {
    local name="$1" crt="$2" key="$3" ca="$4"
    kubectl create secret generic "$name" -n "$NS" \
        --from-file=tls.crt="$crt" \
        --from-file=tls.key="$key" \
        --from-file=ca.crt="$ca" \
        --dry-run=client -o yaml | kubectl apply -f - >/dev/null
}

create_client_secret() {
    local name="$1" crt="$2" key="$3" ca="$4"
    kubectl create secret generic "$name" -n "$NS" \
        --from-file=client.crt="$crt" \
        --from-file=client.key="$key" \
        --from-file=ca.crt="$ca" \
        --dry-run=client -o yaml | kubectl apply -f - >/dev/null
}

cmd_secrets() {
    local cert_dir
    cert_dir="$(mktemp -d)"
    chmod 700 "$cert_dir"
    # EXIT must not capture the local: after this function returns, a trap
    # referencing cert_dir trips set -u ("unbound variable") at script exit.
    # RETURN also does not run when set -e aborts the function.
    _peer_mtls_cert_dir="$cert_dir"
    trap 'rm -rf "${_peer_mtls_cert_dir:-}"' EXIT
    umask 077

    openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
        -keyout "$cert_dir/ca-a.key" -out "$cert_dir/ca-a.crt" \
        -subj "/CN=peer-mtls-e2e-ca-a" >/dev/null 2>&1
    openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
        -keyout "$cert_dir/ca-b.key" -out "$cert_dir/ca-b.crt" \
        -subj "/CN=peer-mtls-e2e-ca-b" >/dev/null 2>&1

    printf 'subjectAltName=DNS:%s\nextendedKeyUsage=serverAuth\n' "$RUNTIME_NAME" >"$cert_dir/server.ext"
    printf 'extendedKeyUsage=clientAuth\n' >"$cert_dir/client.ext"

    issue_leaf "$cert_dir" server "$cert_dir/ca-a.crt" "$cert_dir/ca-a.key" "$cert_dir/server.ext"
    issue_leaf "$cert_dir" manager-client "$cert_dir/ca-a.crt" "$cert_dir/ca-a.key" "$cert_dir/client.ext"
    issue_leaf "$cert_dir" gateway-client "$cert_dir/ca-a.crt" "$cert_dir/ca-a.key" "$cert_dir/client.ext"
    issue_leaf "$cert_dir" untrusted-client "$cert_dir/ca-b.crt" "$cert_dir/ca-b.key" "$cert_dir/client.ext"

    create_tls_secret peer-mtls-server "$cert_dir/server.crt" "$cert_dir/server.key" "$cert_dir/ca-a.crt"
    create_client_secret peer-mtls-manager-client "$cert_dir/manager-client.crt" "$cert_dir/manager-client.key" "$cert_dir/ca-a.crt"
    create_client_secret peer-mtls-gateway-client "$cert_dir/gateway-client.crt" "$cert_dir/gateway-client.key" "$cert_dir/ca-a.crt"

    kubectl apply -f "$ASSETS_DIR/rbac.yaml" >/dev/null

    mkdir -p "$FIXTURE_DIR"
    chmod 700 "$FIXTURE_DIR"
    cp "$cert_dir/ca-a.crt" "$FIXTURE_DIR/ca-a.crt"
    # Untrusted and server-as-client material stays on the runner for curl.
    cp "$cert_dir/untrusted-client.crt" "$FIXTURE_DIR/untrusted.crt"
    cp "$cert_dir/untrusted-client.key" "$FIXTURE_DIR/untrusted.key"
    cp "$cert_dir/server.crt" "$FIXTURE_DIR/server.crt"
    cp "$cert_dir/server.key" "$FIXTURE_DIR/server.key"
    echo "peer-mtls secrets created"
}

cmd_junit() {
    local xml="${1:-test/e2b/reports/junit.xml}"
    python3 - "$xml" <<'PY'
import sys
import xml.etree.ElementTree as ET

tree = ET.parse(sys.argv[1])
for suite in tree.iter("testsuite"):
    print(
        "tests=%s failures=%s errors=%s skipped=%s"
        % (
            suite.attrib.get("tests"),
            suite.attrib.get("failures"),
            suite.attrib.get("errors"),
            suite.attrib.get("skipped"),
        )
    )
    for case in suite.iter("testcase"):
        status = "passed"
        if case.find("skipped") is not None:
            status = "skipped"
        elif case.find("failure") is not None:
            status = "failure"
        elif case.find("error") is not None:
            status = "error"
        print("%s %s::%s" % (status, case.attrib.get("classname", ""), case.attrib.get("name", "")))
PY
}

case "${1:-}" in
    secrets) cmd_secrets ;;
    junit) shift; cmd_junit "$@" ;;
    *) usage ;;
esac
