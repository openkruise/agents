# Running LangChain and LangGraph Agents in OpenKruise Sandboxes

## Introduction

OpenKruise Agents provides secure, isolated execution environments (sandboxes) for running untrusted or experimental code in Kubernetes. Each sandbox runs in its own container with configurable resource limits, network isolation, and filesystem boundaries—making it ideal for running AI agents that execute arbitrary code or interact with external tools.

**Why run LangChain and LangGraph agents in sandboxes?**

AI agents often need to execute generated code, call external APIs, or manipulate files. Without isolation, these operations pose security risks:

- **Code execution safety**: Agents may generate and run Python code that could access sensitive data or consume excessive resources
- **Tool isolation**: Prevent agents from interfering with each other or the host system
- **Resource control**: Set CPU and memory limits to prevent runaway processes
- **Reproducibility**: Create clean, ephemeral environments for each agent execution

The **E2B API** provides a high-level interface for managing OpenKruise sandboxes. Instead of directly interacting with Kubernetes resources, you can create, execute code in, and destroy sandboxes using a simple Python SDK. This tutorial shows you how to combine E2B with LangChain and LangGraph to run AI agents safely in isolated environments.

## High-Level Architecture

The integration follows this flow:

```
┌─────────────────┐      ┌──────────────┐      ┌────────────────────┐      ┌──────────────┐
│  Your Python    │─────▶│  E2B SDK     │─────▶│  OpenKruise        │─────▶│  Isolated    │
│  Application    │      │              │      │  Agents            │      │  Sandbox     │
│                 │      │              │      │  (Kubernetes)      │      │  Container   │
│ LangChain /     │◀─────│  API Client  │◀─────│                    │◀─────│              │
│ LangGraph       │      │              │      │                    │      │  Agent Code  │
│ Agent           │      │              │      │                    │      │  + Tools     │
└─────────────────┘      └──────────────┘      └────────────────────┘      └──────────────┘
```

**Key components:**

1. **Your application**: Defines the LangChain or LangGraph agent logic
2. **E2B SDK**: Handles sandbox lifecycle (create, execute, destroy) via API calls
3. **OpenKruise Agents**: Manages sandbox containers in Kubernetes with isolation and resource limits
4. **Sandbox**: Ephemeral container where agent code and tools actually run

Your agent code runs in your local environment or server. When the agent needs to execute code (e.g., via a Python REPL tool), that execution happens inside the sandbox. This separation ensures that potentially unsafe operations remain isolated.

## Running a LangChain Agent in an OpenKruise Sandbox

Let's build a LangChain agent that can execute Python code in a sandbox. We'll use the E2B SDK to create an isolated environment where code execution happens safely.

### Prerequisites

Install the required packages:

```bash
pip install langchain langchain-openai e2b-code-interpreter
```

Set your API keys:

```bash
export OPENAI_API_KEY="your-openai-key"
export E2B_API_KEY="your-e2b-key"
```

### Creating a Code Execution Tool

First, we'll create a LangChain tool that executes Python code inside a sandbox:

```python
from langchain.tools import tool
from e2b_code_interpreter import Sandbox as CodeInterpreter

@tool
def execute_python(code: str) -> str:
    """
    Execute Python code in an isolated sandbox.
    Returns the output of the code execution.
    """
    with CodeInterpreter() as sandbox:
        execution = sandbox.notebook.exec_cell(code)
        
        if execution.error:
            return f"Error: {execution.error}"
        
        # Combine text output and result
        output = ""
        if execution.logs.stdout:
            output += execution.logs.stdout
        if execution.logs.stderr:
            output += f"\nStderr: {execution.logs.stderr}"
        if execution.results:
            output += f"\nResult: {execution.results[0].text}"
        
        return output or "Code executed successfully (no output)"
```

### Building the Agent

Now we'll create a LangChain agent that can use this tool:

```python
from langchain_openai import ChatOpenAI
from langchain.agents import create_tool_calling_agent, AgentExecutor
from langchain.prompts import ChatPromptTemplate

# Initialize the LLM
llm = ChatOpenAI(model="gpt-4", temperature=0)

# Define the agent prompt
prompt = ChatPromptTemplate.from_messages([
    ("system", "You are a helpful AI assistant with access to a Python sandbox. "
               "Use the execute_python tool to run code when needed."),
    ("human", "{input}"),
    ("placeholder", "{agent_scratchpad}"),
])

# Create the agent with our sandbox tool
tools = [execute_python]
agent = create_tool_calling_agent(llm, tools, prompt)
agent_executor = AgentExecutor(agent=agent, tools=tools, verbose=True)

# Run the agent
result = agent_executor.invoke({
    "input": "Calculate the sum of squares of numbers from 1 to 100"
})

print(result["output"])
```

**What's happening here:**

1. The agent receives your request
2. It decides to use the `execute_python` tool and generates appropriate code
3. The E2B SDK creates a fresh sandbox container via OpenKruise Agents
4. Code executes inside the isolated sandbox
5. Results return to the agent, which formulates a response
6. The sandbox is automatically destroyed when the `with` block exits

**Sandbox isolation in action:**

- The code runs in a separate container with its own filesystem
- Resource limits prevent CPU or memory exhaustion
- Network access can be controlled (depending on sandbox configuration)
- Each execution starts from a clean state

### Long-Lived Sandboxes

For agents that need to maintain state across multiple executions, you can keep the sandbox alive:

```python
from e2b_code_interpreter import Sandbox as CodeInterpreter

# Create a persistent sandbox
sandbox = CodeInterpreter()

@tool
def execute_python_persistent(code: str) -> str:
    """Execute Python code in a persistent sandbox."""
    execution = sandbox.notebook.exec_cell(code)
    
    if execution.error:
        return f"Error: {execution.error}"
    
    output = execution.logs.stdout or ""
    if execution.results:
        output += f"\n{execution.results[0].text}"
    
    return output or "Success"

# Use the tool in your agent...
# When done:
sandbox.close()
```

This approach allows the agent to install packages, define variables, or create files that persist across multiple tool calls within the same session.

## Running a LangGraph Workflow in a Sandbox

LangGraph extends LangChain with graph-based workflows, enabling more complex multi-step agent behaviors. Sandboxes are particularly valuable for LangGraph because workflows often involve multiple code executions and state transformations.

### What is LangGraph?

LangGraph lets you define agents as state machines where nodes represent actions (LLM calls, tool usage) and edges represent transitions. This is useful for:

- Multi-step reasoning tasks
- Agents that need to retry or backtrack
- Workflows with conditional branching

### Example: Data Analysis Workflow

Let's build a LangGraph workflow that analyzes data in multiple steps, all within a sandbox:

```python
from langchain_openai import ChatOpenAI
from langgraph.graph import StateGraph, END
from typing import TypedDict, Annotated
from e2b_code_interpreter import Sandbox as CodeInterpreter
import operator

# Define the state
class AnalysisState(TypedDict):
    input: str
    data_loaded: bool
    analysis_code: str
    visualization_code: str
    results: Annotated[list[str], operator.add]
    sandbox_id: str

# Initialize sandbox for the workflow
sandbox = CodeInterpreter()

def load_data(state: AnalysisState):
    """Load data into the sandbox."""
    code = """
import pandas as pd
import numpy as np

# Create sample dataset
data = pd.DataFrame({
    'month': pd.date_range('2024-01', periods=12, freq='M'),
    'revenue': np.random.randint(50000, 150000, 12),
    'expenses': np.random.randint(30000, 100000, 12)
})
print(data.head())
"""
    
    execution = sandbox.notebook.exec_cell(code)
    result = execution.logs.stdout or "Data loaded"
    
    return {
        "data_loaded": True,
        "results": [f"Data Loading:\n{result}"],
        "sandbox_id": sandbox.id
    }

def analyze_data(state: AnalysisState):
    """Perform statistical analysis."""
    code = """
# Calculate key metrics
total_revenue = data['revenue'].sum()
total_expenses = data['expenses'].sum()
profit = total_revenue - total_expenses
avg_revenue = data['revenue'].mean()

print(f"Total Revenue: ${total_revenue:,}")
print(f"Total Expenses: ${total_expenses:,}")
print(f"Net Profit: ${profit:,}")
print(f"Average Monthly Revenue: ${avg_revenue:,.2f}")
"""
    
    execution = sandbox.notebook.exec_cell(code)
    result = execution.logs.stdout or "Analysis complete"
    
    return {
        "analysis_code": code,
        "results": [f"Analysis:\n{result}"]
    }

def create_visualization(state: AnalysisState):
    """Create visualization code."""
    code = """
import matplotlib.pyplot as plt

# Create profit over time chart
data['profit'] = data['revenue'] - data['expenses']
plt.figure(figsize=(10, 6))
plt.plot(data['month'], data['profit'], marker='o')
plt.title('Monthly Profit Trend')
plt.xlabel('Month')
plt.ylabel('Profit ($)')
plt.grid(True)
plt.xticks(rotation=45)
plt.tight_layout()

print("Visualization created successfully")
"""
    
    execution = sandbox.notebook.exec_cell(code)
    result = execution.logs.stdout or "Visualization complete"
    
    return {
        "visualization_code": code,
        "results": [f"Visualization:\n{result}"]
    }

# Build the graph
workflow = StateGraph(AnalysisState)

# Add nodes
workflow.add_node("load_data", load_data)
workflow.add_node("analyze_data", analyze_data)
workflow.add_node("create_visualization", create_visualization)

# Add edges
workflow.add_edge("load_data", "analyze_data")
workflow.add_edge("analyze_data", "create_visualization")
workflow.add_edge("create_visualization", END)

# Set entry point
workflow.set_entry_point("load_data")

# Compile
app = workflow.compile()

# Run the workflow
result = app.invoke({
    "input": "Analyze monthly revenue and expenses",
    "data_loaded": False,
    "analysis_code": "",
    "visualization_code": "",
    "results": [],
    "sandbox_id": ""
})

# Print results
for step_result in result["results"]:
    print(step_result)
    print("-" * 50)

# Clean up
sandbox.close()
```

**Why sandboxes benefit LangGraph workflows:**

1. **State persistence**: Variables and data structures persist across workflow nodes
2. **Dependency installation**: Install packages once, use throughout the workflow
3. **File operations**: Create, modify, and share files between steps
4. **Isolation**: Each workflow runs independently, preventing interference

### Handling Workflow Failures

Sandboxes help you implement robust error handling in LangGraph:

```python
def safe_execute_node(state: AnalysisState):
    """Execute code with error handling."""
    try:
        execution = sandbox.notebook.exec_cell(state["code_to_run"])
        
        if execution.error:
            return {
                "error": execution.error.value,
                "retry_count": state.get("retry_count", 0) + 1
            }
        
        return {
            "results": [execution.logs.stdout],
            "error": None
        }
    except Exception as e:
        return {
            "error": str(e),
            "retry_count": state.get("retry_count", 0) + 1
        }

# In your graph, add conditional edges based on errors
def should_retry(state: AnalysisState):
    if state.get("error") and state.get("retry_count", 0) < 3:
        return "retry"
    elif state.get("error"):
        return "fail"
    else:
        return "continue"

workflow.add_conditional_edges(
    "execute",
    should_retry,
    {
        "retry": "execute",
        "fail": "handle_error",
        "continue": "next_step"
    }
)
```

## Sandbox Lifecycle Considerations for Agents

Understanding how sandboxes work over time helps you design better agent systems.

### Ephemeral vs. Long-Lived Execution

**Ephemeral (default):**
```python
# Sandbox created and destroyed automatically
with CodeInterpreter() as sandbox:
    result = sandbox.notebook.exec_cell("print('Hello')")
# Sandbox is destroyed here
```

Use ephemeral sandboxes when:
- Each agent invocation is independent
- You want guaranteed clean state
- Resource cleanup is critical

**Long-lived:**
```python
# Sandbox persists across operations
sandbox = CodeInterpreter()

# Multiple executions share state
sandbox.notebook.exec_cell("x = 42")
sandbox.notebook.exec_cell("print(x)")  # Outputs: 42

# Explicit cleanup
sandbox.close()
```

Use long-lived sandboxes when:
- Building up state across multiple agent steps
- Installing packages or loading large datasets
- Implementing conversational agents with memory

### Resource Limits

OpenKruise Agents enforces resource limits on sandboxes. Be mindful of:

**CPU limits**: Prevent infinite loops or CPU-intensive operations from affecting other workloads
```python
# This might hit CPU limits
code = """
while True:
    pass  # Infinite loop - will be terminated
"""
```

**Memory limits**: Large data structures or memory leaks are contained
```python
# This might hit memory limits
code = """
import numpy as np
huge_array = np.zeros((100000, 100000))  # May exceed memory quota
"""
```

**Timeout limits**: Long-running operations eventually terminate
```python
# Handle timeouts gracefully
from e2b_code_interpreter import Sandbox as CodeInterpreter

sandbox = CodeInterpreter(timeout=300)  # 5-minute timeout
```

### Why Isolation Matters for Agent Tool Execution

AI agents can generate unexpected or potentially harmful code. Sandboxes provide defense-in-depth:

1. **Filesystem isolation**: Agents can't access host files or other containers
2. **Network boundaries**: Control which external services agents can reach
3. **Process isolation**: Crashed or hanging processes don't affect other agents
4. **Resource fairness**: No single agent can monopolize CPU or memory

Example of risky agent behavior that sandboxes protect against:

```python
# Agent might generate code like this:
risky_code = """
import os
os.system('rm -rf /')  # Attempted to delete filesystem
"""

# In a sandbox, this only affects the sandbox container
# The host system and other sandboxes remain safe
```

## Best Practices

### 1. Keep Agents Stateless Where Possible

Design agents to be idempotent and avoid unnecessary state:

```python
# Good: Stateless execution
def process_query(query: str) -> str:
    with CodeInterpreter() as sandbox:
        # Fresh environment each time
        code = generate_code_for_query(query)
        result = sandbox.notebook.exec_cell(code)
        return result.logs.stdout

# Use with caution: Stateful execution
sandbox = CodeInterpreter()

def process_query_stateful(query: str) -> str:
    # State accumulates over time
    # Risk of side effects and harder to reason about
    code = generate_code_for_query(query)
    result = sandbox.notebook.exec_cell(code)
    return result.logs.stdout
```

### 2. Use Sandbox Persistence Intentionally

Only maintain sandbox state when there's a clear benefit:

```python
# Good use case: Installing dependencies once
sandbox = CodeInterpreter()
sandbox.notebook.exec_cell("pip install scikit-learn pandas")

# Now use the sandbox for multiple related tasks
for dataset in datasets:
    sandbox.notebook.exec_cell(f"analyze_data('{dataset}')")

sandbox.close()

# Poor use case: Keeping sandbox alive "just in case"
# This wastes resources and may lead to accumulated state bugs
```

### 3. Clean Up Unused Sandboxes

Always close sandboxes explicitly or use context managers:

```python
# Preferred: Automatic cleanup
with CodeInterpreter() as sandbox:
    # Your code here
    pass
# Sandbox automatically closed

# Alternative: Explicit cleanup
sandbox = CodeInterpreter()
try:
    # Your code here
    pass
finally:
    sandbox.close()  # Ensures cleanup even on error
```

For long-running applications, implement sandbox lifecycle management:

```python
from datetime import datetime, timedelta

class SandboxManager:
    def __init__(self, max_age_minutes=30):
        self.sandboxes = {}
        self.max_age = timedelta(minutes=max_age_minutes)
    
    def get_or_create(self, session_id: str) -> CodeInterpreter:
        if session_id in self.sandboxes:
            sandbox, created_at = self.sandboxes[session_id]
            
            # Check if sandbox is too old
            if datetime.now() - created_at > self.max_age:
                sandbox.close()
                del self.sandboxes[session_id]
            else:
                return sandbox
        
        # Create new sandbox
        sandbox = CodeInterpreter()
        self.sandboxes[session_id] = (sandbox, datetime.now())
        return sandbox
    
    def cleanup_all(self):
        for sandbox, _ in self.sandboxes.values():
            sandbox.close()
        self.sandboxes.clear()
```

### 4. Avoid Over-Provisioning Resources

Request only the resources your agent actually needs:

```python
# If your agent does light computation, use defaults
sandbox = CodeInterpreter()

# For resource-intensive tasks, configure appropriately
# (Note: Actual resource configuration depends on E2B/OpenKruise setup)
sandbox = CodeInterpreter(
    # timeout=600  # 10 minutes for long-running analysis
)
```

### 5. Handle Errors Gracefully

Always account for execution failures:

```python
from e2b_code_interpreter import Sandbox as CodeInterpreter

def safe_execute(code: str, retries=3):
    """Execute code with retry logic and error handling."""
    for attempt in range(retries):
        try:
            with CodeInterpreter() as sandbox:
                execution = sandbox.notebook.exec_cell(code)
                
                if execution.error:
                    # Log error and potentially modify code
                    print(f"Attempt {attempt + 1} failed: {execution.error}")
                    continue
                
                return execution.logs.stdout
        
        except Exception as e:
            print(f"Sandbox error on attempt {attempt + 1}: {e}")
            if attempt == retries - 1:
                raise
    
    return None
```

### 6. Monitor and Log Sandbox Usage

Implement observability for agent sandbox usage:

```python
import logging

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

def execute_with_logging(code: str, context: dict):
    """Execute code with comprehensive logging."""
    logger.info(f"Creating sandbox for context: {context}")
    
    with CodeInterpreter() as sandbox:
        logger.info(f"Sandbox created: {sandbox.id}")
        
        start_time = datetime.now()
        execution = sandbox.notebook.exec_cell(code)
        duration = (datetime.now() - start_time).total_seconds()
        
        logger.info(f"Execution completed in {duration}s")
        
        if execution.error:
            logger.error(f"Execution error: {execution.error}")
        
        return execution
```

## Conclusion & Next Steps

Running LangChain and LangGraph agents in OpenKruise sandboxes via the E2B API provides a powerful combination of AI capabilities and execution safety. This approach enables you to:

- **Execute AI-generated code safely** with strong isolation guarantees
- **Build complex multi-step workflows** that maintain state across operations
- **Control resource consumption** to prevent runaway agent processes
- **Scale agent deployments** using Kubernetes infrastructure

### When to Use This Integration

This pattern works well when:

- Your agents need to execute code or manipulate files
- You require strong isolation between agent executions
- You're running multiple agents concurrently
- You need reproducible, containerized execution environments
- Resource limits and quotas are important

### Learn More

Explore these resources to deepen your understanding:

- **LangChain Documentation**: [https://python.langchain.com/docs/](https://python.langchain.com/docs/)
- **LangGraph Documentation**: [https://langchain-ai.github.io/langgraph/](https://langchain-ai.github.io/langgraph/)
- **OpenKruise Agents**: [https://github.com/openkruise/agents](https://github.com/openkruise/agents)
- **E2B Documentation**: [https://e2b.dev/docs](https://e2b.dev/docs)

### Next Steps

1. **Experiment** with the code examples in this guide
2. **Build** a simple agent that uses sandbox execution for your use case
3. **Monitor** resource usage and adjust configurations as needed
4. **Contribute** back to the community by sharing your patterns and improvements

By combining the flexibility of LangChain and LangGraph with the security of OpenKruise sandboxes, you can build sophisticated AI agents that are both powerful and safe to deploy in production environments.