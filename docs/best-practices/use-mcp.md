# Using OpenKruise Agents Sandbox via MCP Protocol

The sandbox-manager component of OpenKruise Agents supports MCP (Model Context Protocol) as an alternative interface to E2B protocol for AI agent integrations.

## Overview

MCP Server provides a standardized protocol interface for AI agents to execute code and commands in sandbox environments. It runs alongside the E2B API server and shares the same authentication and sandbox management infrastructure.

```text
┌─────────────┐    ┌─────────────┐
│   E2B API   │    │   MCP API   │   <-- External Protocol Interfaces
└──────┬──────┘    └──────┬──────┘
       │                  │
       └────────┬─────────┘
                ▼
        ┌───────────────┐
        │ SandboxManager│              <-- Unified Management Layer
        └───────┬───────┘
                ▼
        ┌───────────────┐
        │  Sandbox CRs  │              <-- Kubernetes Resources
        └───────────────┘
```

### Available Tools

| Tool Name      | Description                                                                |
|----------------|----------------------------------------------------------------------------|
| `run_code`     | Execute code in a persistent sandbox with Jupyter Notebook semantics       |
| `run_code_once`| Execute code in a one-time sandbox (auto-cleanup after execution)          |
| `run_command`  | Execute shell commands in the sandbox environment                          |

## Enabling MCP Server

MCP Server is disabled by default. To enable it, set the following environment variables in sandbox-manager:

| Environment Variable   | Description                              | Default Value |
|------------------------|------------------------------------------|---------------|
| `MCP_SERVER_ENABLED`   | Enable MCP Server                        | `false`       |
| `MCP_SERVER_PORT`      | Port for MCP HTTP endpoint               | `8082`        |
| `MCP_SANDBOX_TTL`      | Sandbox TTL in seconds                   | `300`         |
| `MCP_SESSION_SYNC_PORT`| Port for session peer synchronization    | `7790`        |

### Configuration Example

1. Add environment variables to sandbox-manager Deployment:

```yaml
env:
  - name: MCP_SERVER_ENABLED
    value: "true"
  - name: MCP_SERVER_PORT
    value: "8082"
  - name: MCP_SANDBOX_TTL
    value: "300"
```

2. Add container port to sandbox-manager Deployment:

```yaml
ports:
  - containerPort: 8082
    name: http-mcp
```

3. Add service port to sandbox-manager Service (required for Service-based access):

```yaml
ports:
  - port: 8082
    targetPort: 8082
    protocol: TCP
    name: http-mcp
```

## Authentication

MCP Server uses HTTP header authentication, sharing the same API key system with E2B:

- **Header**: `X-API-KEY`
- **Value**: Same API key used for E2B API (`E2B_API_KEY`)

If `E2B_ENABLE_AUTH` is set to `false`, authentication is disabled and anonymous access is allowed.

## Endpoint

The default MCP endpoint path is `/mcp`. Full endpoint URL:

```
http://<sandbox-manager-host>:<MCP_SERVER_PORT>/mcp
```

## Client Integration Methods

### 1. Direct HTTP Access (In-Cluster)

For clients running in the same Kubernetes cluster:

```python
import httpx
from mcp import ClientSession
from mcp.client.streamable_http import streamablehttp_client

async def run_code_in_sandbox():
    # Configure MCP client
    mcp_url = "http://sandbox-manager.sandbox-system.svc.cluster.local:8082/mcp"
    headers = {"X-API-KEY": "<your-api-key>"}
    
    async with streamablehttp_client(mcp_url, headers=headers) as (read, write, _):
        async with ClientSession(read, write) as session:
            await session.initialize()
            
            # Call run_code tool
            result = await session.call_tool("run_code", {
                "code": "print('Hello from sandbox!')",
                "language": "python"
            })
            print(result)
```

### 2. Port Forward to Local Machine

1. Port forward MCP server:
   ```shell
   kubectl port-forward services/sandbox-manager 8082:8082 -n sandbox-system
   ```

2. Connect from local client:
   ```python
   mcp_url = "http://localhost:8082/mcp"
   headers = {"X-API-KEY": "<your-api-key>"}
   ```

### 3. External Access via Ingress

Configure Ingress to expose MCP endpoint externally. Ensure proper TLS and authentication.

## Tool Usage Examples

### run_code

Execute code with persistent session state:

```json
{
  "code": "x = 10\nprint(f'x = {x}')",
  "language": "python"
}
```

Response:
```json
{
  "logs": {
    "stdout": [
      "x = 10\n"
    ],
    "stderr": []
  },
  "results": [],
  "sandbox_id": "default--sandbox-abc123",
  "execution_count": 3
}
```

### run_code_once

Execute code in a disposable sandbox:

```json
{
  "code": "import os\nprint(os.getcwd())",
  "language": "python"
}
```

### run_command

Execute shell commands:

```json
{
  "cmd": "ls -la /workspace"
}

```

Response:
```json
{
  "stdout": "...",
  "stderr": "",
  "exit_code": 0,
  "sandbox_id": "default--sandbox-abc123"
}
```

## Session Management

MCP Server automatically manages sandbox lifecycle:

- **Session Binding**: Each MCP session is bound to a dedicated sandbox
- **Auto-Provisioning**: Sandbox is claimed from SandboxSet pool on first tool call
- **TTL Management**: Sandbox is automatically released after `MCP_SANDBOX_TTL` idle time
- **Peer Sync**: Sessions are synchronized across sandbox-manager replicas

## Comparison with E2B API

| Feature              | E2B API                        | MCP Protocol                   |
|----------------------|--------------------------------|--------------------------------|
| Protocol             | REST HTTP                      | MCP over HTTP (Streamable)     |
| Session State        | Manual sandbox management      | Automatic session-sandbox binding |
| Authentication       | `X-API-KEY` header             | `X-API-KEY` header             |
| Use Case             | Direct sandbox control         | AI agent tool integration      |
| Sandbox Lifecycle    | Explicit create/delete         | Auto-provisioned per session   |

## Health Check

MCP Server provides a health check endpoint:

```shell
curl http://<sandbox-manager-host>:8082/health
# Response: OK
```
