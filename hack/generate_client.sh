#!/usr/bin/env bash
set -o errexit
set -o nounset
set -o pipefail

# Resolve the pinned module instead of rewriting/removing a developer's vendor
# directory. Generated output is built in isolation before copying into client/.
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
    echo "Missing kube_codegen.sh for the pinned code-generator module" >&2
    exit 1
fi

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT
mkdir -p "${TMP_DIR}/client" "${TMP_DIR}/bin"
# Respect a caller-provided tool directory, otherwise keep temporary installs
# isolated. Do not replace an unrelated developer's generator binaries.
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

# Replace generated directories only after generation succeeds, so removed APIs leave no stale output.
rm -rf "${SCRIPT_ROOT}/client/clientset" "${SCRIPT_ROOT}/client/informers" "${SCRIPT_ROOT}/client/listers"
cp -R "${TMP_DIR}/client/." "${SCRIPT_ROOT}/client/"
