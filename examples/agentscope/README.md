# Deploying AgentScope Applications with Sandbox

This example demonstrates how to develop Agent applications using the [AgentScope](https://github.com/modelscope/agentscope) framework and deploy them to a Kubernetes cluster via [AgentScope Runtime](https://github.com/modelscope/agentscope-runtime) and the OpenKruise Agents Sandbox CRD.

## 0. Background

### Sandbox

`Sandbox` is the core CRD of OpenKruise Agents. It manages the lifecycle of a sandbox instance (such as a Pod) and provides advanced features including Pause, Resume, Checkpoint, Fork, and in-place upgrades.

### SandboxSet

`SandboxSet` is the workload that manages `Sandbox`. Its function is similar to a `ReplicaSet` that manages Pods. It enables sub-second sandbox startup by pre-warming a pool of sandbox instances. Optimized specifically for scaling performance, `SandboxSet` can rapidly replenish sandboxes as they are consumed.

### SandboxClaim

`SandboxClaim` is used to claim (acquire) existing sandbox resources from a `SandboxSet` warm pool. Compared to creating a new Sandbox from scratch, claiming a pre-warmed sandbox can achieve second-level or even sub-second delivery.

## 1. Environment Setup

### 1.1 Install Dependencies

```bash
# Install agentscope-runtime (with extended dependencies)
pip install "agentscope-runtime[ext]>=1.0.0"
```

### 1.2 Configure Environment Variables

```bash
# LLM API Key
export DASHSCOPE_API_KEY="your-api-key"

# Image registry URL (optional)
export REGISTRY_URL="your-registry-url"
```

### 1.3 Verify Kubernetes Access

```bash
# Verify cluster connection
kubectl cluster-info

# Check Kruise Sandbox CRDs are installed
kubectl get crd sandboxes.agents.kruise.io
kubectl get crd sandboxsets.agents.kruise.io
```

## 2. Define Agent Application

Agent applications are defined based on `agentscope_runtime.engine.app.AgentApp`, with core components including:

```python
from agentscope.agent import ReActAgent
from agentscope.model import DashScopeChatModel
from agentscope.tool import Toolkit, execute_python_code
from agentscope_runtime.engine.app import AgentApp
from agentscope_runtime.engine.schemas.agent_schemas import AgentRequest

# Create AgentApp instance
agent_app = AgentApp(
    app_name="Friday",
    app_description="A helpful assistant",
)


# Initialization function: Configure Session storage
@agent_app.init
async def init_func(self):
    import fakeredis
    fake_redis = fakeredis.aioredis.FakeRedis(decode_responses=True)
    self.session = RedisSession(connection_pool=fake_redis.connection_pool)


# Core query function: Handle user messages
@agent_app.query(framework="agentscope")
async def query_func(self, msgs, request: AgentRequest = None, **kwargs):
    session_id = request.session_id
    user_id = request.user_id

    # Configure toolkit
    toolkit = Toolkit()
    toolkit.register_tool_function(execute_python_code)

    # Create ReAct Agent
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

    # Load session state
    await self.session.load_session_state(session_id, user_id, agent)

    # Streaming output
    async for msg, last in stream_printing_messages(
        agents=[agent], coroutine_task=agent(msgs)
    ):
        yield msg, last

    # Save session state
    await self.session.save_session_state(session_id, user_id, agent)


# Custom endpoints
@agent_app.endpoint("/sync")
def sync_handler(request: AgentRequest):
    yield {"status": "ok", "payload": request}


@agent_app.endpoint("/stream_async")
async def stream_async_handler(request: AgentRequest):
    for i in range(5):
        yield f"async chunk {i}\n"
```

**Core Decorator Reference:**

| Decorator | Purpose |
|:--|:--|
| `@agent_app.init` | Initialization function, configure Session/DB connections, etc. |
| `@agent_app.query` | Core query processing, supports streaming output |
| `@agent_app.endpoint` | Custom HTTP endpoints |
| `@agent_app.task` | Async tasks (supports Celery queues) |

See [app_deploy_to_kruise.py](app_deploy_to_kruise.py) for complete code.

## 3. Option 1: Direct Deployment (Sandbox)

Suitable for single-instance scenarios, creating an independent Sandbox resource.

See complete script: [app_deploy_to_kruise.py](app_deploy_to_kruise.py)

```python
# app_deploy_to_kruise.py
import asyncio
import os

from agentscope_runtime.engine.deployers.kruise_deployer import (
    KruiseDeployManager,
    K8sConfig,
)
from agentscope_runtime.engine.deployers.utils.docker_image_utils import (
    RegistryConfig,
)


async def deploy_to_kruise():
    """Deploy AgentApp to Kruise Sandbox"""

    # 1. Configure image registry
    registry_config = RegistryConfig(
        registry_url=os.environ.get("REGISTRY_URL", "your-registry-url"),
        namespace="agentscope-runtime",
    )

    # 2. Configure K8s connection
    k8s_config = K8sConfig(
        k8s_namespace="agentscope-runtime",
        kubeconfig_path=None,
    )

    # 3. Create KruiseDeployManager
    deployer = KruiseDeployManager(
        kube_config=k8s_config,
        registry_config=registry_config,
    )

    # 4. Runtime configuration
    runtime_config = {
        "resources": {
            "requests": {"cpu": "200m", "memory": "512Mi"},
            "limits": {"cpu": "1000m", "memory": "2Gi"},
        },
        "image_pull_policy": "IfNotPresent",
    }

    # 5. Deployment configuration
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

    # 6. Execute deployment
    print("🚀 Starting deployment...")
    result = await app.deploy(deployer, **kruise_config)

    print("✅ Deployment successful!")
    print(f"   Deploy ID: {result['deploy_id']}")
    print(f"   Service URL: {result['url']}")
    print(f"   Resource name: {result['resource_name']}")

    return result, deployer


if __name__ == "__main__":
    asyncio.run(deploy_to_kruise())
```

## 4. Option 2: Warm Pool Deployment (SandboxSet + SandboxClaim)

Suitable for scenarios requiring fast multi-instance startup, the warm pool enables second-level Agent delivery.

See complete script: [app_deploy_with_warm_pool.py](app_deploy_with_warm_pool.py)

### 4.1 Create Warm Pool

```python
# app_deploy_with_warm_pool.py
import asyncio
import os

from agentscope_runtime.engine.deployers.kruise_deployer import (
    KruiseDeployManager,
    K8sConfig,
)
from agentscope_runtime.engine.deployers.utils.docker_image_utils import (
    RegistryConfig,
)

# Configuration
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
    "pool_replicas": 3,  # Maintain 3 pre-warmed instances
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
    """Create Agent warm pool (SandboxSet)"""

    print("🚀 Creating warm pool (SandboxSet)...")

    # Build app deployment parameters
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

    print("✅ Warm pool created!")
    print(f"   Pool name: {result['pool_name']}")
    print(f"   Replicas: {result['replicas']}")
    print(f"   Status: {result['status']}")

    return result
```

### 4.2 Claim Sandbox from Warm Pool

```python
async def claim_sandbox_for_user(
    deployer: KruiseDeployManager,
    pool_name: str,
    user_id: str,
):
    """Claim a Sandbox from warm pool (second-level delivery)"""

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

    print("✅ Sandbox claimed!")
    print(f"   Deploy ID: {result['deploy_id']}")
    print(f"   Claim name: {result['claim_name']}")
    print(f"   Sandbox name: {result['sandbox_name']}")
    print(f"   Service URL: {result['url']}")

    return result
```

### 4.3 Release Sandbox

```python
async def release_user_sandbox(deployer: KruiseDeployManager, deploy_id: str):
    """Release claimed Sandbox"""

    print("🧹 Releasing sandbox...")
    result = await deployer.release_claim(deploy_id)

    if result["success"]:
        print(f"✅ Sandbox released: {result['message']}")
    else:
        print(f"❌ Release failed: {result['message']}")

    return result
```

### 4.4 Delete Warm Pool

```python
async def delete_warm_pool(deployer: KruiseDeployManager, pool_name: str):
    """Delete warm pool (SandboxSet)"""

    print(f"🗑️ Deleting warm pool '{pool_name}'...")
    result = await deployer.delete_warm_pool(pool_name)

    if result["success"]:
        print(f"✅ Warm pool deleted: {result['message']}")
    else:
        print(f"❌ Delete failed: {result['message']}")

    return result
```

## 5. Verify Deployment

```bash
# Check Sandbox status
kubectl get sandbox -n agentscope-runtime

# Check SandboxSet status (warm pool)
kubectl get sandboxsets -n agentscope-runtime

# Check SandboxClaim status
kubectl get sandboxclaims -n agentscope-runtime

# View Pod logs
kubectl logs -l app=agentscope-agent -n agentscope-runtime --tail=50

# Port forward for testing
kubectl port-forward svc/agentscope-agent 8080:8080 -n agentscope-runtime

# Health check
curl http://localhost:8080/health

# Test sync endpoint
curl -X POST http://localhost:8080/sync \
  -H "Content-Type: application/json" \
  -d '{"input": [{"role": "user", "content": [{"type": "text", "text": "Hello!"}]}], "session_id": "123"}'

# Test streaming endpoint
curl -X POST http://localhost:8080/stream_async \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  --no-buffer \
  -d '{"input": [{"role": "user", "content": [{"type": "text", "text": "Hello!"}]}], "session_id": "123"}'
```

## 6. Clean Up Resources

```bash
# Stop deployment using CLI
asrt stop <deploy_id>

# Or delete manually
kubectl delete sandbox agentscope-agent -n agentscope-runtime
kubectl delete svc agentscope-agent -n agentscope-runtime

# Delete warm pool
kubectl delete sandboxset agentscope-pool -n agentscope-runtime
```

## 7. Deployment Mode Comparison

| Feature | Direct Deployment (Sandbox) | Warm Pool Deployment (SandboxSet) |
|:--|:--|:--|
| Startup Speed | Minutes | Seconds |
| Resource Usage | On-demand creation | Pre-warmed occupation |
| Use Case | Single instance, long-running | Multi-tenant, on-demand scaling |
| Management Complexity | Simple | Requires pool size management |

## 8. Complete Deployment Flow Summary

### Direct Deployment Mode

```
Define Agent                   Build Image                 K8s Deployment
────────────                   ───────────                 ──────────────
Define AgentApp                docker build                kubectl apply -f sandbox.yaml
Configure @query/@endpoint     docker push                 kubectl get sandbox
python app_deploy_to_kruise.py                             kubectl port-forward
  ↓
localhost:8080 debug & verify
```

### Warm Pool Mode

```
Create Warm Pool               User Claims                 Release/Cleanup
────────────────               ───────────                 ───────────────
kubectl apply sandboxset.yaml  kubectl apply               kubectl delete sandboxclaim
  or                           sandboxclaim.yaml           kubectl delete sandboxset
python app_deploy_with_warm_pool.py  (second-level delivery)
  ↓
SandboxSet maintains N idle Sandboxes
```

## 9. Key Dependencies

- Sandbox/SandboxSet CRD requires [OpenKruise Agents Operator](https://github.com/openkruise/agents) installed in the cluster
- Images need to be pushed to a registry accessible by the cluster
- Warm pool mode requires pre-planning replica count
