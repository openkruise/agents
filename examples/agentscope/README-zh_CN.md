# 基于 Sandbox 部署 AgentScope 应用

该示例演示了如何使用 [AgentScope](https://github.com/modelscope/agentscope) 框架开发 Agent 应用，并通过 [AgentScope Runtime](https://github.com/modelscope/agentscope-runtime) 和 OpenKruise Agents 的 Sandbox CRD 将其部署到 Kubernetes 集群。

## 0. 背景

### Sandbox

`Sandbox` 是 OpenKruise Agents 的核心 CRD。它管理一个沙箱实例（比如一个 Pod）的生命周期，并提供 Pause、Resume、Checkpoint、Fork、原地升级等高级功能。

### SandboxSet

`SandboxSet` 是管理 `Sandbox` 的工作负载。其作用类似管理 Pod 的 `ReplicaSet`。`SandboxSet` 通过预热一批沙箱实例实现沙箱的秒级启动。这个工作负载针对扩容性能特别优化，能够及时地补充被消耗的沙箱。

### SandboxClaim

`SandboxClaim` 用于从 `SandboxSet` 预热池中申请（Claim）已有的沙箱资源。相比从头创建新的 Sandbox，通过 Claim 获取预热好的沙箱可以实现秒级甚至亚秒级交付。

## 1. 环境准备

### 1.1 安装依赖

```bash
# 安装 agentscope-runtime（含扩展依赖）
pip install "agentscope-runtime[ext]>=1.0.0"
```

### 1.2 配置环境变量

```bash
# LLM API Key
export DASHSCOPE_API_KEY="your-api-key"

# 镜像仓库地址（可选）
export REGISTRY_URL="your-registry-url"
```

### 1.3 验证 Kubernetes 访问

```bash
# 验证集群连接
kubectl cluster-info

# 检查 Kruise Sandbox CRD 已安装
kubectl get crd sandboxes.agents.kruise.io
kubectl get crd sandboxsets.agents.kruise.io
```

## 2. 定义 Agent 应用

Agent 应用基于 `agentscope_runtime.engine.app.AgentApp` 定义，核心组件包括：

```python
from agentscope.agent import ReActAgent
from agentscope.model import DashScopeChatModel
from agentscope.tool import Toolkit, execute_python_code
from agentscope_runtime.engine.app import AgentApp
from agentscope_runtime.engine.schemas.agent_schemas import AgentRequest

# 创建 AgentApp 实例
agent_app = AgentApp(
    app_name="Friday",
    app_description="A helpful assistant",
)


# 初始化函数：配置 Session 存储
@agent_app.init
async def init_func(self):
    import fakeredis
    fake_redis = fakeredis.aioredis.FakeRedis(decode_responses=True)
    self.session = RedisSession(connection_pool=fake_redis.connection_pool)


# 核心查询函数：处理用户消息
@agent_app.query(framework="agentscope")
async def query_func(self, msgs, request: AgentRequest = None, **kwargs):
    session_id = request.session_id
    user_id = request.user_id

    # 配置工具集
    toolkit = Toolkit()
    toolkit.register_tool_function(execute_python_code)

    # 创建 ReAct Agent
    agent = ReActAgent(
        name="Friday",
        model=DashScopeChatModel(
            "qwen-turbo",
            api_key=os.getenv("DASHSCOPE_API_KEY"),
            stream=True,
        ),
        sys_prompt="You're a helpful assistant named Friday.",
        toolkit=toolkit,
    )

    # 加载会话状态
    await self.session.load_session_state(session_id, user_id, agent)

    # 流式输出
    async for msg, last in stream_printing_messages(
        agents=[agent], coroutine_task=agent(msgs)
    ):
        yield msg, last

    # 保存会话状态
    await self.session.save_session_state(session_id, user_id, agent)


# 自定义端点
@agent_app.endpoint("/sync")
def sync_handler(request: AgentRequest):
    yield {"status": "ok", "payload": request}


@agent_app.endpoint("/stream_async")
async def stream_async_handler(request: AgentRequest):
    for i in range(5):
        yield f"async chunk {i}\n"
```

**核心装饰器说明：**

| 装饰器 | 用途 |
|:--|:--|
| `@agent_app.init` | 初始化函数，配置 Session/DB 连接等 |
| `@agent_app.query` | 核心查询处理，支持流式输出 |
| `@agent_app.endpoint` | 自定义 HTTP 端点 |
| `@agent_app.task` | 异步任务（支持 Celery 队列） |

完整代码参考 [app_deploy_to_kruise.py](app_deploy_to_kruise.py)

## 3. 方式一：直接部署（Sandbox）

适用于单实例场景，创建独立的 Sandbox 资源。

参考完整脚本：[app_deploy_to_kruise.py](app_deploy_to_kruise.py)

```python
# app_deploy_to_kruise.py
import asyncio
import os

from agent_app import app  # 导入共享的 agent app 定义

from agentscope_runtime.engine.deployers.kruise_deployer import (
    KruiseDeployManager,
    K8sConfig,
)
from agentscope_runtime.engine.deployers.utils.docker_image_utils import (
    RegistryConfig,
)


async def deploy_to_kruise():
    """将 AgentApp 部署到 Kruise Sandbox"""

    # 1. 配置镜像仓库
    registry_config = RegistryConfig(
        registry_url=os.environ.get("REGISTRY_URL", "your-registry-url"),
        namespace="agentscope-runtime",
    )

    # 2. 配置 K8s 连接
    k8s_config = K8sConfig(
        k8s_namespace="agentscope-runtime",
        kubeconfig_path=None,
    )

    # 3. 创建 KruiseDeployManager
    deployer = KruiseDeployManager(
        kube_config=k8s_config,
        registry_config=registry_config,
    )

    # 4. 运行时配置
    runtime_config = {
        "resources": {
            "requests": {"cpu": "200m", "memory": "512Mi"},
            "limits": {"cpu": "1000m", "memory": "2Gi"},
        },
        "image_pull_policy": "IfNotPresent",
    }

    # 5. 部署配置
    kruise_config = {
        "port": "8080",
        "image_name": "agent_app",
        "image_tag": "v1",
        "base_image": "python:3.10-slim-bookworm",
        "requirements": [
            "agentscope",
            "fastapi",
            "uvicorn",
            "fakeredis",
        ],
        "environment": {
            "PYTHONPATH": "/app",
            "LOG_LEVEL": "INFO",
            "DASHSCOPE_API_KEY": os.environ.get("DASHSCOPE_API_KEY"),
        },
        "runtime_config": runtime_config,
        "platform": "linux/amd64",
        "push_to_registry": True,
    }

    # 6. 执行部署
    print("🚀 Starting deployment...")
    result = await app.deploy(deployer, **kruise_config)

    print("✅ 部署成功!")
    print(f"   Deploy ID: {result['deploy_id']}")
    print(f"   Service URL: {result['url']}")
    print(f"   Resource name: {result['resource_name']}")

    return result, deployer


if __name__ == "__main__":
    asyncio.run(deploy_to_kruise())
```

## 4. 方式二：预热池部署（SandboxSet + SandboxClaim）

适用于需要快速启动多实例的场景，预热池可实现秒级 Agent 交付。

参考完整脚本：[app_deploy_with_warm_pool.py](app_deploy_with_warm_pool.py)

### 4.1 创建预热池

```python
# app_deploy_with_warm_pool.py
import asyncio
import os

from agent_app import app  # 导入共享的 agent app 定义

from agentscope_runtime.engine.deployers.kruise_deployer import (
    KruiseDeployManager,
    K8sConfig,
)
from agentscope_runtime.engine.deployers.utils.docker_image_utils import (
    RegistryConfig,
)

# 配置
REGISTRY_CONFIG = RegistryConfig(
    registry_url=os.environ.get("REGISTRY_URL", "your-registry-url"),
    namespace="agentscope-runtime",
)

K8S_CONFIG = K8sConfig(
    k8s_namespace="agentscope-runtime",
    kubeconfig_path=None,
)

POOL_CONFIG = {
    "pool_name": "friday-pool",
    "pool_replicas": 3,  # 维护 3 个预热实例
    "port": 8080,
    "image_name": "friday-agent",
    "image_tag": "v1",
    "base_image": "python:3.10-slim-bookworm",
    "requirements": [
        "agentscope",
        "fastapi",
        "uvicorn",
        "fakeredis",
    ],
    "environment": {
        "PYTHONPATH": "/app",
        "LOG_LEVEL": "INFO",
        "DASHSCOPE_API_KEY": os.environ.get("DASHSCOPE_API_KEY"),
    },
    "runtime_config": {
        "resources": {
            "requests": {"cpu": "200m", "memory": "512Mi"},
            "limits": {"cpu": "1000m", "memory": "2Gi"},
        },
    },
    "push_to_registry": True,
    "platform": "linux/amd64",
}


async def create_warm_pool(deployer: KruiseDeployManager):
    """创建 Agent 预热池（SandboxSet）"""

    print("🚀 Creating warm pool (SandboxSet)...")

    # 构建 app 部署参数
    app_deploy_kwargs = {
        "app": app,
        "custom_endpoints": app.custom_endpoints,
        "runner": app._runner,
        "endpoint_path": app.endpoint_path,
        "stream": app.stream,
        "protocol_adapters": app.protocol_adapters,
    }
    app_deploy_kwargs.update(**POOL_CONFIG)

    result = await deployer.deploy_warm_pool(**app_deploy_kwargs)

    print("✅ 预热池创建成功!")
    print(f"   Pool name: {result['pool_name']}")
    print(f"   Replicas: {result['replicas']}")
    print(f"   Status: {result['status']}")

    return result
```

### 4.2 从预热池申请 Sandbox

```python
async def claim_sandbox_for_user(
    deployer: KruiseDeployManager,
    pool_name: str,
    user_id: str,
):
    """从预热池申请一个 Sandbox（秒级交付）"""

    print(f"🎯 Claiming sandbox for user '{user_id}'...")

    result = await deployer.claim_from_pool(
        pool_name=pool_name,
        claim_name=f"user-{user_id}",
        env_vars={
            "USER_ID": user_id,
        },
        labels={
            "user": user_id,
        },
        port=8080,
        claim_timeout="60s",
    )

    print("✅ Sandbox 申请成功!")
    print(f"   Deploy ID: {result['deploy_id']}")
    print(f"   Claim name: {result['claim_name']}")
    print(f"   Sandbox name: {result['sandbox_name']}")
    print(f"   Service URL: {result['url']}")

    return result
```

### 4.3 释放 Sandbox

```python
async def release_user_sandbox(deployer: KruiseDeployManager, deploy_id: str):
    """释放已申请的 Sandbox"""

    print("🧹 Releasing sandbox...")
    result = await deployer.release_claim(deploy_id)

    if result["success"]:
        print(f"✅ Sandbox 已释放: {result['message']}")
    else:
        print(f"❌ 释放失败: {result['message']}")

    return result
```

### 4.4 删除预热池

```python
async def delete_warm_pool(deployer: KruiseDeployManager, pool_name: str):
    """删除预热池（SandboxSet）"""

    print(f"🗑️ Deleting warm pool '{pool_name}'...")
    result = await deployer.delete_warm_pool(pool_name)

    if result["success"]:
        print(f"✅ 预热池已删除: {result['message']}")
    else:
        print(f"❌ 删除失败: {result['message']}")

    return result
```

## 5. 验证部署

```bash
# 检查 Sandbox 状态
kubectl get sandbox -n agentscope-runtime

# 检查 SandboxSet 状态（预热池）
kubectl get sandboxsets -n agentscope-runtime

# 检查 SandboxClaim 状态
kubectl get sandboxclaims -n agentscope-runtime

# 查看 Pod 日志
kubectl logs -l app=agentscope-agent -n agentscope-runtime --tail=50

# 端口转发测试
kubectl port-forward svc/agentscope-agent 8080:8080 -n agentscope-runtime

# 健康检查
curl http://localhost:8080/health

# 测试同步端点
curl -X POST http://localhost:8080/sync \
  -H "Content-Type: application/json" \
  -d '{"input": [{"role": "user", "content": [{"type": "text", "text": "Hello!"}]}], "session_id": "123"}'

# 测试流式端点
curl -X POST http://localhost:8080/stream_async \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  --no-buffer \
  -d '{"input": [{"role": "user", "content": [{"type": "text", "text": "Hello!"}]}], "session_id": "123"}'
```

## 6. 清理资源

```bash
# 使用 CLI 停止部署
asrt stop <deploy_id>

# 或手动删除
kubectl delete sandbox agentscope-agent -n agentscope-runtime
kubectl delete svc agentscope-agent -n agentscope-runtime

# 删除预热池
kubectl delete sandboxset agentscope-pool -n agentscope-runtime
```

## 7. 两种部署模式对比

| 特性 | 直接部署 (Sandbox) | 预热池部署 (SandboxSet) |
|:--|:--|:--|
| 启动速度 | 分钟级 | 秒级 |
| 资源利用 | 按需创建 | 预热占用 |
| 适用场景 | 单实例、长期运行 | 多租户、按需扩展 |
| 管理复杂度 | 简单 | 需管理池大小 |

## 8. 完整部署流程总结

### 直接部署模式

```
定义 Agent                     构建镜像                    K8s 部署
─────────                      ─────────                  ─────────
定义 AgentApp                  docker build               kubectl apply -f sandbox.yaml
配置 @query/@endpoint          docker push                kubectl get sandbox
python app_deploy_to_kruise.py                            kubectl port-forward
  ↓
localhost:8080 调试验证
```

### 预热池模式

```
创建预热池                      用户申请                    释放/清理
─────────                      ─────────                  ─────────
kubectl apply sandboxset.yaml  kubectl apply              kubectl delete sandboxclaim
  或                           sandboxclaim.yaml          kubectl delete sandboxset
python app_deploy_with_warm_pool.py  (秒级交付)
  ↓
SandboxSet 维护 N 个空闲 Sandbox
```

## 9. 关键依赖

- Sandbox/SandboxSet CRD 需要集群中已安装 [OpenKruise Agents Operator](https://github.com/openkruise/agents)
- 镜像需推送到集群可访问的 Registry
- 预热池模式需提前规划副本数量

