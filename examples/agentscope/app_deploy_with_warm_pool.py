# -*- coding: utf-8 -*-
"""
Deploy AgentApp using SandboxSet (warm pool) + SandboxClaim mode.

This example demonstrates how to:
1. Create a warm pool (SandboxSet) with pre-warmed sandboxes
2. Claim a sandbox from the pool for a user (SandboxClaim)
3. Release the claimed sandbox when done
4. Delete the warm pool
"""
import asyncio
import os


from agentscope.agent import ReActAgent
from agentscope.formatter import DashScopeChatFormatter
from agentscope.formatter import OpenAIChatFormatter

from agentscope.model import DashScopeChatModel
from agentscope.model import OpenAIChatModel
from agentscope.pipeline import stream_printing_messages
from agentscope.tool import Toolkit, execute_python_code, execute_shell_command
from agentscope.memory import InMemoryMemory
from agentscope.session import RedisSession

from agentscope_runtime.engine.app import AgentApp
from agentscope_runtime.engine.schemas.agent_schemas import AgentRequest

from agentscope_runtime.engine.deployers.utils.docker_image_utils import (
    RegistryConfig,
)


agent_app = AgentApp(
    app_name="Friday",
    app_description="A helpful assistant",
)


@agent_app.init
async def init_func(self):
    import fakeredis

    fake_redis = fakeredis.aioredis.FakeRedis(decode_responses=True)
    # NOTE: This FakeRedis instance is for development/testing only.
    # In production, replace it with your own Redis client/connection
    # (e.g., aioredis.Redis)
    self.session = RedisSession(connection_pool=fake_redis.connection_pool)


@agent_app.query(framework="agentscope")
async def query_func(
    self,
    msgs,
    request: AgentRequest = None,
    **kwargs,
):
    assert kwargs is not None, "kwargs is Required for query_func"
    session_id = request.session_id
    user_id = request.user_id

    toolkit = Toolkit()
    toolkit.register_tool_function(execute_python_code)

    agent = ReActAgent(
        name="Friday",
        model=DashScopeChatModel(
            "qwen-turbo",
            api_key=os.getenv("DASHSCOPE_API_KEY"),
            enable_thinking=True,
            stream=True,
        ),
        sys_prompt="You're a helpful assistant named Friday.",
        toolkit=toolkit,
        memory=InMemoryMemory(),
        formatter=DashScopeChatFormatter(),
    )

    await self.session.load_session_state(
        session_id=session_id,
        user_id=user_id,
        agent=agent,
    )

    async for msg, last in stream_printing_messages(
        agents=[agent],
        coroutine_task=agent(msgs),
    ):
        yield msg, last

    await self.session.save_session_state(
        session_id=session_id,
        user_id=user_id,
        agent=agent,
    )


@agent_app.endpoint("/sync")
def sync_handler(request: AgentRequest):
    yield {"status": "ok", "payload": request}


@agent_app.endpoint("/async")
async def async_handler(request: AgentRequest):
    yield {"status": "ok", "payload": request}


@agent_app.endpoint("/stream_async")
async def stream_async_handler(request: AgentRequest):
    for i in range(5):
        yield f"async chunk {i}, with request payload {request}\n"


@agent_app.endpoint("/stream_sync")
def stream_sync_handler(request: AgentRequest):
    for i in range(5):
        yield f"sync chunk {i}, with request payload {request}\n"


@agent_app.task("/task", queue="celery1")
def task_handler(request: AgentRequest):
    time.sleep(30)
    yield {"status": "ok", "payload": request}


@agent_app.task("/atask")
async def atask_handler(request: AgentRequest):
    await asyncio.sleep(15)
    yield {"status": "ok", "payload": request}



# ==================== Configuration ====================

REGISTRY_CONFIG = RegistryConfig(
    registry_url=os.environ.get("REGISTRY_URL", "your-registry-url"),
    namespace="agentscope-runtime",
)



POOL_CONFIG = {
    "pool_name": "friday-pool",
    "pool_replicas": 3,  # Maintain 3 pre-warmed sandboxes
    "port": 8080,
    "image_name": "friday-agent",
    "image_tag": "v1",
    "base_image": "kube-ai-registry.cn-shanghai.cr.aliyuncs.com/kube-ai/python:3.12.12",
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
        "image_pull_policy": "IfNotPresent",
    },
    "push_to_registry": True,
    "platform": "linux/amd64",
}


# ==================== Warm Pool Deployment ====================


async def create_warm_pool(deployer):
    """Step 1: Create a warm pool (SandboxSet) with pre-warmed sandboxes"""

    print("🚀 Creating warm pool (SandboxSet)...")

    # 构建 app 部署参数（参考 sandbox_claim_deploy.py 已跑通的方式）
    app_deploy_kwargs = {
        "app": agent_app,
        "custom_endpoints": agent_app.custom_endpoints,
        "runner": agent_app._runner,
        "endpoint_path": agent_app.endpoint_path,
        "stream": agent_app.stream,
        "protocol_adapters": agent_app.protocol_adapters,
    }
    app_deploy_kwargs.update(**POOL_CONFIG)

    result = await deployer.deploy_warm_pool(**app_deploy_kwargs)

    print("✅ Warm pool created!")
    print(f"   Pool name: {result['pool_name']}")
    print(f"   Replicas: {result['replicas']}")
    print(f"   Status: {result['status']}")

    return result


async def claim_sandbox_for_user(
    deployer,
    pool_name: str,
    user_id: str,
):

    """Step 2: Claim a sandbox from the pool for a specific user"""

    print(f"\n🎯 Claiming sandbox for user '{user_id}' from pool '{pool_name}'...")

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


async def release_user_sandbox(deployer, deploy_id: str):
    """Step 3: Release the claimed sandbox when user is done"""

    print(f"\n🧹 Releasing sandbox for deploy_id '{deploy_id}'...")

    result = await deployer.release_claim(deploy_id)

    if result["success"]:
        print(f"✅ Sandbox released: {result['message']}")
    else:
        print(f"❌ Release failed: {result['message']}")

    return result


async def delete_warm_pool(deployer, pool_name: str):
    """Step 4: Delete the warm pool when no longer needed"""

    print(f"\n🗑️ Deleting warm pool '{pool_name}'...")

    result = await deployer.delete_warm_pool(pool_name)

    if result["success"]:
        print(f"✅ Warm pool deleted: {result['message']}")
    else:
        print(f"❌ Delete failed: {result['message']}")

    return result


async def test_service(service_url: str):
    """Test the deployed service"""
    import aiohttp

    try:
        async with aiohttp.ClientSession() as session:
            # Health check
            async with session.get(
                f"{service_url}/health",
            ) as response:
                if response.status == 200:
                    print("✅ Health check passed")
                else:
                    print(f"❌ Health check failed: {response.status}")

    except Exception as e:
        print(f"❌ Service test exception: {e}")


async def main():
    """
    Main function demonstrating the full warm pool workflow:
    1. Create warm pool (admin, one-time setup)
    2. Claim sandbox (per user request)
    3. Use the sandbox
    4. Release sandbox (when user is done)
    5. Delete warm pool (cleanup)
    """


    from agentscope_runtime.engine.deployers.kruise_deployer import (
    KruiseDeployManager,
    K8sConfig,
    )
    from agentscope_runtime.engine.deployers.utils.docker_image_utils import (
        RegistryConfig,
    )

    K8S_CONFIG = K8sConfig(
        k8s_namespace="agentscope-runtime",
        kubeconfig_path=None,
    )
    # Create deployer
    deployer = KruiseDeployManager(
        kube_config=K8S_CONFIG,
        registry_config=REGISTRY_CONFIG,
    )

    pool_name = POOL_CONFIG["pool_name"]

    try:
        # Step 1: Create warm pool (typically done once by admin)
        pool_result = await create_warm_pool(deployer)

        print("\n" + "=" * 60)
        print("📦 Warm pool is ready! Now simulating user requests...")
        print("=" * 60)

        # Step 2: Simulate user claiming a sandbox
        user_id = "alice"
        claim_result = await claim_sandbox_for_user(
            deployer,
            pool_name=pool_name,
            user_id=user_id,
        )

        # Step 3: Test the claimed sandbox
        print(f"\n🧪 Testing service at {claim_result['url']}...")
        await test_service(claim_result["url"])

        # Show kubectl commands for verification
        print(
            f"""

📝 Verify with kubectl:
    # Check SandboxSet (warm pool)
    kubectl get sandboxsets -n agentscope-runtime

    # Check SandboxClaim
    kubectl get sandboxclaims -n agentscope-runtime

    # Check Sandboxes (should see claimed ones with label)
    kubectl get sandbox -n agentscope-runtime
    kubectl get sandbox -l agents.kruise.io/claim-name=user-{user_id} -n agentscope-runtime

    # Check Services
    kubectl get svc -n agentscope-runtime

    # Test the service
    curl {claim_result['url']}/health

    # Test sync endpoint
    curl -X POST {claim_result['url']}/sync \\
      -H "Content-Type: application/json" \\
      -d '{{"input": [{{"role": "user", "content": [{{"type": "text", "text": "Hello!"}}]}}], "session_id": "123"}}'
        """
        )

        # Wait for user confirmation
        input("\nPress Enter to release the claimed sandbox...")

        # Step 4: Release the claimed sandbox
        await release_user_sandbox(deployer, claim_result["deploy_id"])

        # Wait for user confirmation
        input("\nPress Enter to delete the warm pool...")

        # Step 5: Delete the warm pool
        await delete_warm_pool(deployer, pool_name)

        print("\n✅ Warm pool workflow completed!")

    except Exception as e:
        print(f"❌ Error: {e}")
        import traceback

        traceback.print_exc()


if __name__ == "__main__":
    asyncio.run(main())
