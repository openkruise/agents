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

set -e

# Default values
TIMEOUT="60m"
WITH_GATEWAY="false"
AUTH_DISABLED="false"

# Get the directory where this script is located
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
TEST_DIR="$PROJECT_ROOT/test/e2b"
MANAGER_SELECTOR="app.kubernetes.io/name=sandbox-manager"
E2B_SDK_COMPAT_MIN_VERSION="2.25.0"

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
    pip show e2b 2>/dev/null | awk '/^Version:/ {print $2; exit}'
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

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    key="$1"
    case $key in
        --e2b-version)
            E2B_VERSION="$2"
            shift # past argument
            shift # past value
            ;;
        --timeout)
            TIMEOUT="$2"
            shift # past argument
            shift # past value
            ;;
        --sdk-version)
            SDK_VERSION="$2"
            shift # past argument
            shift # past value
            ;;
        --with-gateway)
            WITH_GATEWAY="true"
            shift # past argument
            ;;
        --auth-disabled)
            AUTH_DISABLED="true"
            shift # past argument
            ;;
        *)
            echo "Unknown parameter passed: $1"
            echo "Usage: $0 [--e2b-version VERSION] [--sdk-version VERSION] [--timeout TIMEOUT] [--with-gateway] [--auth-disabled]"
            exit 1
            ;;
    esac
done

config=${KUBECONFIG:-${HOME}/.kube/config}
export KUBECONFIG=${config}

set -x

# Step 1: Wait for sandbox-manager pods to be ready
echo "Waiting for sandbox-manager pods to be ready..."

if ! kubectl wait --for=condition=ready pod \
        -l component=sandbox-manager \
        -n sandbox-system \
        --timeout=5m; then
    echo "Error: Failed to wait for sandbox-manager pods to be ready"
    echo "\n=== Pod Status ==="
    kubectl get pod -l component=sandbox-manager -n sandbox-system -o wide
    echo "\n=== Pod Describe ==="
    kubectl describe pod -l component=sandbox-manager -n sandbox-system
    echo "\n=== Pod Logs ==="
    for pod in $(kubectl get pod -l component=sandbox-manager -n sandbox-system --no-headers -o jsonpath='{.items[*].metadata.name}'); do
        echo "--- Logs for $pod ---"
        kubectl logs "$pod" -n sandbox-system -c controller --tail=200 2>&1 || echo "Failed to get current controller logs for $pod"
        echo "--- Previous controller logs for $pod ---"
        kubectl logs "$pod" -n sandbox-system -c controller --previous --tail=200 2>&1 || echo "Failed to get previous controller logs for $pod"
    done
    exit 1
fi

echo "All sandbox-manager pods are ready"

if [ "$WITH_GATEWAY" = "true" ]; then
    # Wait for sandbox-gateway pods to be ready
    echo "Waiting for sandbox-gateway pods to be ready..."
    kubectl wait --for=condition=ready pod \
        -l app.kubernetes.io/name=sandbox-gateway \
        -n sandbox-system \
        --timeout=5m
    echo "All sandbox-gateway pods are ready"

    # Port-forward gateway as unified entry point (80 -> 7788, which targets Envoy :10000)
    sudo -E kubectl port-forward svc/sandbox-gateway 80:7788 -n sandbox-system &
else
    # Port-forward sandbox-manager directly
    sudo -E kubectl port-forward svc/sandbox-manager 80:7788 -n sandbox-system &
fi

# Step 2: Install e2b-code-interpreter
echo "Installing dependencies..."
pip install -r $TEST_DIR/requirements.txt
if [ -n "$E2B_VERSION" ]; then
    pip install "e2b==$E2B_VERSION"
fi
if [ -n "$SDK_VERSION" ]; then
    pip install "e2b-code-interpreter==$SDK_VERSION"
else
    pip install e2b-code-interpreter
fi

echo "dependencies installed successfully"

installed_e2b_version="$(get_installed_e2b_version)"
if [ -z "$installed_e2b_version" ]; then
    echo "Error: failed to determine installed e2b version"
    exit 1
fi
if [ "$AUTH_DISABLED" = "true" ]; then
    echo "E2B auth is disabled; skipping API key compatibility conversion"
    export E2B_API_KEY="${E2B_AUTH_DISABLED_API_KEY:-e2b_abc123}"
else
    convert_e2b_api_key_for_sdk_if_needed "$installed_e2b_version"
fi

# Step 3: Run pytest tests serially (print the key for debug, it's safe to print it in a ci pipeline)
echo "Using E2B_API_KEY: ${E2B_API_KEY:-}"
echo "Running E2B pytest tests..."
set +e

# Check if code-interpreter sandboxset already exists
echo "Checking for code-interpreter sandboxset..."
if kubectl get sandboxset code-interpreter -n default &>/dev/null; then
    echo "SandboxSet 'code-interpreter' already exists, skipping creation"
else
    echo "Creating code-interpreter sandboxset..."
    kubectl apply -f $TEST_DIR/assets/sandboxset-code-interpreter.yaml
    echo "wait 5 seconds before test start"
    sleep 5
fi

# Check if code-interpreter-0 sandboxset already exists
echo "Checking for code-interpreter-0 sandboxset..."
if kubectl get sandboxset code-interpreter-0 -n default &>/dev/null; then
    echo "SandboxSet 'code-interpreter-0' already exists, skipping creation"
else
    echo "Creating code-interpreter-0 sandboxset..."
    kubectl apply -f $TEST_DIR/assets/sandboxset-code-interpreter-0.yaml
    echo "wait 5 seconds before test start"
    sleep 5
fi

# Run pytest with serial execution (no parallel flag)
cd "$PROJECT_ROOT"
pytest_args=(-v -s -x --tb=short)
if [ "$WITH_GATEWAY" != "true" ]; then
    pytest_args+=(--ignore="$TEST_DIR/test_gateway.py")
    pytest_args+=(--ignore="$TEST_DIR/test_gateway_auth.py")
fi
if [ "$AUTH_DISABLED" = "true" ]; then
    pytest_args+=(--ignore="$TEST_DIR/test_apikey.py")
fi
pytest "${pytest_args[@]}" "$TEST_DIR"
retVal=$?

set +x

if [ "$retVal" -ne 0 ]; then
    echo "Tests failed"
else
    echo "All E2B tests passed successfully!"
fi

# Check if sandbox-manager pods restarted
restartCount=$(kubectl get pod -n sandbox-system -l "${MANAGER_SELECTOR}" --no-headers | awk '{sum+=$4} END {print sum}')
if [ -z "$restartCount" ]; then
    restartCount=0
fi

if [ "${restartCount}" -eq "0" ]; then
    echo "sandbox-manager has not restarted"
else
    kubectl get pod -n sandbox-system --no-headers -l "${MANAGER_SELECTOR}"
    echo "sandbox-manager has restarted, abort!!!"
    kubectl get pod -n sandbox-system -l "${MANAGER_SELECTOR}" --no-headers | awk '{print $1}' | xargs -I {} kubectl logs -p -n sandbox-system -c controller {}
    exit 1
fi

# Check if sandbox-gateway pods restarted (only when gateway is enabled)
if [ "$WITH_GATEWAY" = "true" ]; then
    gwRestartCount=$(kubectl get pod -n sandbox-system -l app.kubernetes.io/name=sandbox-gateway --no-headers | awk '{sum+=$4} END {print sum}')
    if [ -z "$gwRestartCount" ]; then
        gwRestartCount=0
    fi

    if [ "${gwRestartCount}" -eq "0" ]; then
        echo "sandbox-gateway has not restarted"
    else
        kubectl get pod -n sandbox-system --no-headers -l app.kubernetes.io/name=sandbox-gateway
        echo "sandbox-gateway has restarted, abort!!!"
        kubectl get pod -n sandbox-system -l app.kubernetes.io/name=sandbox-gateway --no-headers | awk '{print $1}' | xargs -I {} kubectl logs -p -n sandbox-system {}
        exit 1
    fi
fi

exit $retVal
