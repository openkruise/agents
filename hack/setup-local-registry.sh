#!/usr/bin/env bash
# Copyright 2026.
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

# Deploy a self-signed TLS registry in the kind cluster for e2e tests.
# This creates:
#   1. A self-signed CA + server certificate
#   2. A registry:2 Deployment with TLS enabled (port 5443)
#   3. Configures kind nodes with certs.d so containerd/nerdctl trusts the CA
#   4. Adds /etc/hosts entries on kind nodes for hostNetwork pod DNS resolution

set -o errexit
set -o pipefail

REGISTRY_NS="e2e-registry"
REGISTRY_NAME="tls-registry"
REGISTRY_HOST="${REGISTRY_NAME}.${REGISTRY_NS}.svc.cluster.local"
REGISTRY_PORT="5443"
KIND_CLUSTER="${KIND_CLUSTER:-ci-testing}"
KIND="${KIND:-kind}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "${TMPDIR}"' EXIT

echo "==> Generating self-signed CA and server certificate..."

# Generate CA key and certificate
openssl genrsa -out "${TMPDIR}/ca.key" 2048 2>/dev/null
openssl req -x509 -new -nodes -key "${TMPDIR}/ca.key" \
  -sha256 -days 365 -out "${TMPDIR}/ca.crt" \
  -subj "/CN=e2e-registry-ca" 2>/dev/null

# Generate server key and CSR
openssl genrsa -out "${TMPDIR}/server.key" 2048 2>/dev/null
openssl req -new -key "${TMPDIR}/server.key" \
  -out "${TMPDIR}/server.csr" \
  -subj "/CN=${REGISTRY_HOST}" 2>/dev/null

# Create server certificate with SAN
cat > "${TMPDIR}/extfile.cnf" <<EOF
[v3_req]
subjectAltName = DNS:${REGISTRY_HOST}
EOF

openssl x509 -req -in "${TMPDIR}/server.csr" \
  -CA "${TMPDIR}/ca.crt" -CAkey "${TMPDIR}/ca.key" -CAcreateserial \
  -out "${TMPDIR}/server.crt" -days 365 -sha256 \
  -extfile "${TMPDIR}/extfile.cnf" -extensions v3_req 2>/dev/null

echo "==> Creating namespace ${REGISTRY_NS}..."
kubectl create namespace "${REGISTRY_NS}" --dry-run=client -o yaml | kubectl apply -f -

echo "==> Creating TLS secret..."
kubectl create secret tls registry-tls \
  -n "${REGISTRY_NS}" \
  --cert="${TMPDIR}/server.crt" \
  --key="${TMPDIR}/server.key" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "==> Deploying TLS registry..."
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${REGISTRY_NAME}
  namespace: ${REGISTRY_NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${REGISTRY_NAME}
  template:
    metadata:
      labels:
        app: ${REGISTRY_NAME}
    spec:
      containers:
      - name: registry
        image: registry:2
        ports:
        - containerPort: ${REGISTRY_PORT}
        env:
        - name: REGISTRY_HTTP_ADDR
          value: "0.0.0.0:${REGISTRY_PORT}"
        - name: REGISTRY_HTTP_TLS_CERTIFICATE
          value: "/certs/tls.crt"
        - name: REGISTRY_HTTP_TLS_KEY
          value: "/certs/tls.key"
        volumeMounts:
        - name: certs
          mountPath: /certs
          readOnly: true
      volumes:
      - name: certs
        secret:
          secretName: registry-tls
---
apiVersion: v1
kind: Service
metadata:
  name: ${REGISTRY_NAME}
  namespace: ${REGISTRY_NS}
spec:
  selector:
    app: ${REGISTRY_NAME}
  ports:
  - port: ${REGISTRY_PORT}
    targetPort: ${REGISTRY_PORT}
    protocol: TCP
EOF

echo "==> Configuring kind nodes with certs.d for ${REGISTRY_HOST}:${REGISTRY_PORT}..."
CERTS_DIR="/etc/containerd/certs.d/${REGISTRY_HOST}:${REGISTRY_PORT}"

for node in $(${KIND} get nodes --name "${KIND_CLUSTER}" 2>/dev/null); do
  echo "    Configuring node: ${node}"
  docker exec "${node}" mkdir -p "${CERTS_DIR}"

  # Copy CA certificate to node
  docker cp "${TMPDIR}/ca.crt" "${node}:${CERTS_DIR}/ca.crt"

  # Write hosts.toml for containerd/nerdctl
  docker exec "${node}" bash -c "cat > ${CERTS_DIR}/hosts.toml" <<EOF
server = "https://${REGISTRY_HOST}:${REGISTRY_PORT}"

[host."https://${REGISTRY_HOST}:${REGISTRY_PORT}"]
  ca = "${CERTS_DIR}/ca.crt"
EOF
done

echo "==> Waiting for TLS registry pod to be ready..."
kubectl wait --for=condition=available deployment/${REGISTRY_NAME} \
  -n "${REGISTRY_NS}" --timeout=120s

# Job pods use hostNetwork: true, so they use the node's DNS (not CoreDNS)
# and cannot resolve *.svc.cluster.local names. Add /etc/hosts entries
# on each kind node so the FQDN resolves to the Service ClusterIP.
echo "==> Adding /etc/hosts entries on kind nodes for hostNetwork pods..."
CLUSTER_IP=$(kubectl get svc "${REGISTRY_NAME}" -n "${REGISTRY_NS}" -o jsonpath='{.spec.clusterIP}')
for node in $(${KIND} get nodes --name "${KIND_CLUSTER}" 2>/dev/null); do
  docker exec "${node}" bash -c "echo '${CLUSTER_IP} ${REGISTRY_HOST}' >> /etc/hosts"
done

echo "==> TLS registry is ready at ${REGISTRY_HOST}:${REGISTRY_PORT}"
