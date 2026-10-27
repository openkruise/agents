#!/usr/bin/env bash
# Copyright 2026 The OpenKruise Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_ROOT"
PROFILE="${1:-}"
case "$PROFILE" in
  ingress-no-redis) ACCESS_MODE=ingress; QUOTA_PROFILE=no-redis; AUTH_ENABLED=true ;;
  direct-redis) ACCESS_MODE=direct; QUOTA_PROFILE=redis; AUTH_ENABLED=true ;;
  direct-auth-disabled) ACCESS_MODE=direct; QUOTA_PROFILE=none; AUTH_ENABLED=false ;;
  *) echo "Usage: $0 {ingress-no-redis|direct-redis|direct-auth-disabled}" >&2; exit 2 ;;
esac
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-opensandbox-ci}"
CONTEXT="kind-${KIND_CLUSTER_NAME}"
PYTHON="${PYTHON:-python3}"
ARTIFACTS_DIR="${ARTIFACTS_DIR:-$PROJECT_ROOT/test/opensandbox/reports}"
mkdir -p "$ARTIFACTS_DIR"
ARTIFACTS_DIR="$(cd "$ARTIFACTS_DIR" && pwd)"
export ARTIFACTS_DIR
export CONTROLLER_IMG="${CONTROLLER_IMG:-agent-sandbox-controller:latest}"
export MANAGER_IMG="${MANAGER_IMG:-sandbox-manager:latest}"
export GATEWAY_IMG="${GATEWAY_IMG:-sandbox-gateway:latest}"
export RUNTIME_IMG="${RUNTIME_IMG:-agent-runtime:opensandbox-ci-74b304e}"
export CODE_INTERPRETER_IMG="${CODE_INTERPRETER_IMG:-registry-ap-southeast-1.ack.aliyuncs.com/acs/code-interpreter:v1.6}"
export OPENSANDBOX_SDK_VERSION="${OPENSANDBOX_SDK_VERSION:-1.1.0}"
export OPENSANDBOX_API_KEY="${OPENSANDBOX_API_KEY:-some-api-key}"
export OPENSANDBOX_AUTH_ENABLED="$AUTH_ENABLED"
export OPENSANDBOX_ACCESS_MODE="$ACCESS_MODE"
export OPENSANDBOX_QUOTA_PROFILE="$QUOTA_PROFILE"
export OPENSANDBOX_GATEWAY_URL=http://sandbox-gateway.sandbox-system.svc:7788
export OPENSANDBOX_KUBE_CONTEXT="$CONTEXT"
export OPENSANDBOX_POOL_REF=opensandbox-control-plane
export OPENSANDBOX_EMPTY_POOL_REF=opensandbox-control-plane-empty
export OPENSANDBOX_IMAGE="$CODE_INTERPRETER_IMG"
export TEST_NAMESPACE=default
PORT="${OPENSANDBOX_E2E_PORT:-18080}"
export OPENSANDBOX_API_URL="http://127.0.0.1:${PORT}/v1"
export E2B_API_URL="http://127.0.0.1:${PORT}/kruise/api"

# This script owns its dedicated Kind deployment, never an existing user cluster.
if [[ "$(kubectl config current-context)" != "$CONTEXT" ]]; then
  echo "Select the dedicated Kind context $CONTEXT before running this script" >&2
  exit 2
fi
for deployment in sandbox-manager sandbox-controller-manager sandbox-gateway; do
  existing="$(kubectl -n sandbox-system get deployment "$deployment" --ignore-not-found -o json)"
  if [[ -n "$existing" ]]; then
    printf '%s' "$existing" | "$PYTHON" -c '
import json, sys
obj = json.load(sys.stdin)
if obj["metadata"].get("labels", {}).get("opensandbox-control-plane-e2e") != "true":
    raise SystemExit("Refusing to replace a deployment not owned by this suite")
'
  fi
done
WORK="$(mktemp -d "${TMPDIR:-/tmp}/opensandbox-control-plane.XXXXXX")"
forward_pid=""
cleanup() {
  local result=$?
  trap - EXIT
  if [[ "$result" -ne 0 ]]; then
    kubectl -n sandbox-system get pods -o wide > "$ARTIFACTS_DIR/system-pods.log" 2>&1 || true
    kubectl -n default get pods -o wide > "$ARTIFACTS_DIR/workload-pods.log" 2>&1 || true
    kubectl -n default describe pods -l opensandbox-control-plane-e2e=true \
      > "$ARTIFACTS_DIR/workload-describe.log" 2>&1 || true
    kubectl -n default logs -l opensandbox-control-plane-e2e=true --all-containers \
      --prefix --tail=200 --max-log-requests=20 > "$ARTIFACTS_DIR/workload-containers.log" 2>&1 || true
    kubectl get events -A --sort-by=.lastTimestamp > "$ARTIFACTS_DIR/events.log" 2>&1 || true
    for deployment in sandbox-manager sandbox-controller-manager sandbox-gateway; do
      kubectl -n sandbox-system logs "deployment/$deployment" --all-containers --tail=200 \
        > "$ARTIFACTS_DIR/$deployment.log" 2>&1 || true
    done
  fi
  if [[ -n "$forward_pid" ]]; then
    kill "$forward_pid" 2>/dev/null || true
    wait "$forward_pid" 2>/dev/null || true
  fi
  rm -rf "$WORK"
  exit "$result"
}
trap cleanup EXIT

"$PYTHON" -m venv "$WORK/venv"
PYTHON="$WORK/venv/bin/python"
"$PYTHON" -m pip install --disable-pip-version-check -r test/opensandbox/requirements.txt \
  'PyYAML==6.0.3' > "$ARTIFACTS_DIR/pip-install.log" 2>&1
if [[ "$OPENSANDBOX_SDK_VERSION" == latest ]]; then
  "$PYTHON" -m pip install --disable-pip-version-check --upgrade opensandbox >> "$ARTIFACTS_DIR/pip-install.log" 2>&1
elif [[ "$OPENSANDBOX_SDK_VERSION" != 1.1.0 ]]; then
  echo "Unsupported SDK track: $OPENSANDBOX_SDK_VERSION" >&2
  exit 2
fi
"$PYTHON" -m pip freeze > "$ARTIFACTS_DIR/packages.txt"
"$PYTHON" - <<'PY'
from importlib.metadata import version
import json, os
from pathlib import Path
actual = version("opensandbox")
track = os.environ["OPENSANDBOX_SDK_VERSION"]
assert track == "latest" or actual == track, (track, actual)
report = {"sdk_track": track, "sdk_version": actual,
          "access_mode": os.environ["OPENSANDBOX_ACCESS_MODE"],
          "quota_profile": os.environ["OPENSANDBOX_QUOTA_PROFILE"],
          "auth_enabled": os.environ["OPENSANDBOX_AUTH_ENABLED"]}
Path(os.environ["ARTIFACTS_DIR"], "profile.json").write_text(json.dumps(report, indent=2) + "\n")
print(json.dumps(report))
PY
mkdir -p "$ARTIFACTS_DIR/manifests"
"$PYTHON" test/opensandbox/prepare_control_plane.py --runtime-image "$RUNTIME_IMG" --workload-image "$CODE_INTERPRETER_IMG" \
  --output-dir "$ARTIFACTS_DIR/manifests"
make kustomize
for component in default sandbox-manager sandbox-gateway; do
  bin/kustomize build "config/$component" > "$WORK/$component.yaml"
done
"$PYTHON" - "$WORK" "$ARTIFACTS_DIR/manifests" <<'PY'
import json, os, sys
from pathlib import Path
import yaml
for component in ("default", "sandbox-manager", "sandbox-gateway"):
    documents = [obj for obj in yaml.safe_load_all(Path(sys.argv[1], component + ".yaml").read_text()) if obj is not None]
    for obj in documents:
        if obj.get("kind") != "Deployment":
            continue
        obj["metadata"].setdefault("labels", {})["opensandbox-control-plane-e2e"] = "true"
        containers = obj["spec"]["template"]["spec"]["containers"]
        if component == "sandbox-manager":
            manager = next(c for c in containers if c["name"] == "controller")
            manager["image"] = os.environ["MANAGER_IMG"]
            prefixes = ("--enable-opensandbox-compat=", "--opensandbox-access-mode=",
                        "--opensandbox-gateway-url=", "--e2b-enable-auth=", "--e2b-admin-key=",
                        "--quota-redis-addr=", "--quota-redis-operation-timeout=")
            args = [a for a in manager["args"] if not a.startswith(prefixes)]
            redis = "redis.sandbox-system.svc.cluster.local:6379" if os.environ["OPENSANDBOX_QUOTA_PROFILE"] == "redis" else ""
            manager["args"] = args + ["--enable-opensandbox-compat=true",
                "--opensandbox-access-mode=" + os.environ["OPENSANDBOX_ACCESS_MODE"],
                "--opensandbox-gateway-url=" + os.environ["OPENSANDBOX_GATEWAY_URL"],
                "--e2b-admin-key=" + os.environ["OPENSANDBOX_API_KEY"],
                "--e2b-enable-auth=" + os.environ["OPENSANDBOX_AUTH_ENABLED"],
                "--quota-redis-addr=" + redis, "--quota-redis-operation-timeout=1s"]
            manager["resources"]["requests"] = {"cpu": "100m", "memory": "256Mi"}
            next(c for c in containers if c["name"] == "envoy-proxy")["resources"]["requests"] = {"cpu": "50m", "memory": "128Mi"}
        else:
            containers[0]["image"] = os.environ["CONTROLLER_IMG" if component == "default" else "GATEWAY_IMG"]
    Path(sys.argv[2], component + ".json").write_text(json.dumps({"apiVersion": "v1", "kind": "List", "items": documents}, indent=2) + "\n")
PY
kubectl create namespace sandbox-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f "$ARTIFACTS_DIR/manifests/default.json"
kubectl -n sandbox-system rollout status deployment/sandbox-controller-manager --timeout=300s
if [[ "$QUOTA_PROFILE" == redis ]]; then
  kubectl apply -f test/e2b/assets/quota-redis.yaml
  kubectl -n sandbox-system rollout status deployment/redis --timeout=120s
fi
kubectl apply -f "$ARTIFACTS_DIR/manifests/runtime-injection.json"
kubectl apply -f "$ARTIFACTS_DIR/manifests/sandbox-manager.json"
kubectl apply -f "$ARTIFACTS_DIR/manifests/sandbox-gateway.json"
kubectl -n sandbox-system rollout status deployment/sandbox-manager --timeout=300s
kubectl -n sandbox-system rollout status deployment/sandbox-gateway --timeout=300s
kubectl apply -f "$ARTIFACTS_DIR/manifests/sandboxset-opensandbox-control-plane.json"
kubectl apply -f "$ARTIFACTS_DIR/manifests/sandboxset-opensandbox-control-plane-empty.json"
kubectl wait sandboxset/opensandbox-control-plane --for=jsonpath='{.status.availableReplicas}'=4 --timeout=180s
kubectl -n sandbox-system port-forward --address=127.0.0.1 service/sandbox-manager "${PORT}:8080" \
  > "$ARTIFACTS_DIR/port-forward.log" 2>&1 &
forward_pid=$!
for _ in $(seq 1 50); do
  if ! kill -0 "$forward_pid" 2>/dev/null; then
    cat "$ARTIFACTS_DIR/port-forward.log" >&2
    exit 1
  fi
  if grep -q "Forwarding from 127.0.0.1:" "$ARTIFACTS_DIR/port-forward.log"; then
    break
  fi
  sleep 0.2
done
grep -q "Forwarding from 127.0.0.1:" "$ARTIFACTS_DIR/port-forward.log"
curl --fail --silent --show-error --retry 30 --retry-delay 1 --retry-connrefused \
  --connect-timeout 2 --max-time 5 -H "OPEN-SANDBOX-API-KEY: $OPENSANDBOX_API_KEY" \
  "$OPENSANDBOX_API_URL/sandboxes" > "$ARTIFACTS_DIR/preflight.json"
kill -0 "$forward_pid"
tests=(test/opensandbox/test_create.py test/opensandbox/test_sandbox.py
       test/opensandbox/test_endpoint.py test/opensandbox/test_auth.py test/opensandbox/test_quota.py)
"$PYTHON" -m pytest "${tests[@]}" --collect-only -q > "$ARTIFACTS_DIR/collection.log"
"$PYTHON" -m pytest "${tests[@]}" --tb=short -v --junitxml="$ARTIFACTS_DIR/junit.xml" \
  2>&1 | tee "$ARTIFACTS_DIR/pytest.log"
# Match E2B's post-suite Manager/Gateway restart gate.
kubectl -n sandbox-system get pods \
  -l 'app.kubernetes.io/name in (sandbox-manager,sandbox-gateway)' -o json \
  > "$ARTIFACTS_DIR/component-pods.json"
"$PYTHON" - "$ARTIFACTS_DIR" <<'PY'
import json, sys
from pathlib import Path
reports = Path(sys.argv[1])
pods = json.loads((reports / "component-pods.json").read_text())["items"]
expected = {"sandbox-manager", "sandbox-gateway"}
observed = set()
statuses = []
for pod in pods:
    metadata = pod["metadata"]
    observed.add(metadata["labels"]["app.kubernetes.io/name"])
    containers = pod.get("status", {}).get("containerStatuses", [])
    assert {c["name"] for c in containers} == {c["name"] for c in pod["spec"]["containers"]}, metadata["name"]
    statuses.extend({"pod": metadata["name"], "container": c["name"], "restartCount": c["restartCount"]}
                    for c in containers)
(reports / "component-restarts.json").write_text(json.dumps(statuses, indent=2) + "\n")
assert observed == expected, (observed, expected)
assert all(c["restartCount"] == 0 for c in statuses), statuses
PY
