# 运行 E2B 代码执行沙箱

该示例演示了如何通过 OpenKruise Agents 部署 [E2B](https://e2b.dev/) code-interpreter 沙箱并通过 E2B SDK 进行调用。

## 0. 基本概念

### Sandbox

`Sandbox` 是 OpenKruise Agents 的核心 CRD。它管理一个沙箱实例（比如一个 Pod）的生命周期，并提供
Pause、Resume、Checkpoint、Fork、原地升级
等高级功能。

### SandboxSet

`SandboxSet` 是管理 `Sandbox` 的工作负载。其作用类似管理 Pod 的 `ReplicaSet`。`SandboxSet` 通过预热一批沙箱实例以沙箱的秒级启动。
这个工作负载针对扩容性能特别优化，能够及时地补充被消耗的沙箱。

### sandbox-manager

`sandbox-manager` 是一个无状态的后端管控组件，提供了一套兼容 E2B 协议的 API，用于管理与操作沙箱实例。

### agent-runtime

`agent-runtime` 是注入到 Sandbox 中的一个 Sidecar，为沙箱提供一系列高级功能，包括兼容 E2B envd 的远程操作接口、动态 CSI
挂载等。

## 1. 定义模板

OpenKruise Agents 提供了兼容 E2B 协议的后端管控组件 `sandbox-manager`，使得用户可以直接通过原生的 E2B SDK 管理与操作沙箱。该示例中，
我们将在 K8s 集群中部署一个 E2B 官方的 code-interpreter 模板。

### 1.1 通过 SandboxSet 部署预热池

`SandboxSet` 作为管理 `Sandbox` 的工作负载，将会自动被 `sandbox-manager` 作为模板所识别。通过在 K8s 中创建如下的
`SandboxSet`，
就可以创建一个名为 `code-interpreter` 的模板。

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  annotations:
    # 启用 SandboxManager 的 Envd 初始化能力
    e2b.agents.kruise.io/should-init-envd: "true"
  name: code-interpreter
  namespace: default
spec:
  # 预热池的大小，建议比预估的请求突发量略大
  replicas: 100
  template: # 声明一个 Pod 模板
    spec:
      initContainers:
        - name: init # 通过 native sidecar 注入 agent-runtime 组件
          image: registry-cn-hangzhou.ack.aliyuncs.com/acs/agent-runtime:v0.0.1
          volumeMounts:
            - name: agent-runtime-volume
              mountPath: /mnt/agent-runtime
          env:
            - name: AGENT_RUNTIME_WORKSPACE
              value: /mnt/agent-runtime
          restartPolicy: Always
      containers:
        - name: sandbox
          image: e2bdev/code-interpreter:latest # 使用 E2B 官方的 code-interpreter 镜像
          resources:
            requests:
              cpu: 1
              memory: 1Gi
            limits:
              cpu: 1
              memory: 1Gi
          env:
            - name: AGENT_RUNTIME_WORKSPACE
              value: /mnt/agent-runtime
          volumeMounts:
            - name: agent-runtime-volume
              mountPath: /mnt/agent-runtime
          startupProbe:
            failureThreshold: 20
            httpGet: # 官方镜像中的 health 检查接口
              path: /health
              port: 49999
            initialDelaySeconds: 1
            periodSeconds: 2
            timeoutSeconds: 1
      volumes:
        - name: agent-runtime-volume # 定义 agent-runtime 与主容器的共享目录
          emptyDir: { }
```

### 1.2 使用自定义镜像

`agent-runtime` 提供了兼容 E2B 的接口，支持其命令执行、文件操作、代码运行等功能。如果官方的镜像不满足需求，您可以替换为自定义的镜像。

### 1.3 跨 Namespace 部署模板

您可以通过在多个 Namespace 中创建多个 **同名** 的 `SandboxSet` 来实现模板的跨 Namespace 部署。这在超大规模的预热场景下，可以有效降低集群压力。

## 2. 通过 E2B Python SDK 使用沙箱

您可以通过以下环境变量将原生 E2B Python SDK 与 JavaScript SDK 连接到 `sandbox-manager`。在本节中，将以 Python SDK 为例进行介绍。

> 域名与初始 API Token 请在安装时通过 helm value 配置

```shell
export E2B_DOMAIN=your.domain
export E2B_API_TOKEN=your-token
```

您可以通过以下命令安装 E2B code interpreter Python SDK:

```shell
pip install e2b-code-interpreter
```

### 2.1 E2B 标准能力

`sandbox-manager` 兼容 E2B 的标准能力，包括管控与沙箱实例操作。下面这个例子展示了通过 E2B SDK 进行创建沙箱、执行代码、操作文件、执行命令等功能。

#### 2.1.1 创建与删除

通过以下代码可以从预热池中快速分配一个沙箱实例。完成分配的同时，`SandboxSet` 将会立刻创建一个新的沙箱实例进行补充。

```python
import os

# Import the E2B SDK
from e2b_code_interpreter import Sandbox

# Create a sandbox using the E2B Python SDK
# 这里 template 名字要和 SandboxSet 名字保持一致
sbx = Sandbox.create(template="code-interpreter", timeout=300)
print(f"sandbox id: {sbx.sandbox_id}")

sbx.kill()
print(f"sandbox {sbx.sandbox_id} killed")
```

#### 2.1.2 执行代码

```python
with Sandbox.create(template="code-interpreter", timeout=300) as sbx:
    sbx.run_code("print('hello world')")
```

#### 2.1.3 操作文件

```python
with Sandbox.create(template="code-interpreter", timeout=300) as sbx:
    with open(os.path.abspath(__file__), "rb") as file:
        sbx.files.write("/home/user/my-file", file)
    file_content = sbx.files.read("/home/user/my-file")
    print(file_content)
```

#### 2.1.4 执行命令

```python
with Sandbox.create(template="code-interpreter", timeout=300) as sbx:
    result = sbx.commands.run('echo hello; sleep 1; echo world', on_stdout=lambda data: print(data), on_stderr=lambda data: print(data))
    print(result)
```

#### 2.1.5 休眠与唤醒

> 注意：目前，仅在阿里云 ACS 上支持休眠与唤醒的内存状态保留

```python
with Sandbox.create(template="code-interpreter", timeout=300) as sbx:
    # Pause the sandbox
    sbx.run_code("a = 1")
    sbx.beta_pause()
    
    # Resume the sandbox
    sbx.connect()
    sbx.run_code("print(a)")
```

### 2.2 OpenKruise Agents 扩展能力

除了 E2B 标准能力外，OpenKruise Agents 还提供了一系列扩展功能。

#### 2.2.1 原地升级

OpenKruise Agents 支持调用 `Sandbox.create` 接口时指定镜像进行原地升级，将预热的容器镜像替换为指定的镜像。这在一些强化学习场景非常有用。
使用原地升级会影响 create 接口的交付速度，可能无法在秒级完成交付。

```python
from e2b_code_interpreter import Sandbox

sbx = Sandbox.create(template="some-template", timeout=300, metadata={
    "e2b.agents.kruise.io/image": "e2bdev/code-interpreter:latest"
})
```

#### 2.2.2 动态挂载

OpenKruise Agents 支持通过 `Sandbox.create` 创建沙箱时动态挂载 CSI Volume，为每个沙箱指定单独的挂载卷（如阿里云 NAS、OSS 等）。
这个能力依赖 agent-runtime，并且也会影响交付效率。

以下的例子中，这段代码演示了如何在创建沙箱时使用动态挂载功能，将指定的CSI存储卷（`oss-pv-test`）挂载到沙箱内的`/data`目录，使沙箱环境能够访问远程存储资源。

```python
from e2b_code_interpreter import Sandbox
from kruise_agents.csi import AlibabaCloudNAS

sbx = Sandbox.create(template="some-template", timeout=300, metadata={
    "e2b.agents.kruise.io/csi-volume-name": "oss-pv-test",
    "e2b.agents.kruise.io/csi-mount-point": "/data"
})
```