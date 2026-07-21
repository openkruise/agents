#!/usr/bin/env bash
# Copyright (c) 2025 Alibaba Group Holding Ltd.

# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at

#      http://www.apache.org/licenses/LICENSE-2.0

# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

# Default values (prefer env vars when set, CLI args override both)
PLUGIN="conftest_base"
WITH_GATEWAY="false"
E2B_VERSION="${E2B_VERSION:-}"
SDK_VERSION="${SDK_VERSION:-}"
NO_PORT_FORWARD="false"
AUTH_DISABLED="false"
REPEAT=""
PYTEST_KEYWORD_EXPR=""
PYTEST_MARKER_EXPR=""
PYTEST_EXTRA_ARGS=""
TEST_FILES=()
MANIFEST_GLOBS=()
RENDERED_DIR=""
PORT_FORWARD_PID=""

# Get the directory where this script is located
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
TEST_DIR="$PROJECT_ROOT/test/e2b"
MANAGER_SELECTOR="app.kubernetes.io/name=sandbox-manager"

E2B_SDK_COMPAT_MIN_VERSION="2.25.0"

# ============================================================================
# Argument parsing
# ============================================================================

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --plugin)         PLUGIN="$2";             shift 2 ;;
            --e2b-version)    E2B_VERSION="$2";        shift 2 ;;
            --sdk-version)    SDK_VERSION="$2";        shift 2 ;;
            --with-gateway)   WITH_GATEWAY="true";     shift ;;
            --no-port-forward) NO_PORT_FORWARD="true"; shift ;;
            --manifests)      MANIFEST_GLOBS+=("$2");  shift 2 ;;
            --repeat)         REPEAT="$2";             shift 2 ;;
            -k)               PYTEST_KEYWORD_EXPR="$2"; shift 2 ;;
            -m)               PYTEST_MARKER_EXPR="$2"; shift 2 ;;
            --test-file)      TEST_FILES+=("$2");      shift 2 ;;
            --pytest-extra-args) PYTEST_EXTRA_ARGS="$2"; shift 2 ;;
            --auth-disabled)  AUTH_DISABLED="true"; shift ;;
            *)
                echo "Unknown parameter: $1" >&2
                echo "Usage: $0 [--plugin P] [--e2b-version V] [--sdk-version V] [--with-gateway] [--no-port-forward] [--manifests GLOB]... [--test-file PATH]... [--repeat N] [-k EXPR] [-m MARKER_EXPR] [--pytest-extra-args FLAGS]" >&2
                exit 1 ;;
        esac
    done

    if [[ ${#MANIFEST_GLOBS[@]} -eq 0 ]]; then
        MANIFEST_GLOBS=("$TEST_DIR/assets/*.yaml")
    fi
}

# ============================================================================
# Shared functions
# ============================================================================

wait_for_manager() {
    echo "Waiting for sandbox-manager pods to appear and become ready (up to 10 min)..."
    local pod_count i

    for i in $(seq 1 60); do
        pod_count=$(kubectl get pod -l "${MANAGER_SELECTOR}" \
            -n sandbox-system --no-headers 2>/dev/null | wc -l | tr -d ' ')
        if [[ "$pod_count" -gt 0 ]]; then
            echo "Found $pod_count sandbox-manager pod(s), waiting for readiness..."
            if kubectl wait --for=condition=ready pod \
                    -l "${MANAGER_SELECTOR}" \
                    -n sandbox-system \
                    --timeout=5m; then
                echo "All sandbox-manager pods are ready"
                return 0
            fi
            break
        fi
        echo "No sandbox-manager pods yet (attempt ${i}/60)"
        sleep 10
    done

    echo "ERROR: sandbox-manager pods not ready" >&2
    echo "=== Pod Status ==="
    kubectl get pod -l "${MANAGER_SELECTOR}" -n sandbox-system -o wide || true
    echo "=== All resources in sandbox-system ==="
    kubectl get all -n sandbox-system || true
    echo "=== Pod Describe ==="
    kubectl describe pod -l "${MANAGER_SELECTOR}" -n sandbox-system || true
    echo "=== Pod Logs ==="
    local pod
    for pod in $(kubectl get pod -l "${MANAGER_SELECTOR}" -n sandbox-system \
                    --no-headers -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
        echo "--- Logs for $pod (controller, tail=200) ---"
        kubectl logs "$pod" -n sandbox-system -c controller --tail=200 2>&1 || true
        echo "--- Previous controller logs for $pod (tail=200) ---"
        kubectl logs "$pod" -n sandbox-system -c controller --previous --tail=200 2>&1 || true
    done
    return 1
}

wait_for_gateway() {
    echo "Waiting for sandbox-gateway pods to be ready..."
    kubectl wait --for=condition=ready pod \
        -l app.kubernetes.io/name=sandbox-gateway \
        -n sandbox-system \
        --timeout=5m
    echo "All sandbox-gateway pods are ready"
}

install_deps() {
    echo "Installing dependencies..."
    pip install -r "$TEST_DIR/requirements.txt"
    if [[ -n "$E2B_VERSION" ]]; then
        pip install "e2b==${E2B_VERSION}"
    fi
    if [[ -n "$SDK_VERSION" ]]; then
        pip install "e2b-code-interpreter==${SDK_VERSION}"
    else
        pip install e2b-code-interpreter
    fi
    echo "Dependencies installed"
}

check_manager_restart() {
    local restart_count
    restart_count=$(kubectl get pod -n sandbox-system \
        -l "${MANAGER_SELECTOR}" -o jsonpath='{range .items[*]}{.status.containerStatuses[*].restartCount}{" "}{end}' 2>/dev/null \
        | awk '{for(i=1;i<=NF;i++) s+=$i} END {print s+0}')

    if [[ "$restart_count" -eq 0 ]]; then
        echo "sandbox-manager has not restarted"
    else
        kubectl get pod -n sandbox-system --no-headers -l "${MANAGER_SELECTOR}"
        echo "ERROR: sandbox-manager has restarted" >&2
        kubectl get pod -n sandbox-system -l "${MANAGER_SELECTOR}" --no-headers \
            | awk '{print $1}' \
            | xargs -I {} kubectl logs -p -n sandbox-system -c controller {}
        exit 1
    fi
}

check_gateway_restart() {
    local gw_restart_count
    gw_restart_count=$(kubectl get pod -n sandbox-system \
        -l app.kubernetes.io/name=sandbox-gateway -o jsonpath='{range .items[*]}{.status.containerStatuses[*].restartCount}{" "}{end}' 2>/dev/null \
        | awk '{for(i=1;i<=NF;i++) s+=$i} END {print s+0}')

    if [[ "$gw_restart_count" -eq 0 ]]; then
        echo "sandbox-gateway has not restarted"
    else
        kubectl get pod -n sandbox-system --no-headers -l app.kubernetes.io/name=sandbox-gateway
        echo "ERROR: sandbox-gateway has restarted" >&2
        kubectl get pod -n sandbox-system -l app.kubernetes.io/name=sandbox-gateway --no-headers \
            | awk '{print $1}' \
            | xargs -I {} kubectl logs -p -n sandbox-system {}
        exit 1
    fi
}

# ============================================================================
# SDK compat (API key conversion for e2b >= 2.25.0)
# ============================================================================

version_part_to_number() {
    local value="$1"
    value="${value#v}"
    value="${value%%[!0-9]*}"
    if [ -z "$value" ]; then
        value=0
    fi
    echo "$value"
}

version_ge() {
    local version="$1"
    local minimum="$2"
    local major minor patch min_major min_minor min_patch

    version="${version#v}"
    minimum="${minimum#v}"
    IFS=. read -r major minor patch _ <<<"$version"
    IFS=. read -r min_major min_minor min_patch _ <<<"$minimum"

    major="$(version_part_to_number "$major")"
    minor="$(version_part_to_number "$minor")"
    patch="$(version_part_to_number "$patch")"
    min_major="$(version_part_to_number "$min_major")"
    min_minor="$(version_part_to_number "$min_minor")"
    min_patch="$(version_part_to_number "$min_patch")"

    if ((10#$major > 10#$min_major)); then
        return 0
    fi
    if ((10#$major < 10#$min_major)); then
        return 1
    fi
    if ((10#$minor > 10#$min_minor)); then
        return 0
    fi
    if ((10#$minor < 10#$min_minor)); then
        return 1
    fi
    ((10#$patch >= 10#$min_patch))
}

get_installed_e2b_version() {
    pip show e2b 2>/dev/null | awk '/^Version:/ {print $2}'
}

convert_e2b_api_key_for_sdk_if_needed() {
    local installed_e2b_version="$1"
    local api_url response compatible_api_key xtrace_enabled

    if ! version_ge "$installed_e2b_version" "$E2B_SDK_COMPAT_MIN_VERSION"; then
        echo "Installed e2b version $installed_e2b_version does not require SDK-compatible API key conversion"
        return
    fi

    echo "Converting E2B_API_KEY for e2b $installed_e2b_version SDK validation..."
    if [[ $- == *x* ]]; then
        xtrace_enabled="true"
        set +x
    else
        xtrace_enabled="false"
    fi

    if [ -z "${E2B_API_KEY:-}" ]; then
        echo "Error: E2B_API_KEY must be set for e2b >= $E2B_SDK_COMPAT_MIN_VERSION"
        exit 1
    fi

    api_url="${E2B_API_URL:-http://${E2B_DOMAIN:-localhost}/kruise/api}"
    api_url="${api_url%/}"
    response="$(
        curl --fail --silent --show-error \
            --retry 30 --retry-delay 1 --retry-connrefused \
            --connect-timeout 5 --max-time 10 \
            --header "X-API-Key: ${E2B_API_KEY}" \
            "${api_url}/api-keys/compatible"
    )"
    compatible_api_key="$(
        printf '%s' "$response" | python3 -c 'import json, sys
data = json.load(sys.stdin)
key = data.get("key")
if not isinstance(key, str) or not key:
    raise SystemExit("compatible API key response does not contain a non-empty key")
print(key)'
    )"
    export E2B_API_KEY="$compatible_api_key"

    if [ "$xtrace_enabled" = "true" ]; then
        set -x
    fi
    echo "E2B_API_KEY converted to SDK-compatible form"
}

# ============================================================================
# Manifest rendering
# ============================================================================

render_and_apply_manifests() {
    RENDERED_DIR=$(mktemp -d)
    local glob matched_any="false"

    for glob in "${MANIFEST_GLOBS[@]}"; do
        local files=()
        # Use compgen to safely expand glob without errors on no-match.
        while IFS= read -r f; do
            files+=("$f")
        done < <(compgen -G "$glob" || true)

        if [[ ${#files[@]} -eq 0 ]]; then
            echo "Warning: no files matched glob: $glob" >&2
            continue
        fi

        for template in "${files[@]}"; do
            local basename
            basename="$(basename "$template")"
            echo "Rendering template: $template → $RENDERED_DIR/$basename"
            envsubst < "$template" > "$RENDERED_DIR/$basename"
            matched_any="true"
        done
    done

    if [[ "$matched_any" == "false" ]]; then
        echo "Warning: no manifest templates matched any glob, skipping kubectl apply" >&2
        return 0
    fi

    echo "Applying rendered manifests from $RENDERED_DIR..."
    kubectl apply -f "$RENDERED_DIR"
    echo "Manifests applied"
}

# ============================================================================
# Cleanup (EXIT trap)
# ============================================================================

# shellcheck disable=SC2329
cleanup() {
    local exit_code=$?
    set +e

    if [[ -n "$PORT_FORWARD_PID" ]]; then
        kill "$PORT_FORWARD_PID" 2>/dev/null || true
    fi

    if [[ "${E2E_DEBUG:-}" == "true" ]]; then
        echo "E2E_DEBUG is set — skipping manifest cleanup. Rendered manifests in ${RENDERED_DIR:-<none>}"
        exit "$exit_code"
    fi

    if [[ -n "$RENDERED_DIR" ]] && [[ -d "$RENDERED_DIR" ]]; then
        echo "Cleaning up rendered manifests..."
        kubectl delete -f "$RENDERED_DIR" --ignore-not-found 2>/dev/null || true
        rm -rf "$RENDERED_DIR"
    fi

    exit "$exit_code"
}

# ============================================================================
# Main
# ============================================================================

parse_args "$@"
trap cleanup EXIT

export KUBECONFIG="${KUBECONFIG:-${HOME}/.kube/config}"

# Step 1: Wait for sandbox-manager
wait_for_manager

# Step 2: Port-forward (unless --no-port-forward)
if [[ "$NO_PORT_FORWARD" != "true" ]]; then
    if [[ "$WITH_GATEWAY" == "true" ]]; then
        wait_for_gateway
        # Port-forward gateway as unified entry point (80 -> 7788, which targets Envoy :10000)
        sudo -E kubectl port-forward svc/sandbox-gateway 80:7788 -n sandbox-system &
        PORT_FORWARD_PID=$!
    else
        # Port-forward sandbox-manager directly
        sudo -E kubectl port-forward svc/sandbox-manager 80:7788 -n sandbox-system &
        PORT_FORWARD_PID=$!
    fi
fi

# Step 3: Install Python deps
install_deps

# Enable tracing early for debugging
set -x

# Step 4: SDK API key conversion
installed_e2b_version="$(get_installed_e2b_version)"
if [ -z "$installed_e2b_version" ]; then
    echo "Error: failed to determine installed e2b version"
    exit 1
fi
echo "Detected e2b version: $installed_e2b_version"
if [[ "$AUTH_DISABLED" == "true" ]]; then
    echo "sandbox-manager E2B API auth is disabled; skipping API key compatibility conversion"
    export E2B_API_KEY="${E2B_API_KEY:-e2b_abc123}"
else
    convert_e2b_api_key_for_sdk_if_needed "$installed_e2b_version"
fi

# Step 5: Render and apply K8s manifests
render_and_apply_manifests
sleep 5

# Step 6: Run pytest (print the key for debug, it's safe to print it in a ci pipeline)
echo "Using E2B_API_KEY: ${E2B_API_KEY:-}"
echo "Running E2B pytest tests..."

# pytest -p imports the plugin before test collection adds TEST_DIR to sys.path
export PYTHONPATH="${TEST_DIR}:${PYTHONPATH:-}"

# Base pytest arguments: plugin loader + selection filters + CI-only artifacts.
# Shared test-suite defaults (verbosity, traceback, timeout) live in
# test/e2b/pytest.ini (addopts) so they apply to any pytest invocation,
# including community users running locally. This script layers on only
# the CI-specific artifacts (junit XML output), plus E2E_DEBUG overrides
# (e.g. -vv, --tb=long, --showlocals) injected
# by the caller via --pytest-extra-args — the script itself does not
# know about E2E_DEBUG, keeping it reusable by both community and
# internal CI.
#
# Rerun behavior is controlled entirely by `@pytest.mark.flaky(reruns=N)`
# on individual tests — there is no global --reruns flag. This keeps the
# retry policy close to the code that knows which cases are flaky.
pytest_args=(-p "$PLUGIN")
if [[ -n "$REPEAT" ]]; then pytest_args+=(--count "$REPEAT"); fi
if [[ -n "$PYTEST_KEYWORD_EXPR" ]]; then pytest_args+=(-k "$PYTEST_KEYWORD_EXPR"); fi
if [[ -n "$PYTEST_MARKER_EXPR" ]]; then pytest_args+=(-m "$PYTEST_MARKER_EXPR"); fi
if [[ "$WITH_GATEWAY" != "true" ]]; then
    pytest_args+=(--ignore="$TEST_DIR/test_gateway.py")
    pytest_args+=(--ignore="$TEST_DIR/test_gateway_auth.py")
    pytest_args+=(--ignore="$TEST_DIR/test_gateway_jwt_auth.py")
    pytest_args+=(--ignore="$TEST_DIR/test_gateway_runtime_mtls.py")
elif [[ -z "$PYTEST_MARKER_EXPR" ]]; then
    # The default gateway deployment has authentication and Runtime mTLS disabled.
    pytest_args+=(-m "not gateway_uuid_auth and not jwt_auth and not runtime_mtls")
fi
if [[ "$AUTH_DISABLED" == "true" ]]; then pytest_args+=(--ignore="$TEST_DIR/test_apikey.py"); fi

# CI-only: write JUnit XML for the CI system to parse, and surface each
# failure live (traceback + captured stdout/stderr/log) as it happens so
# CI task logs show real-time diagnosis instead of making us wait
# until pytest exits. --instafail does NOT abort the run; subsequent
# tests still execute, and the junit.xml is still produced at the end.
# Not in pytest.ini because local pytest runs usually want the default
# end-of-run summary, not streamed failures.
ARTIFACTS_DIR="${ARTIFACTS_DIR:-${TEST_DIR}/reports}"
mkdir -p "$ARTIFACTS_DIR"
pytest_args+=(
    --junitxml="${ARTIFACTS_DIR}/junit.xml"
    --instafail
)

# shellcheck disable=SC2086,SC2206
if [[ -n "$PYTEST_EXTRA_ARGS" ]]; then pytest_args+=($PYTEST_EXTRA_ARGS); fi

set +e
pytest_targets=("$TEST_DIR")
if [[ ${#TEST_FILES[@]} -gt 0 ]]; then
    pytest_targets=("${TEST_FILES[@]}")
fi
pytest "${pytest_args[@]}" "${pytest_targets[@]}"
retVal=$?
set -e

if [ "$retVal" -ne 0 ]; then
    echo "Tests failed"
else
    echo "All E2B tests passed successfully!"
fi

# Step 7: Check for sandbox-manager restarts
check_manager_restart

# Step 8: Check gateway restarts (if applicable)
if [[ "$WITH_GATEWAY" == "true" ]]; then
    check_gateway_restart
fi

exit "$retVal"
