#!/usr/bin/env bash
# Copyright 2026. Licensed under the Apache License, Version 2.0.
set -euo pipefail

TASK_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_DIR="$TASK_ROOT/test/opensandbox"
MODE="${1:-both}"
RUNTIME_IMG="${RUNTIME_IMG:-agent-runtime:latest}"
MANAGER_DEPLOYMENT="${MANAGER_DEPLOYMENT:-sandbox-manager}"
REPORT_DIR="${REPORT_DIR:-$TASK_ROOT/opensandbox-e2e-results}"
export OPENSANDBOX_GATEWAY_URL="${OPENSANDBOX_GATEWAY_URL:-http://sandbox-gateway.sandbox-system.svc:7788}"
export OPENSANDBOX_API_URL="${OPENSANDBOX_API_URL:-http://sandbox-manager.sandbox-system.svc:8080}"
export OPENSANDBOX_API_KEY="${OPENSANDBOX_API_KEY:-some-api-key}"
export CODE_INTERPRETER_IMG="${CODE_INTERPRETER_IMG:-registry-ap-southeast-1.ack.aliyuncs.com/acs/code-interpreter:v1.6}"

case "$MODE" in both) MODES=(ingress direct);; ingress|direct) MODES=("$MODE");; *) echo "Usage: $0 [both|ingress|direct]" >&2; exit 2;; esac
mkdir -p "$REPORT_DIR"
kubectl rollout status "deployment/$MANAGER_DEPLOYMENT" -n sandbox-system --timeout=5m
kubectl rollout status deployment/sandbox-gateway -n sandbox-system --timeout=5m
python3 - "$TEST_DIR/assets/runtime-injection.yaml" "$RUNTIME_IMG" <<'PY' | kubectl apply -f -
import pathlib, sys
print(pathlib.Path(sys.argv[1]).read_text().replace("${RUNTIME_IMG}", sys.argv[2]))
PY
kubectl apply -f "$TEST_DIR/assets/runner-rbac.yaml"
kubectl create configmap opensandbox-e2e-suite --dry-run=client -o yaml \
  --from-file="$TEST_DIR/conftest.py" --from-file="$TEST_DIR/test_commands.py" \
  --from-file="$TEST_DIR/test_files.py" --from-file="$TEST_DIR/test_auth.py" \
  --from-file="$TEST_DIR/requirements.txt" --from-file="$TEST_DIR/pytest.ini" \
  --from-file="$TASK_ROOT/test/e2b/joke.txt" | kubectl apply -f -

for access_mode in "${MODES[@]}"; do
  kubectl get deployment "$MANAGER_DEPLOYMENT" -n sandbox-system -o json > "$REPORT_DIR/manager.json"
  python3 - "$REPORT_DIR/manager.json" "$access_mode" > "$REPORT_DIR/manager-patch.json" <<'PY'
import json, os, sys
deployment = json.load(open(sys.argv[1]))
containers = deployment["spec"]["template"]["spec"]["containers"]
index = next(i for i, item in enumerate(containers) if item["name"] == "controller")
prefixes = ("--enable-opensandbox-compat", "--opensandbox-access-mode", "--opensandbox-gateway-url")
args = [arg for arg in containers[index].get("args", []) if not arg.startswith(prefixes)]
args += ["--enable-opensandbox-compat=true", f"--opensandbox-access-mode={sys.argv[2]}", f"--opensandbox-gateway-url={os.environ['OPENSANDBOX_GATEWAY_URL']}"]
print(json.dumps([{"op": "add", "path": f"/spec/template/spec/containers/{index}/args", "value": args}]))
PY
  kubectl patch deployment "$MANAGER_DEPLOYMENT" -n sandbox-system --type=json --patch-file "$REPORT_DIR/manager-patch.json"
  kubectl rollout status "deployment/$MANAGER_DEPLOYMENT" -n sandbox-system --timeout=5m
  job="opensandbox-e2e-$access_mode"
  kubectl delete job "$job" --ignore-not-found --wait=true
  python3 - "$job" "$access_mode" <<'PY' | kubectl apply -f -
import json, os, sys
command = "python -m pip install --disable-pip-version-check -r /tests/requirements.txt; status=0; python -m pytest /tests -c /tests/pytest.ini --junitxml=/tmp/junit.xml || status=$?; echo OPENSANDBOX_JUNIT_BEGIN; cat /tmp/junit.xml; exit $status"
env = [{"name": name, "value": os.environ[name]} for name in ("OPENSANDBOX_API_URL", "OPENSANDBOX_GATEWAY_URL", "OPENSANDBOX_API_KEY", "CODE_INTERPRETER_IMG")]
env += [{"name": "OPENSANDBOX_ACCESS_MODE", "value": sys.argv[2]}, {"name": "OPENSANDBOX_AUTH_E2E", "value": os.environ.get("OPENSANDBOX_AUTH_E2E", "false")}]
print(json.dumps({"apiVersion": "batch/v1", "kind": "Job", "metadata": {"name": sys.argv[1], "namespace": "default"}, "spec": {"backoffLimit": 0, "activeDeadlineSeconds": 900, "template": {"spec": {"serviceAccountName": "opensandbox-e2e", "restartPolicy": "Never", "containers": [{"name": "pytest", "image": "python:3.12-slim", "command": ["/bin/sh", "-ec", command], "env": env, "volumeMounts": [{"name": "suite", "mountPath": "/tests", "readOnly": True}]}], "volumes": [{"name": "suite", "configMap": {"name": "opensandbox-e2e-suite"}}]}}}}))
PY
  for _ in $(seq 1 185); do
    state="$(kubectl get job "$job" -o jsonpath='{.status.succeeded} {.status.failed}')"
    if [[ "$state" == 1* || "$state" == *1 ]]; then break; fi
    sleep 5
  done
  kubectl logs "job/$job" > "$REPORT_DIR/$access_mode.log" 2>&1 || true
  python3 - "$REPORT_DIR/$access_mode.log" "$REPORT_DIR/junit-$access_mode.xml" <<'PY'
import pathlib, sys
log = pathlib.Path(sys.argv[1]).read_text()
if "OPENSANDBOX_JUNIT_BEGIN\n" in log:
    pathlib.Path(sys.argv[2]).write_text(log.split("OPENSANDBOX_JUNIT_BEGIN\n", 1)[1])
PY
  if [[ "$state" != 1* ]]; then
    cat "$REPORT_DIR/$access_mode.log"
    kubectl get events --sort-by=.lastTimestamp
    kubectl logs -n sandbox-system "deployment/$MANAGER_DEPLOYMENT" -c controller --tail=100 || true
    exit 1
  fi
  cat "$REPORT_DIR/$access_mode.log"
done
