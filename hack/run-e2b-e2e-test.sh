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

# Get the directory where this script is located
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
TEST_DIR="$PROJECT_ROOT/test/e2b"

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
        *)
            echo "Unknown parameter passed: $1"
            echo "Usage: $0 [--e2b-version VERSION] [--sdk-version VERSION] [--timeout TIMEOUT]"
            exit 1
            ;;
    esac
done

config=${KUBECONFIG:-${HOME}/.kube/config}
export KUBECONFIG=${config}

set -x

# Step 1: Wait for sandbox-manager pods to be ready
echo "Waiting for sandbox-manager pods to be ready..."
kubectl wait --for=condition=ready pod \
    -l component=sandbox-manager \
    -n sandbox-system \
    --timeout=5m

echo "All sandbox-manager pods are ready"

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

# Step 3: Run pytest tests serially
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

# Run pytest with serial execution (no parallel flag)
cd "$PROJECT_ROOT"
pytest -v -s -x --tb=short "$TEST_DIR"
retVal=$?

set +x

if [ "$retVal" -ne 0 ]; then
    echo "Tests failed"
else
    echo "All E2B tests passed successfully!"
fi

# Check if sandbox-manager pods restarted
restartCount=$(kubectl get pod -n sandbox-system -l component=sandbox-manager --no-headers | awk '{sum+=$4} END {print sum}')
if [ -z "$restartCount" ]; then
    restartCount=0
fi

if [ "${restartCount}" -eq "0" ]; then
    echo "sandbox-manager has not restarted"
else
    kubectl get pod -n sandbox-system --no-headers -l component=sandbox-manager
    echo "sandbox-manager has restarted, abort!!!"
    kubectl get pod -n sandbox-system -l component=sandbox-manager --no-headers | awk '{print $1}' | xargs -I {} kubectl logs -p -n sandbox-system {}
    exit 1
fi

exit $retVal
