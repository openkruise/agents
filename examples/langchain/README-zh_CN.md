# 基于 Sandbox 部署 LangChain Agent 服务

该示例演示了如何基于 [Aegra](https://github.com/aegra-ai/aegra) 框架开发 LangChain Agent 应用，并通过 OpenKruise Agents 的 Sandbox CRD 将其部署到 Kubernetes 集群。

## 0. 背景

### Sandbox

`Sandbox` 是 OpenKruise Agents 的核心 CRD。它管理一个沙箱实例（比如一个 Pod）的生命周期，并提供Pause、Resume、Checkpoint、Fork、原地升级等高级功能。

### SandboxSet

`SandboxSet` 是管理 `Sandbox` 的工作负载。其作用类似管理 Pod 的 `ReplicaSet`。`SandboxSet` 通过预热一批沙箱实例以沙箱的秒级启动。这个工作负载针对扩容性能特别优化，能够及时地补充被消耗的沙箱。

### Aegra

LangGraph 官方的 LangSmith Deployments 在自托管场景下需要 Enterprise 许可证，且免费版仅支持本地开发。Aegra 是一个 Apache 2.0 开源的 Agent 服务框架。

Aegra 承担两个职责：

1. **开发脚手架**：通过 `aegra init` 生成项目骨架（状态定义、工具、上下文配置等），`aegra dev` 一键启动 PostgreSQL + 开发服务器
2. **在线 Server**：生产环境中作为 HTTP 服务入口，通过 `aegra.json` 加载实际的 Agent Graph，对外暴露 API 端口

实际的 Agent 逻辑（模型调用、工具编排、状态流转）完全基于 LangChain / LangGraph 实现。

## 1. 本地开发

### 1.1 环境准备

```bash
# 安装 aegra-cli
pip install aegra-cli

```

### 1.2 本地开发Agent服务器

```bash
# 创建项目根路径
mkdir langchain-test

# 使用aegra初始化项目agent
aegra init
# 预期输出
📂 Where should I create the project? [.]: 

🌟 Choose a template:
  1. New Aegra Project — A simple chatbot with message memory.
  2. ReAct Agent — An agent with tools that reasons and acts step by step.

Enter your choice [1]: 2
You selected: ReAct Agent — An agent with tools that reasons and acts step by step.
...

```

执行完成后，本地的项目结构类似

```bash

➜  langchain-test tree
# 预期输出
.
├── Dockerfile                 # 多阶段构建的部署镜像
├── README.md
├── aegra.json                 # Aegra 入口配置，指定 Graph 加载路径
├── docker-compose.yml         # 本地开发用 PostgreSQL + API 编排
├── pyproject.toml             # Python 项目元数据与依赖声明
└── src/langchain_test/        # Agent 核心代码
    ├── __init__.py
    ├── context.py             # 运行时上下文（模型、Prompt 可配置）
    ├── graph.py               # LangGraph StateGraph 定义与编译
    ├── prompts.py             # System Prompt 模板
    ├── state.py               # 输入/内部状态 dataclass
    ├── tools.py               # LangChain @tool 定义
    └── utils.py               # 模型加载工具（支持 OpenAI/Anthropic/DashScope）
```

在上文中的 `src/langchain_test/utils.py` 可以自定义模型提供方的信息。以修改成DashScope（百炼）为例

```python
from langchain_openai import ChatOpenAI

def load_chat_model(fully_specified_name: str) -> BaseChatModel:
    ...
    if provider == "dashscope":
        return ChatOpenAI(
            model=model,
            base_url="https://dashscope.aliyuncs.com/compatible-mode/v1",
            api_key=os.getenv("DASHSCOPE_API_KEY"),
        )
```

新增`.env`配置文件，或者直接使用`export k=v`进行环境配置，内容如下

```text

# --- LLM Providers ---
DASHSCOPE_API_KEY=sk-****
MODEL=dashscope/qwen3.5-plus

# --- Database ---
POSTGRES_DB=langchain_test
POSTGRES_HOST=localhost
POSTGRES_PASSWORD=langchain_test_secret
POSTGRES_PORT=5432
POSTGRES_USER=langchain_test

# --- Server Configuration ---
HOST=0.0.0.0
PORT=8000
SERVER_URL=http://localhost:8000

```

本地启动Agent服务进行测试

```bash
uv sync
# 本地启动Agent Server和持久化存储Server
uv run aegra dev
```

使用客户端访问本地Server

```python
import asyncio
from langgraph_sdk import get_client


async def main():
    client = get_client(url="http://localhost:8000")
    thread = await client.threads.create()
    async for chunk in client.runs.stream(
        thread_id=thread["thread_id"],
        assistant_id="langchain_test",
        input={"messages": [{"type": "human", "content": "8888 + 8888 的结果是什么"}]},
    ):
        print(chunk)

asyncio.run(main())
```

## 2. 构建部署镜像

本地Agent代码修改完成后，需要将Agent服务整体打包成镜像，用户服务部署：

```bash
docker build  --platform linux/amd64 -t your-registry/agent_app:langchain-v1 .
```

构建完成后推送镜像：

```bash
docker push your-registry/agent_app:langchain-v1
```

## 3. Kubernetes 部署

部署包含两个组件：PostgreSQL 持久化存储和 Agent 工作负载。

### 3.1 部署 PostgreSQL

PostgreSQL 使用 `pgvector/pgvector:pg18` 镜像。

```bash
kubectl apply -f postgres.yaml
```

**postgres.yaml 资源清单：**

| 资源 | 说明 |
|:--|:--|
| `Secret/postgres-secret` | 存储 DB 用户名、密码、数据库名 |
| `Deployment/postgres` | 单副本 PostgreSQL，含 readiness/liveness 探针 |
| `Service/postgres` | ClusterIP 服务，集群内通过 `postgres:5432` 访问 |

> **存储策略**：当前使用 `emptyDir`（测试环境，Pod 重建数据丢失）。生产环境建议使用 PVC。

验证 PostgreSQL 就绪：

```bash
kubectl get pods -l app=postgres

```

### 3.2 部署 Agent 工作负载

Agent 使用 OpenKruise Agents 的 `Sandbox` CRD 部署，相比原生 Deployment 提供 Pause/Resume/Checkpoint 等沙箱管理能力。

```bash
kubectl apply -f k8s/sdx-langchain-dev.yaml
```

**sdx-langchain-dev.yaml 资源清单：**

| 资源 | 说明 |
|:--|:--|
| `Secret/langchain-dev-secret` | LLM API Key、DATABASE_URL |
| `ConfigMap/langchain-dev-config` | 模型名称、服务地址与端口 |
| `Sandbox/langchain-dev` | Agent 工作负载，挂载上述 Secret/ConfigMap |
| `Service/langchain-dev` | ClusterIP 服务，暴露 8000 端口 |

**Sandbox spec 核心配置：**

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: Sandbox
metadata:
  name: langchain-dev
spec:
  template:
    spec:
      containers:
        - name: aegra
          image: your-registry/agent_app:langchain-v1
          command: ["aegra", "serve", "--host", "0.0.0.0", "--port", "8000"]
          ports:
            - containerPort: 8000
          envFrom:
            - configMapRef:
                name: langchain-dev-config
            - secretRef:
                name: langchain-dev-secret
```

- `command` 直接调用 `aegra serve` 启动 HTTP 服务
- 通过 `envFrom` 注入配置和密钥，Agent 运行时自动读取 `MODEL`、`DATABASE_URL` 等变量
- PostgreSQL 连接信息从 `postgres-secret` 引用，与 postgres.yaml 中的 Secret 保持一致

### 3.3 验证部署

```bash
# 检查 Pod 状态
kubectl get sandbox langchain-dev
kubectl get pods -l app=langchain-dev

# 查看日志
kubectl logs -l app=langchain-dev --tail=50

# 将集群内服务转发到本地
kubectl port-forward svc/langchain-dev 8000:8000

# 本地访问测试
curl -s http://localhost:8000/health
```

使用Python客户端访问集群Agent Server

```python
import asyncio
from langgraph_sdk import get_client


async def main():
    client = get_client(url="http://localhost:8000")
    thread = await client.threads.create()
    async for chunk in client.runs.stream(
        thread_id=thread["thread_id"],
        assistant_id="langchain_test",
        input={"messages": [{"type": "human", "content": "8888 + 8888 的结果是什么"}]},
    ):
        print(chunk)

asyncio.run(main())
```


### 3.4 外部访问

当前 Service 类型为 `ClusterIP`，仅集群内可达。如需外部访问，可选择：

```yaml
# 方案 1：NodePort
spec:
  type: NodePort
  ports:
    - port: 8000
      targetPort: 8000
      nodePort: 30800

# 方案 2：配合 Ingress
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: langchain-test-ingress
spec:
  rules:
    - host: langchain-test.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: langchain-test
                port:
                  number: 8000
```

## 4. 完整部署流程总结

```
本地开发                        构建镜像                    K8s 部署
─────────                      ─────────                  ─────────
aegra init                     docker build               kubectl apply -f postgres.yaml
配置 .env / export 环境变量     docker push                kubectl apply -f sdx-langchain-dev.yaml
uv sync                                                   kubectl get sandbox langchain-test
uv run aegra dev
  ↓
localhost:8000 调试验证
```

**关键依赖关系：**
- Agent 工作负载依赖 PostgreSQL 先就绪（checkpoint 存储）
- Sandbox CRD 依赖集群中已安装 OpenKruise Agents Operator
- 镜像中 `aegra.json` 决定加载哪个 Graph
