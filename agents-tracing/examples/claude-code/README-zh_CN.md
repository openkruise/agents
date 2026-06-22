# 运行 Claude Code 沙箱

该示例演示了如何通过 OpenKruise Agents 部署 [Claude Code](https://github.com/anthropics/claude-code)。通过 E2B SDK 从预热池中，获取 Claude Code 实例并执行编程任务。

## 0. 基本概念

### Claude Code

`Claude Code` 是 Anthropic 推出的 AI 编程助手工具，支持自主代码生成、修改和调试。通过 OpenKruise Agents 部署 Claude Code，可以为每个用户提供独立的、安全隔离的沙箱环境来执行编程任务。

---

## 1. 准备工作

### 1.1 获取 API Key

使用 Claude Code 需要 Anthropic API Key：

```bash
export ANTHROPIC_API_KEY="****"
```

如果使用自定义大模型提供方，则需要提供如下环境变量

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

## 2. 部署 Claude Code 预热池

### 2.1 通过 SandboxSet 部署

创建 SandboxSet 来部署 Claude Code 预热池：

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

### 2.2 验证部署

```bash
# 部署 SandboxSet
kubectl apply -f sandboxset.yaml

# 查看预热池状态
kubectl get sbs claude-code-sbs

# 预期输出
NAME              REPLICAS   AVAILABLE   UPDATEREVISION   AGE
claude-code-sbs   3          3           xxxxxxxx         2m

# 查看可用的 Sandbox
kubectl get sbx -l agents.kruise.io/sandbox-pool=claude-code-sbs \
                -l agents.kruise.io/sandbox-claimed=false
```

---

## 3. 通过 E2B SDK 使用 Claude Code

你可以通过以下环境变量将原生 E2B Python SDK 与 JavaScript SDK 连接到 `sandbox-manager`。在本节中，将以 Python SDK 为例进行介绍。

### 3.1 初始化环境

```bash
# 安装 E2B SDK
pip install e2b-code-interpreter

# 配置环境变量
export E2B_DOMAIN=your.domain
export E2B_API_KEY=your-token
# 如使用自签名证书
export SSL_CERT_FILE=/path/to/ca-fullchain.pem
```

### 3.2 申请资源 & 安装 Claude Code

```python
from e2b_code_interpreter import Sandbox
# 申请物理实例
sandbox = Sandbox.create("claude-code-sbs", timeout=3600)
print(sandbox.sandbox_id)
```

```python
# 安装 Claude Code（按需）
install_exec = sandbox.commands.run("npm install -g @anthropic-ai/claude-code@latest", user="root")
print(install_exec)
```

### 3.3 与 Claude Code 交互

首次交互

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
            'claude --dangerously-skip-permissions --output-format json -p "你是谁?"',
            envs=envs,
            timeout=360,
        )
session_id=json.loads(run_exec.stdout)["session_id"]
print("session_id: ", session_id)
print(run_exec)
```

根据`session_id`继续对话

```python
run_exec = sandbox.commands.run(
            f'claude --dangerously-skip-permissions --output-format json --resume {session_id} -p "将上述回答翻译成英文"',
            envs=envs,
            timeout=360,
        )
print(run_exec)
```

## 4. 最佳实践建议

1. **镜像预装**：建议将 Claude Code 直接安装到自定义镜像中，避免在使用时安装（`npm install -g @anthropic-ai/claude-code@latest`）增加额外耗时
2. **超时配置**：根据 Claude Code 任务预期耗时，设置 `timeout`（如 600 秒）
3. **资源配置**：根据任务复杂度调整 CPU/内存配额，复杂编程任务建议 2C4G 以上
4. **会话复用**：对于多步骤任务，使用 `--resume` 复用会话，避免重复上下文加载
5. **网络隔离**：生产使用还需要通过 vSwitch & NetworkPolicy 等，将 Claude Code 与其他业务网络隔离
