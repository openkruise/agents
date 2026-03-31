# Running OpenClaw Sandbox

This example demonstrates how to deploy [OpenClaw](https://github.com/openclaw) via OpenKruise Agents and initialize
OpenClaw instances from a pre-warmed pool using the E2B SDK.

## 0. Basic Concepts

### OpenClaw

`OpenClaw` is an open-source AI programming assistant gateway that provides unified LLM access, tool invocation, and
code execution capabilities. By deploying OpenClaw through OpenKruise Agents, you can provide each user with an
independent, data-persistent sandbox environment.

### Data Persistence

Unlike regular code execution sandboxes, OpenClaw needs to persist user configuration files and working data. This
example uses `volumeClaimTemplates` to automatically create independent cloud disks for each Sandbox, ensuring data
is not lost when a Sandbox is recreated.

---

## 1. Prerequisites

### 1.1 Configuring Dynamic Storage

OpenClaw requires persistent storage to save user configurations and data. First, create a StorageClass:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: openclaw-disk-sc
provisioner: ****
parameters:
  type: *****
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Retain
allowVolumeExpansion: true
```

**Key Configuration Parameters:**

| Parameter | Description |
|-----------|-------------|
| `provisioner` | Cloud disk provider |
| `volumeBindingMode: WaitForFirstConsumer` | Delayed binding - schedules Pod first, then creates disk based on availability zone |
| `reclaimPolicy: Retain` | Retains PV and data when PVC is deleted; manual cleanup required |

---

## 2. Deploying OpenClaw Pre-warmed Pool

### 2.1 Deploying via SandboxSet

Create a SandboxSet to deploy the OpenClaw pre-warmed pool:

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  name: openclaw-sbs
  namespace: default
spec:
  replicas: 3
  template:
    metadata:
      labels:
        app: openclaw
    spec:
      automountServiceAccountToken: false
      enableServiceLinks: false
      hostNetwork: false
      hostPID: false
      hostIPC: false
      shareProcessNamespace: false
      hostname: openclaw
      initContainers:
        # Inject tini process manager
        - name: tini-copy
          image: krallin/ubuntu-tini:latest
          command: ["sh", "-c"]
          args:
            - |
              cp /usr/bin/tini /mnt/tini/tini
              chmod +x /mnt/tini/tini
          volumeMounts:
            - name: tini-volume
              mountPath: /mnt/tini
        # Inject Runtime (envd)
        - name: init
          image: openkruise/agent-runtime:preview-v0.0.2
          volumeMounts:
            - name: envd-volume
              mountPath: /mnt/envd
          env:
            - name: ENVD_DIR
              value: /mnt/envd
          restartPolicy: Always
      containers:
        - name: gateway
          image: ghcr.io/openclaw/openclaw:2026.3.28
          securityContext:
            readOnlyRootFilesystem: false
            runAsUser: 1000
            runAsGroup: 1000
          command: ["/mnt/tini/tini", "--"]
          args:
            - bash
            - -c
            - |
              exec node openclaw.mjs gateway run --allow-unconfigured
          ports:
            - name: gateway
              containerPort: 18789
              protocol: TCP
          env:
            - name: ENVD_DIR
              value: /mnt/envd
            - name: OPENCLAW_CONFIG_DIR
              value: /home/node/.openclaw
            - name: KUBERNETES_SERVICE_PORT_HTTPS
              value: ""
            - name: KUBERNETES_SERVICE_PORT
              value: ""
            - name: KUBERNETES_PORT_443_TCP
              value: ""
            - name: KUBERNETES_PORT_443_TCP_PROTO
              value: ""
            - name: KUBERNETES_PORT_443_TCP_ADDR
              value: ""
            - name: KUBERNETES_SERVICE_HOST
              value: ""
            - name: KUBERNETES_PORT
              value: ""
            - name: KUBERNETES_PORT_443_TCP_PORT
              value: ""
          volumeMounts:
            - name: envd-volume
              mountPath: /mnt/envd
            - name: tini-volume
              mountPath: /mnt/tini
            - name: openclaw-dir
              mountPath: /home/node/.openclaw
          resources:
            requests:
              cpu: 2
              memory: 4Gi
            limits:
              cpu: 2
              memory: 4Gi
          lifecycle:
            postStart:
              exec:
                command:
                  - bash
                  - /mnt/envd/envd-run.sh
          startupProbe:
            exec:
              command:
                - node
                - -e
                - "require('http').get('http://127.0.0.1:18789/healthz', r => process.exit(r.statusCode < 400 ? 0 : 1)).on('error', () => process.exit(1))"
            initialDelaySeconds: 2
            periodSeconds: 2
            failureThreshold: 150
      volumes:
        - name: envd-volume
          emptyDir: { }
        - name: tini-volume
          emptyDir: { }
  # Automatically create PVC for each Sandbox
  volumeClaimTemplates:
  - metadata:
      name: openclaw-dir
    spec:
      accessModes: ["ReadWriteOnce"]
      storageClassName: "openclaw-disk-sc"
      resources:
        requests:
          storage: 20Gi
```

### 2.2 Verifying Deployment

```bash
# Deploy SandboxSet
kubectl apply -f storageclass.yaml
kubectl apply -f sandboxset.yaml

# Check pre-warmed pool status
kubectl get sbs openclaw-sbs

# Expected output
NAME           REPLICAS   AVAILABLE   UPDATEREVISION   AGE
openclaw-sbs   3          3           xxxxxxxx         92s

# List available Sandboxes
kubectl get sbx -l agents.kruise.io/sandbox-pool=openclaw-sbs \
                -l agents.kruise.io/sandbox-claimed=false
```

---

## 3. Requesting OpenClaw from Pre-warmed Pool via E2B SDK

You can connect the native E2B Python SDK and JavaScript SDK to `sandbox-manager` through the following environment
variables. In this section, we will use the Python SDK as an example.

### 3.1 Environment Setup

```bash
# Install E2B SDK
pip install e2b-code-interpreter

# Configure environment variables
export E2B_DOMAIN=your.domain
export E2B_API_KEY=your-token
# If using self-signed certificates
export SSL_CERT_FILE=/path/to/ca-fullchain.pem
```

### 3.2 Creating a Sandbox and Configuring OpenClaw

Create a configuration template `openclaw-template.json`:

```json
{
    "agents": {
        "defaults": {
            "model": {
                "primary": "bailian/qwen3.5-plus"
            },
            "workspace": "/root/.openclaw/workspace"
        }
    },
    "models": {
        "mode": "merge",
        "providers": {
            "bailian": {
                "baseUrl": "https://dashscope.aliyuncs.com/compatible-mode/v1",
                "apiKey": "${DASHSCOPE_API_KEY}",
                "api": "openai-completions",
                "models": [
                    {
                        "id": "qwen3.5-plus",
                        "name": "qwen",
                        "input": ["text"],
                        "contextWindow": 1000000,
                        "maxTokens": 65536
                    }
                ]
            }
        }
    },
    "commands": {
        "native": "auto",
        "nativeSkills": "auto",
        "restart": true,
        "ownerDisplay": "raw"
    },
    "gateway": {
        "port": 18789,
        "bind": "lan",
        "controlUi": {
            "allowedOrigins": ["*"],
            "dangerouslyAllowHostHeaderOriginFallback": true,
            "allowInsecureAuth": true,
            "dangerouslyDisableDeviceAuth": true
        },
        "auth": {
            "mode": "token",
            "token": "${GATEWAY_TOKEN}"
        }
    }
}
```

Python script `create_sandbox.py`:

```python
import os
from string import Template
from e2b_code_interpreter import Sandbox

# Create Sandbox from pre-warmed pool (never-timeout keeps it running)
sbx = Sandbox.create(
    template="openclaw-sbs",
    metadata={"e2b.agents.kruise.io/never-timeout": "true"}
)
print(f"Sandbox ID: {sbx.sandbox_id}")

# Read environment variables
GATEWAY_TOKEN = os.environ.get("GATEWAY_TOKEN", "your-token")
DASHSCOPE_API_KEY = os.environ.get("DASHSCOPE_API_KEY", "sk-****")

# Render configuration template
with open("openclaw-template.json", "r") as f:
    template_content = f.read()

rendered = Template(template_content).safe_substitute(
    GATEWAY_TOKEN=GATEWAY_TOKEN,
    DASHSCOPE_API_KEY=DASHSCOPE_API_KEY,
)

# Write configuration and trigger restart (using node user)
sbx.files.write("/home/node/.openclaw/openclaw.json", rendered, user="node")
print("Configuration written. OpenClaw will automatically restart to load the new configuration.")
```

Execute:
```bash
python create_sandbox.py
```

### 3.3 Retrieving Sandbox Information

```python
print(f"Sandbox ID: {sbx.sandbox_id}")
# Format: {Namespace}--{Sandbox Name}
# Example: default--openclaw-sbs-4p4h7
```

View details via kubectl:
```bash
kubectl describe sandbox openclaw-sbs-4p4h7 -n default

# View Pod IP
# Status:
#   Pod Info:
#     Pod IP: 10.10.24.10
```

---

## 4. Accessing OpenClaw UI

### 4.1 Via E2B Native Interface

URL format:
```
https://{port}-{sandbox-id}.{domain}/#token={token}
```

Example:
```
https://18789-default--openclaw-sbs-4p4h7.your.domain/#token=abc
```

### 4.2 Via kubectl port-forward (Quick Testing)

```bash
kubectl port-forward openclaw-sbs-4p4h7 18789:18789 -n default
```

Then access in your local browser:
```
http://127.0.0.1:18789/#token=abc
```

---

## 5. Best Practices

1. **Data Persistence**: Use `Retain` reclaim policy to prevent accidental data deletion
2. **Access Control**: Tighten `allowedOrigins` and authentication settings in production environments
3. **Resource Allocation**: Adjust CPU/memory quotas based on actual workload requirements
4. **Image Versioning**: Select OpenClaw image versions based on business needs; back up data before updating images
5. **Network Isolation**: For production use, isolate OpenClaw from other workloads using vSwitch & NetworkPolicy
