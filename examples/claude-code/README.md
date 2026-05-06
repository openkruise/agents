# Running Claude Code Sandbox

This example demonstrates how to deploy [Claude Code](https://github.com/anthropics/claude-code) via OpenKruise Agents. Use the E2B SDK to obtain a Claude Code instance from the warm pool and execute programming tasks.

## 0. Basic Concepts

### Claude Code

`Claude Code` is an AI programming assistant tool by Anthropic, supporting autonomous code generation, modification, and debugging. By deploying Claude Code with OpenKruise Agents, you can provide each user with an independent, securely isolated sandbox environment for programming tasks.

---

## 1. Prerequisites

### 1.1 Obtain API Key

Using Claude Code requires an Anthropic API Key:

```bash
export ANTHROPIC_API_KEY="****"
```

If using a custom model provider, you need to provide the following environment variables:

```bash
export ANTHROPIC_API_KEY="sk-****"
export ANTHROPIC_BASE_URL="https://dashscope.aliyuncs.com/apps/anthropic"
export ANTHROPIC_MODEL="qwen3.6-plus"
export ANTHROPIC_DEFAULT_OPUS_MODEL="qwen3.6-plus"
export ANTHROPIC_DEFAULT_SONNET_MODEL="qwen3.6-plus"
export ANTHROPIC_DEFAULT_HAIKU_MODEL="qwen3.6-flash"
export CLAUDE_CODE_SUBAGENT_MODEL="qwen3.6-flash"
export CLAUDE_CODE_EFFORT_LEVEL="max"
```

---

## 2. Deploy Claude Code Warm Pool

### 2.1 Deploy via SandboxSet

Create a SandboxSet to deploy the Claude Code warm pool:

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  name: claude-code-sbs
  namespace: default
spec:
  replicas: 3
  runtimes:
  - name: agent-runtime
  template:
    metadata:
      labels:
        app: claude-code
    spec:
      containers:
        - name: claude-code
          image: registry-ap-southeast-1.ack.aliyuncs.com/acs/code-interpreter:v1.6
          resources:
            requests:
              cpu: 2
              memory: 4Gi
            limits:
              cpu: 2
              memory: 4Gi
          startupProbe:
            failureThreshold: 20
            httpGet:
              path: /health
              port: 49999
            initialDelaySeconds: 1
            periodSeconds: 2
            timeoutSeconds: 1
```

### 2.2 Verify Deployment

```bash
# Deploy SandboxSet
kubectl apply -f sandboxset.yaml

# Check warm pool status
kubectl get sbs claude-code-sbs

# Expected output
NAME              REPLICAS   AVAILABLE   UPDATEREVISION   AGE
claude-code-sbs   3          3           xxxxxxxx         2m

# Check available Sandboxes
kubectl get sbx -l agents.kruise.io/sandbox-pool=claude-code-sbs \
                -l agents.kruise.io/sandbox-claimed=false
```

---

## 3. Use Claude Code via E2B SDK

You can connect the native E2B Python SDK or JavaScript SDK to `sandbox-manager` using the following environment variables. This section uses the Python SDK as an example.

### 3.1 Initialize Environment

```bash
# Install E2B SDK
pip install e2b-code-interpreter

# Configure environment variables
export E2B_DOMAIN=your.domain
export E2B_API_KEY=your-token
# If using self-signed certificates
export SSL_CERT_FILE=/path/to/ca-fullchain.pem
```

### 3.2 Claim Resources & Install Claude Code

```python
from e2b_code_interpreter import Sandbox
# Claim a sandbox instance
sandbox = Sandbox.create("claude-code-sbs", timeout=3600)
print(sandbox.sandbox_id)
```

```python
# Install Claude Code (optional)
install_exec = sandbox.commands.run("npm install -g @anthropic-ai/claude-code@latest", user="root")
print(install_exec)
```

### 3.3 Interact with Claude Code

First interaction:

```python
import os
import json

envs = {
    "ANTHROPIC_API_KEY": os.environ["ANTHROPIC_API_KEY"],
    "ANTHROPIC_BASE_URL": os.environ["ANTHROPIC_BASE_URL"],
    "ANTHROPIC_MODEL": os.environ["ANTHROPIC_MODEL"],
    "ANTHROPIC_DEFAULT_OPUS_MODEL": os.environ["ANTHROPIC_DEFAULT_OPUS_MODEL"],
    "ANTHROPIC_DEFAULT_SONNET_MODEL": os.environ["ANTHROPIC_DEFAULT_SONNET_MODEL"],
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": os.environ["ANTHROPIC_DEFAULT_HAIKU_MODEL"],
    "CLAUDE_CODE_SUBAGENT_MODEL": os.environ["CLAUDE_CODE_SUBAGENT_MODEL"],
    "CLAUDE_CODE_EFFORT_LEVEL": os.environ["CLAUDE_CODE_EFFORT_LEVEL"],
}

run_exec = sandbox.commands.run(
            'claude --dangerously-skip-permissions --output-format json -p "Who are you?"',
            envs=envs,
            timeout=360,
        )
session_id=json.loads(run_exec.stdout)["session_id"]
print("session_id: ", session_id)
print(run_exec)
```

Continue the conversation using `session_id`:

```python
run_exec = sandbox.commands.run(
            f'claude --dangerously-skip-permissions --output-format json --resume {session_id} -p "Translate your previous answer into Chinese"',
            envs=envs,
            timeout=360,
        )
print(run_exec)
```

## 4. Best Practices

1. **Pre-install in Image**: It is recommended to install Claude Code directly into a custom image to avoid installing at runtime (`npm install -g @anthropic-ai/claude-code@latest`) which adds extra latency
2. **Timeout Configuration**: Set `timeout` based on the expected duration of Claude Code tasks (e.g., 600 seconds)
3. **Resource Configuration**: Adjust CPU/memory quotas based on task complexity; 2C4G or above is recommended for complex programming tasks
4. **Session Reuse**: For multi-step tasks, use `--resume` to reuse sessions and avoid redundant context loading
5. **Network Isolation**: For production use, isolate Claude Code from other workloads via vSwitch & NetworkPolicy
