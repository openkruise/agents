#!/usr/bin/env bash
set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${SCRIPT_ROOT}"
if [[ -z "${CODEGEN_PKG:-}" ]]; then
    CODEGEN_PKG="$(go list -mod=readonly -m -f '{{.Dir}}' k8s.io/code-generator)"
    if [[ -z "${CODEGEN_PKG}" ]]; then
        go mod download k8s.io/code-generator
        CODEGEN_PKG="$(go list -mod=readonly -m -f '{{.Dir}}' k8s.io/code-generator)"
    fi
fi
if [[ ! -f "${CODEGEN_PKG}/kube_codegen.sh" ]]; then
    printf '%s\n' "Missing kube_codegen.sh for the pinned code-generator module" >&2
    exit 1
fi

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT
mkdir -p "${TMP_DIR}/client" "${TMP_DIR}/bin"
export GOBIN="${GOBIN:-${TMP_DIR}/bin}"
source "${CODEGEN_PKG}/kube_codegen.sh"

GO111MODULE=on kube::codegen::gen_client \
    --with-watch \
    --one-input-api api \
    --plural-exceptions "SandboxUpdateOps:sandboxupdateops" \
    --output-dir "${TMP_DIR}/client" \
    --output-pkg "github.com/openkruise/agents/client" \
    --boilerplate "${SCRIPT_ROOT}/hack/boilerplate.go.txt" \
    "${SCRIPT_ROOT}"

# Publish only complete generated output, without modifying a developer's vendor directory.
rm -rf "${SCRIPT_ROOT}/client/clientset" "${SCRIPT_ROOT}/client/informers" "${SCRIPT_ROOT}/client/listers"
cp -R "${TMP_DIR}/client/." "${SCRIPT_ROOT}/client/"
