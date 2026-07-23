# Using OpenKruise Agents with LangChain and LangGraph

OpenKruise Agents exposes an E2B-compatible API, so any LangChain or LangGraph agent that already works with E2B works against an OpenKruise Agents cluster with no code changes — only the environment variables differ. This guide walks through the setup and shows two minimal examples.

## Prerequisites

- An OpenKruise Agents cluster with `sandbox-manager` reachable. See [use-e2b.md](./use-e2b.md) for installation and `E2B_DOMAIN` configuration.
- A pre-warmed `SandboxSet` to act as your template. The [code_interpreter example](../../examples/code_interpreter/sandboxset.yaml) is a good starting point — it provisions a template named `code-interpreter`.
- Python 3.10+.

Install the SDKs:

```shell
pip install langchain langgraph langchain-openai e2b-code-interpreter
```

## Configuring the E2B SDK

OpenKruise Agents supports both the native E2B protocol and a private (path-based) protocol. The private protocol avoids wildcard DNS / wildcard certificates and is the recommended option for self-hosted clusters. See [use-e2b.md](./use-e2b.md) for the trade-offs.

```shell
export E2B_DOMAIN=your.domain.com         # your sandbox-manager ingress host
export E2B_API_KEY=your-api-key
export OPENAI_API_KEY=your-openai-key     # or whichever LLM provider you use
```

If you are using the private protocol over plain HTTP (typical for local kind / dev clusters), patch the E2B SDK with the bundled shim before importing `Sandbox`:

```python
from kruise_agents.patch_e2b import patch_e2b
patch_e2b(https=False)  # set to True for HTTPS

from e2b_code_interpreter import Sandbox
```

See [sdk/customized_e2b/README.md](../../sdk/customized_e2b/README.md) for details on the shim.

## Example 1 — LangChain tool that runs Python in a sandbox

A common pattern is to expose sandbox code execution as a LangChain `@tool`. Each invocation claims a sandbox from the pool, runs the code, and shuts the sandbox down.

```python
import os

# Optional: only needed for the private protocol over HTTP.
# from kruise_agents.patch_e2b import patch_e2b
# patch_e2b(https=False)

from e2b_code_interpreter import Sandbox
from langchain_core.tools import tool
from langchain_openai import ChatOpenAI
from langgraph.prebuilt import create_react_agent


@tool
def python_run(code: str) -> str:
    """Execute Python code in an isolated sandbox and return stdout/stderr."""
    with Sandbox.create(template="code-interpreter", timeout=300) as sbx:
        result = sbx.run_code(code)
        out = result.logs.stdout or []
        err = result.logs.stderr or []
        return "\n".join(out + err)


llm = ChatOpenAI(model="gpt-4o-mini")
agent = create_react_agent(llm, [python_run])

response = agent.invoke({
    "messages": [("user", "Compute the 50th Fibonacci number using Python.")]
})
print(response["messages"][-1].content)
```

The `template` argument must match the name of a `SandboxSet` already deployed in your cluster.

## Example 2 — LangGraph workflow with a persistent sandbox

When later steps in a workflow need to share state with earlier ones (variables, files, installed packages), keep one sandbox alive across the whole graph run instead of creating a new one per tool call.

```python
import os
from typing import Annotated, TypedDict

from e2b_code_interpreter import Sandbox
from langchain_core.tools import tool
from langchain_openai import ChatOpenAI
from langgraph.graph import StateGraph, START
from langgraph.graph.message import add_messages
from langgraph.prebuilt import ToolNode, tools_condition


# Long-lived sandbox shared across the graph run.
sandbox = Sandbox.create(template="code-interpreter", timeout=600)


@tool
def python_run(code: str) -> str:
    """Execute Python in a sandbox that persists state across calls."""
    result = sandbox.run_code(code)
    out = result.logs.stdout or []
    err = result.logs.stderr or []
    return "\n".join(out + err)


@tool
def write_file(path: str, contents: str) -> str:
    """Write a file inside the sandbox."""
    sandbox.files.write(path, contents)
    return f"wrote {len(contents)} bytes to {path}"


class State(TypedDict):
    messages: Annotated[list, add_messages]


tools = [python_run, write_file]
llm = ChatOpenAI(model="gpt-4o-mini").bind_tools(tools)


def llm_node(state: State):
    return {"messages": [llm.invoke(state["messages"])]}


graph = (
    StateGraph(State)
    .add_node("llm", llm_node)
    .add_node("tools", ToolNode(tools))
    .add_edge(START, "llm")
    .add_conditional_edges("llm", tools_condition)
    .add_edge("tools", "llm")
    .compile()
)

try:
    final = graph.invoke({
        "messages": [(
            "user",
            "Save a CSV of the squares 1..10 to /tmp/sq.csv, then load it and print the sum.",
        )],
    })
    print(final["messages"][-1].content)
finally:
    sandbox.kill()
```

`sandbox.beta_pause()` hibernates the sandbox between graph runs and `Sandbox.connect(sandbox_id=...)` reattaches later — useful for long-running multi-turn agent sessions where you want to preserve installed packages and warm state without paying for idle compute.

## Operational notes

- Each `Sandbox.create(template=...)` call removes one sandbox from the pre-warmed pool until the sandbox is killed or paused. Size your `SandboxSet` replicas to your expected concurrent agent runs to avoid waits.
- The `metadata={...}` argument to `Sandbox.create` accepts the OpenKruise-specific extension keys, including `e2b.agents.kruise.io/csi-volume-name`, `e2b.agents.kruise.io/csi-mount-point`, and `e2b.agents.kruise.io/csi-subpath`. These let you mount a shared persistent volume for cross-sandbox skill or workspace sharing — see the [Dynamic Persistent Volume Mounting user manual](https://openkruise.io/kruiseagents/user-manuals/sandbox-claim#dynamic-persistent-volume-mounting).
- Set `timeout` per `Sandbox.create()` to a value that matches your expected agent step duration. The maximum is configured server-side via `--e2b-max-timeout` on `sandbox-manager`.

## Related

- [use-e2b.md](./use-e2b.md) — installing and configuring the E2B-compatible API
- [examples/code_interpreter](../../examples/code_interpreter) — a runnable code-interpreter example
- [sdk/customized_e2b](../../sdk/customized_e2b) — `patch_e2b` shim for the private protocol
- [LangChain documentation](https://python.langchain.com/) and [LangGraph documentation](https://langchain-ai.github.io/langgraph/)
