#!/usr/bin/env bash
set -o errexit
set -o nounset
set -o pipefail

# Generates clientset, listers and informers in module mode: the code
# generators are pinned as tool directives in tools.mod and run via
# `go run -modfile=tools.mod`, so no `go install`, GOBIN, or vendored
# GOPATH flow is involved.

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${SCRIPT_ROOT}"

BOILERPLATE="${SCRIPT_ROOT}/hack/boilerplate.go.txt"
OUTPUT_PKG="github.com/openkruise/agents/client"
PLURAL_EXCEPTIONS="SandboxUpdateOps:sandboxupdateops"
V="${KUBE_VERBOSE:-0}"

# Discover API packages requesting clients: directories under api/ whose Go
# files carry a +genclient tag and whose leaf directory is a version (e.g.
# v1alpha1). This mirrors the discovery logic of kube_codegen.sh's gen_client.
input_pkgs=()
while read -r dir; do
    pkg="$(cd "${dir}" && go list -find .)"
    input_pkgs+=("${pkg}")
done < <(
    ( grep -l --null \
        -e '^\s*//\s*+genclient' \
        -r "${SCRIPT_ROOT}/api" \
        --include '*.go' \
        --exclude-dir .git \
        --exclude-dir _output \
        --exclude-dir vendor \
        || true \
    ) | while read -r -d $'\0' F; do dirname "${F}"; done \
      | LC_ALL=C sort -u \
      | while read -r dir; do
            leaf="$(basename "${dir}")"
            if grep -E -q '^v[0-9]+((alpha|beta)[0-9]+)?$' <<< "${leaf}"; then
                echo "${dir}"
            fi
        done
)

if [ "${#input_pkgs[@]}" == 0 ]; then
    printf '%s\n' "No +genclient tagged APIs found under ${SCRIPT_ROOT}/api" >&2
    exit 1
fi

inputs=()
for pkg in "${input_pkgs[@]}"; do
    inputs+=(--input "${pkg}")
done

# Generate into a staging directory and publish only complete output.
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

echo "Generating client code for ${#input_pkgs[@]} targets"
go run -modfile=tools.mod k8s.io/code-generator/cmd/client-gen \
    -v "${V}" \
    --go-header-file "${BOILERPLATE}" \
    --output-dir "${TMP_DIR}/clientset" \
    --output-pkg "${OUTPUT_PKG}/clientset" \
    --clientset-name "versioned" \
    --apply-configuration-package "" \
    --input-base "" \
    --plural-exceptions "${PLURAL_EXCEPTIONS}" \
    --prefers-protobuf=false \
    "${inputs[@]}"

echo "Generating lister code for ${#input_pkgs[@]} targets"
go run -modfile=tools.mod k8s.io/code-generator/cmd/lister-gen \
    -v "${V}" \
    --go-header-file "${BOILERPLATE}" \
    --output-dir "${TMP_DIR}/listers" \
    --output-pkg "${OUTPUT_PKG}/listers" \
    --plural-exceptions "${PLURAL_EXCEPTIONS}" \
    "${input_pkgs[@]}"

echo "Generating informer code for ${#input_pkgs[@]} targets"
go run -modfile=tools.mod k8s.io/code-generator/cmd/informer-gen \
    -v "${V}" \
    --go-header-file "${BOILERPLATE}" \
    --output-dir "${TMP_DIR}/informers" \
    --output-pkg "${OUTPUT_PKG}/informers" \
    --versioned-clientset-package "${OUTPUT_PKG}/clientset/versioned" \
    --listers-package "${OUTPUT_PKG}/listers" \
    --plural-exceptions "${PLURAL_EXCEPTIONS}" \
    "${input_pkgs[@]}"

rm -rf "${SCRIPT_ROOT}/client/clientset" "${SCRIPT_ROOT}/client/informers" "${SCRIPT_ROOT}/client/listers"
cp -R "${TMP_DIR}/clientset" "${TMP_DIR}/listers" "${TMP_DIR}/informers" "${SCRIPT_ROOT}/client/"
