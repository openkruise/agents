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

# Deploy a single-replica MySQL 8 Deployment + Service into sandbox-system (Kind / CI).
# Ensures the target namespace exists (creates it if missing) so MySQL can be deployed before Kruise Agents.
#
# Env (optional): MYSQL_DATABASE, MYSQL_ROOT_PASSWORD, MYSQL_NAMESPACE, MYSQL_WAIT_TIMEOUT

set -euo pipefail

MYSQL_DATABASE="${MYSQL_DATABASE:-e2b_keys}"
MYSQL_ROOT_PASSWORD="${MYSQL_ROOT_PASSWORD:-ci-password}"
MYSQL_NAMESPACE="${MYSQL_NAMESPACE:-sandbox-system}"
MYSQL_WAIT_TIMEOUT="${MYSQL_WAIT_TIMEOUT:-5m}"

log() {
  echo "[deploy-mysql-kind-ci $(date -u +%Y-%m-%dT%H:%M:%SZ)] $*"
}

log_fail() {
  echo "[deploy-mysql-kind-ci $(date -u +%Y-%m-%dT%H:%M:%SZ)] ERROR: $*" >&2
}

dump_mysql_debug() {
  log "Collecting diagnostics for MySQL deploy failure..."
  set +e
  kubectl get namespace "${MYSQL_NAMESPACE}" -o wide 2>&1 || true
  kubectl get deployment,svc,pods -n "${MYSQL_NAMESPACE}" -l app=mysql -o wide 2>&1 || true
  kubectl describe deployment mysql -n "${MYSQL_NAMESPACE}" 2>&1 || true
  kubectl describe svc mysql -n "${MYSQL_NAMESPACE}" 2>&1 || true
  kubectl get pods -n "${MYSQL_NAMESPACE}" -l app=mysql -o wide 2>&1 || true
  while read -r pod; do
    [[ -z "${pod}" ]] && continue
    log "--- describe pod ${pod} ---"
    kubectl describe pod "${pod}" -n "${MYSQL_NAMESPACE}" 2>&1 || true
    log "--- logs pod ${pod} (current) ---"
    kubectl logs "${pod}" -n "${MYSQL_NAMESPACE}" --tail=200 2>&1 || true
    log "--- logs pod ${pod} (previous) ---"
    kubectl logs "${pod}" -n "${MYSQL_NAMESPACE}" --previous --tail=200 2>&1 || true
  done < <(kubectl get pods -n "${MYSQL_NAMESPACE}" -l app=mysql -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
  log "--- recent events in ${MYSQL_NAMESPACE} ---"
  kubectl get events -n "${MYSQL_NAMESPACE}" --sort-by=.lastTimestamp 2>&1 | tail -n 40 || true
  set -e
}

cleanup_on_error() {
  dump_mysql_debug
  exit 1
}

trap cleanup_on_error ERR

log "Starting MySQL deploy: namespace=${MYSQL_NAMESPACE} database=${MYSQL_DATABASE} wait_timeout=${MYSQL_WAIT_TIMEOUT}"
log "Ensuring namespace ${MYSQL_NAMESPACE} exists..."
kubectl create namespace "${MYSQL_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# The official image runs as OS user 'mysql'; mysqladmin defaults to -u matching that user, so TCP
# checks must use -uroot. Probes bake the password at apply time from MYSQL_ROOT_PASSWORD.
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mysql
  namespace: ${MYSQL_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: mysql
  template:
    metadata:
      labels:
        app: mysql
    spec:
      containers:
      - name: mysql
        image: mysql:8.0
        imagePullPolicy: IfNotPresent
        env:
        - name: MYSQL_ROOT_PASSWORD
          value: "${MYSQL_ROOT_PASSWORD}"
        - name: MYSQL_DATABASE
          value: "${MYSQL_DATABASE}"
        ports:
        - containerPort: 3306
        resources:
          requests:
            cpu: "250m"
            memory: "512Mi"
        startupProbe:
          exec:
            command:
            - /bin/sh
            - -c
            - mysqladmin ping -h 127.0.0.1 -uroot -p"${MYSQL_ROOT_PASSWORD}"
          periodSeconds: 5
          timeoutSeconds: 5
          failureThreshold: 36
        readinessProbe:
          exec:
            command:
            - /bin/sh
            - -c
            - mysqladmin ping -h 127.0.0.1 -uroot -p"${MYSQL_ROOT_PASSWORD}"
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
          failureThreshold: 12
---
apiVersion: v1
kind: Service
metadata:
  name: mysql
  namespace: ${MYSQL_NAMESPACE}
spec:
  selector:
    app: mysql
  ports:
  - port: 3306
    targetPort: 3306
EOF

log "Applied Deployment and Service; waiting for rollout..."
kubectl rollout status deployment/mysql -n "${MYSQL_NAMESPACE}" --timeout="${MYSQL_WAIT_TIMEOUT}"

log "Waiting for pod Ready condition..."
kubectl wait --for=condition=ready pod -l app=mysql -n "${MYSQL_NAMESPACE}" --timeout="${MYSQL_WAIT_TIMEOUT}"

log "MySQL is ready."
kubectl get pods -n "${MYSQL_NAMESPACE}" -l app=mysql -o wide

trap - ERR
