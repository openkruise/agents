# Deploying LangChain Agent Service with Sandbox

This example demonstrates how to develop a LangChain Agent application using the [Aegra](https://github.com/aegra-ai/aegra) framework and deploy it to a Kubernetes cluster via the OpenKruise Agents Sandbox CRD.

## 0. Background

### Sandbox

`Sandbox` is the core CRD of OpenKruise Agents. It manages the lifecycle of a sandbox instance (such as a Pod) and provides advanced features including Pause, Resume, Checkpoint, Fork, and in-place upgrades.

### SandboxSet

`SandboxSet` is the workload that manages `Sandbox`. Its function is similar to a `ReplicaSet` that manages Pods. It enables sub-second sandbox startup by pre-warming a pool of sandbox instances. Optimized specifically for scaling performance, `SandboxSet` can rapidly replenish sandboxes as they are consumed.

### Aegra

LangGraph's official LangSmith Deployments requires an Enterprise license for self-hosted scenarios, and the free version only supports local development. Aegra is an Apache 2.0 open-source Agent service framework.

Aegra serves two purposes:

1. **Development Scaffold**: Generate project skeleton (state definitions, tools, context configuration, etc.) via `aegra init`, and start PostgreSQL + development server with `aegra dev`
2. **Online Server**: Serve as HTTP service entry point in production, load the actual Agent Graph via `aegra.json`, and expose API ports

The actual Agent logic (model invocation, tool orchestration, state transitions) is fully implemented using LangChain / LangGraph.

## 1. Local Development

### 1.1 Environment Setup

```bash
# Install aegra-cli
pip install aegra-cli
```

### 1.2 Local Development of Agent Server

```bash
# Create project root directory
mkdir langchain-test

# Initialize project agent using aegra
aegra init
# Expected output
📂 Where should I create the project? [.]: 

🌟 Choose a template:
  1. New Aegra Project — A simple chatbot with message memory.
  2. ReAct Agent — An agent with tools that reasons and acts step by step.

Enter your choice [1]: 2
You selected: ReAct Agent — An agent with tools that reasons and acts step by step.
...
```

After execution, the local project structure looks like:

```bash
➜  langchain-test tree
# Expected output
.
├── Dockerfile                 # Multi-stage build deployment image
├── README.md
├── aegra.json                 # Aegra entry configuration, specifies Graph loading path
├── docker-compose.yml         # Local development PostgreSQL + API orchestration
├── pyproject.toml             # Python project metadata and dependency declaration
└── src/langchain_test/        # Agent core code
    ├── __init__.py
    ├── context.py             # Runtime context (model, Prompt configurable)
    ├── graph.py               # LangGraph StateGraph definition and compilation
    ├── prompts.py             # System Prompt templates
    ├── state.py               # Input/internal state dataclass
    ├── tools.py               # LangChain @tool definitions
    └── utils.py               # Model loading utilities (supports OpenAI/Anthropic/DashScope)
```

In `src/langchain_test/utils.py`, you can customize model provider information. Example for DashScope:

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

Create a `.env` configuration file, or use `export k=v` directly for environment configuration:

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

Start the Agent service locally for testing:

```bash
uv sync
# Start Agent Server and persistence storage Server locally
uv run aegra dev
```

Use a client to access the local Server:

```python
import asyncio
from langgraph_sdk import get_client


async def main():
    client = get_client(url="http://localhost:8000")
    thread = await client.threads.create()
    async for chunk in client.runs.stream(
        thread_id=thread["thread_id"],
        assistant_id="langchain_test",
        input={"messages": [{"type": "human", "content": "What is 8888 + 8888?"}]},
    ):
        print(chunk)

asyncio.run(main())
```

## 2. Building Deployment Image

After local Agent code modifications are complete, package the Agent service into an image for deployment:

```bash
docker build --platform linux/amd64 -t your-registry/agent_app:langchain-v1 .
```

Push the image after building:

```bash
docker push your-registry/agent_app:langchain-v1
```

## 3. Kubernetes Deployment

Deployment includes two components: PostgreSQL persistent storage and Agent workload.

### 3.1 Deploy PostgreSQL

PostgreSQL uses the `pgvector/pgvector:pg18` image.

```bash
kubectl apply -f postgres.yaml
```

**postgres.yaml Resource Manifest:**

| Resource | Description |
|:--|:--|
| `Secret/postgres-secret` | Stores DB username, password, database name |
| `Deployment/postgres` | Single replica PostgreSQL with readiness/liveness probes |
| `Service/postgres` | ClusterIP service, accessible within cluster via `postgres:5432` |

> **Storage Strategy**: Currently uses `emptyDir` (test environment, data lost on Pod restart). PVC is recommended for production.

Verify PostgreSQL is ready:

```bash
kubectl get pods -l app=postgres
```

### 3.2 Deploy Agent Workload

Agent is deployed using OpenKruise Agents' `Sandbox` CRD, which provides sandbox management capabilities like Pause/Resume/Checkpoint compared to native Deployment.

```bash
kubectl apply -f sdx-langchain-dev.yaml
```

**sdx-langchain-dev.yaml Resource Manifest:**

| Resource | Description |
|:--|:--|
| `Secret/langchain-dev-secret` | LLM API Key, DATABASE_URL |
| `ConfigMap/langchain-dev-config` | Model name, service address and port |
| `Sandbox/langchain-dev` | Agent workload, mounts above Secret/ConfigMap |
| `Service/langchain-dev` | ClusterIP service, exposes port 8000 |

**Sandbox spec Core Configuration:**

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

- `command` directly invokes `aegra serve` to start HTTP service
- Configuration and secrets are injected via `envFrom`, Agent runtime automatically reads `MODEL`, `DATABASE_URL` and other variables
- PostgreSQL connection info is referenced from `postgres-secret`, consistent with the Secret in postgres.yaml

### 3.3 Verify Deployment

```bash
# Check Pod status
kubectl get sandbox langchain-dev
kubectl get pods -l app=langchain-dev

# View logs
kubectl logs -l app=langchain-dev --tail=50

# Forward cluster service to local
kubectl port-forward svc/langchain-dev 8000:8000

# Local access test
curl -s http://localhost:8000/health
```

Use Python client to access cluster Agent Server:

```python
import asyncio
from langgraph_sdk import get_client


async def main():
    client = get_client(url="http://localhost:8000")
    thread = await client.threads.create()
    async for chunk in client.runs.stream(
        thread_id=thread["thread_id"],
        assistant_id="langchain_test",
        input={"messages": [{"type": "human", "content": "What is 8888 + 8888?"}]},
    ):
        print(chunk)

asyncio.run(main())
```

### 3.4 External Access

Current Service type is `ClusterIP`, only accessible within the cluster. For external access, choose:

```yaml
# Option 1: NodePort
spec:
  type: NodePort
  ports:
    - port: 8000
      targetPort: 8000
      nodePort: 30800

# Option 2: With Ingress
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

## 4. Complete Deployment Flow Summary

```
Local Development              Build Image                 K8s Deployment
─────────────────              ───────────                 ──────────────
aegra init                     docker build                kubectl apply -f postgres.yaml
Configure .env / export vars   docker push                 kubectl apply -f sdx-langchain-dev.yaml
uv sync                                                    kubectl get sandbox langchain-test
uv run aegra dev
  ↓
localhost:8000 debug & verify
```

**Key Dependencies:**
- Agent workload depends on PostgreSQL being ready first (checkpoint storage)
- Sandbox CRD requires OpenKruise Agents Operator installed in the cluster
- `aegra.json` in the image determines which Graph to load
