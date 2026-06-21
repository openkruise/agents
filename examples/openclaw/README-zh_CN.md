# 运行 OpenClaw 沙箱

该示例演示了如何通过 OpenKruise Agents 部署 [OpenClaw](https://github.com/openclaw) 。通过 E2B SDK 从预热池中，获取 OpenClaw 实例并进行初始化。

## 0. 基本概念

### OpenClaw

`OpenClaw` 是一个开源的 AI 编程助手网关，提供统一的 LLM 接入、工具调用和代码执行能力。通过 OpenKruise Agents 部署 OpenClaw，可以为每个用户提供独立的、数据持久化的沙箱环境。

### 数据持久化

与普通的代码执行沙箱不同，OpenClaw 需要保存用户的配置文件和工作数据。本示例通过 `volumeClaimTemplates` 为每个 Sandbox 自动创建独立的云盘，确保数据不会因 Sandbox 重建而丢失。

---

## 1. 准备工作

### 1.1 配置动态存储

OpenClaw 需要持久化存储来保存用户配置和数据。首先创建 StorageClass：

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

**关键配置说明：**

| 参数 | 说明 |
|------|------|
| `provisioner` | 云盘提供方 |
| `volumeBindingMode: WaitForFirstConsumer` | 延迟绑定，先调度 Pod 再根据可用区创建云盘 |
| `reclaimPolicy: Retain` | 删除 PVC 时保留 PV 和数据，需手动清理 |


---

## 2. 部署 OpenClaw 预热池

### 2.1 通过 SandboxSet 部署

创建 SandboxSet 来部署 OpenClaw 预热池：

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
        # 注入 tini 进程管理器
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
        # 注入 Runtime（envd）
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
  # 为每个 Sandbox 自动创建 PVC
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

### 2.2 验证部署

```bash
# 部署 SandboxSet
kubectl apply -f storageclass.yaml
kubectl apply -f sandboxset.yaml

# 查看预热池状态
kubectl get sbs openclaw-sbs

# 预期输出
NAME           REPLICAS   AVAILABLE   UPDATEREVISION   AGE
openclaw-sbs   3          3           xxxxxxxx         92s

# 查看可用的 Sandbox
kubectl get sbx -l agents.kruise.io/sandbox-pool=openclaw-sbs \
                -l agents.kruise.io/sandbox-claimed=false
```

---

## 3. 通过 E2B SDK 从预热池申请 OpenClaw
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

### 3.2 创建 Sandbox 并配置 OpenClaw

创建配置模板 `openclaw-template.json`：

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

Python 脚本 `create_sandbox.py`：

```python
import os
from string import Template
from e2b_code_interpreter import Sandbox

# 从预热池创建 Sandbox（never-timeout 保持运行）
sbx = Sandbox.create(
    template="openclaw-sbs",
    metadata={"e2b.agents.kruise.io/never-timeout": "true"}
)
print(f"Sandbox ID: {sbx.sandbox_id}")

# 读取环境变量
GATEWAY_TOKEN = os.environ.get("GATEWAY_TOKEN", "your-token")
DASHSCOPE_API_KEY = os.environ.get("DASHSCOPE_API_KEY", "sk-****")

# 渲染配置模板
with open("openclaw-template.json", "r") as f:
    template_content = f.read()

rendered = Template(template_content).safe_substitute(
    GATEWAY_TOKEN=GATEWAY_TOKEN,
    DASHSCOPE_API_KEY=DASHSCOPE_API_KEY,
)

# 写入配置并触发重启（使用 node 用户）
sbx.files.write("/home/node/.openclaw/openclaw.json", rendered, user="node")
print("配置已写入，OpenClaw 将自动重启加载新配置")
```

执行：
```bash
python create_sandbox.py
```

### 3.3 为每个用户挂载独立的 NFS 子路径

当多个 OpenClaw 实例共享同一个大型 NFS 导出目录时，可以通过 `SandboxClaim` 为每个用户挂载不同的目录。
这种场景应使用 `dynamicVolumesMount`；如果每个 Sandbox 都需要独立供应的存储卷，仍建议使用
`volumeClaimTemplates`。

创建 Claim 前，请确认：

- NFS 卷由 **CSI 类型的 PV**（`spec.csi`）表示，而不是旧式的 `spec.nfs` PV；
- sandbox-manager 已通过 `DYNAMIC_STORAGE_DRIVER_LIST` 启用该 PV 的 CSI 驱动，并且 Sandbox 已配置对应的
  CSI sidecar；
- SandboxSet 已配置 `agent-runtime` 和 `csi` runtime；
- 如果 NFS CSI 驱动不会自动创建远端用户目录，需要提前创建该目录。

对于使用 runtime 注入的资源池，SandboxSet 中的相关配置如下：

```yaml
spec:
  runtimes:
    - name: agent-runtime
    - name: csi
```

集群管理员需要根据已安装的 NFS CSI 驱动，在 `sandbox-injection-config` ConfigMap 中配置
`agent-runtime` 和 `csi`。前文的 SandboxSet 使用了旧式 runtime init container，因此添加上述配置前，
需要先将其替换为 runtime 注入方式。请勿在同一个 Sandbox 中同时使用两种 runtime 配置。

假设名为 `openclaw-shared-nfs` 的 CSI PV 基础路径为 `/exports/openclaw`，以下 Claim 会将
`/exports/openclaw/users/alice` 挂载到 OpenClaw 的工作目录：

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxClaim
metadata:
  name: openclaw-alice
  namespace: default
spec:
  templateName: openclaw-sbs
  replicas: 1
  dynamicVolumesMount:
    - pvName: openclaw-shared-nfs
      mountPath: /home/node/.openclaw/workspace
      subPath: users/alice
      readOnly: false
```

应用 Claim 并等待完成：

```bash
kubectl apply -f sandboxclaim-alice.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Completed \
  sandboxclaim/openclaw-alice --timeout=2m
kubectl get sandbox \
  -l agents.kruise.io/claim-name=openclaw-alice
```

后续 Claim 应使用唯一的 `subPath`，例如 `users/bob`。该路径会相对于 PV 的基础路径解析；试图跳出基础路径的值
（例如 `../other-user`）会被拒绝。系统会把子路径追加到 PV 的 `spec.csi.volumeAttributes.path`，因此请使用
能够识别 `path` 属性的 CSI 驱动，并根据该驱动的要求调整 PV 属性。

> **安全提示：** 不同子路径可以避免意外的数据重叠，但不能作为授权边界。对于不受信任的租户，还需通过
> NFS 导出策略、身份和文件权限实施隔离。

### 3.4 获取 Sandbox 信息

```python
print(f"Sandbox ID: {sbx.sandbox_id}")
# 格式: {Namespace}--{Sandbox Name}
# 示例: default--openclaw-sbs-4p4h7
```

通过 kubectl 查看详情：
```bash
kubectl describe sandbox openclaw-sbs-4p4h7 -n default

# 查看 Pod IP
# Status:
#   Pod Info:
#     Pod IP: 10.10.24.10
```

---

## 4. 访问 OpenClaw UI

### 4.1 通过 E2B 原生接口访问

URL 格式：
```
https://{port}-{sandbox-id}.{domain}/#token={token}
```

示例：
```
https://18789-default--openclaw-sbs-4p4h7.your.domain/#token=abc
```


### 4.2 通过 kubectl port-forward 访问（测试快速使用）

```bash
kubectl port-forward openclaw-sbs-4p4h7 18789:18789 -n default
```

然后在本地浏览器访问：
```
http://127.0.0.1:18789/#token=abc
```

---

## 5. 最佳实践建议

1. **数据持久化**：使用 `Retain` 回收策略，避免误删用户数据
2. **权限控制**：生产环境请收紧 `allowedOrigins` 和认证配置
3. **资源配置**：根据实际负载调整 CPU/内存配额
4. **镜像版本**：根据业务需求选择 OpenClaw 镜像版本，更新镜像时，注意备份数据
5. **网络隔离**：生产使用还需要通过 vSwitch & NetWork Policy 等，将 OpenClaw 与其他业务网络隔离
