# Running E2B Desktop Sandbox

This example demonstrates how to deploy an [E2B](https://e2b.dev/) desktop sandbox through OpenKruise Agents and invoke
it via the E2B SDK.

For basic concepts, please refer to [Running E2B Code Interpreter Sandbox](../code_interpreter)

## 1. Defining Templates

Similar to the code-interpreter template, we can define a template using the official E2B Desktop image and create a
pre-warming pool via `SandboxSet`.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  annotations:
    # Enable SandboxManager's Envd initialization capability
    e2b.agents.kruise.io/should-init-envd: "true"
  name: code-interpreter
  namespace: default
spec:
  # Pre-warming pool size, recommended to be slightly larger than the estimated request burst volume
  replicas: 100
  template: # Declare a Pod template
    spec:
      initContainers:
        - name: init # Inject agent-runtime component through native sidecar
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
          image: e2bdev/desktop:latest # Use the official E2B desktop image
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
      volumes:
        - name: agent-runtime-volume # Define the shared directory between agent-runtime and main container
          emptyDir: { }
```

## 2. Using Sandboxes via E2B Python SDK

Specify the E2B backend address:

```shell
export E2B_DOMAIN=your.domain
export E2B_API_TOKEN=your-token
```

You can install the E2B desktop Python SDK with the following command:

```shell
pip install e2b-desktop
```

### 2.1 E2B Standard Capabilities

#### 2.1.1 Streaming Remote Desktop

With the following code, you can open an interactive remote desktop stream.

```python
import time

from e2b_desktop import Sandbox

# Create a new desktop sandbox
desktop = Sandbox.create(template="desktop")
print(f"sandboxId: {desktop.sandbox_id}")
# Note: There can be only one stream at a time
# You need to stop the current stream before streaming another application
desktop.stream.start(
    # window_id=desktop.get_current_window_id(), # if not provided the whole desktop will be streamed
    require_auth=True
)

# Get the stream auth key
auth_key = desktop.stream.get_auth_key()

# Print the stream URL
print('Stream URL:', desktop.stream.get_url(auth_key=auth_key))
input("press ENTER to exit")

# Kill the sandbox after the tasks are finished
desktop.kill()
```

#### 2.1.2 Controlling Remote Desktop

The following code demonstrates operations such as opening a browser and entering a URL.

```python

from e2b_desktop import Sandbox

with Sandbox.create(template="desktop") as desktop:
    desktop.launch('google-chrome')
    desktop.write('https://openkruise.io')
    desktop.press('enter')
    input("press ENTER to exit")

```

#### 2.1.3 Other Basics

In addition to the above desktop features, basic functions such as command execution, file operations, pause/resume, etc.
are also supported.

### 2.2 OpenKruise Agents Extended Capabilities

In addition to E2B standard capabilities, OpenKruise Agents also provides a series of extended functions.

#### 2.2.1 CDP (Chrome DevTools Protocol) Support

`sandbox-manager` provides CDP handshake support, enabling the Agent to remotely operate the Chrome browser in the
sandbox directly through the CDP protocol. This is very useful when using frameworks
like [browser-use](https://browser-use.com/). Below is an example using browser-use that will create an Agent that
operates a browser.

```python
import asyncio
import os
import time

from browser_use import Agent, BrowserSession
from browser_use.llm import ChatOpenAI
from e2b_code_interpreter import Sandbox

async def screenshot(agent: Agent):
    try:
        print("Starting screenshot...")
        page = await agent.browser_session.get_current_page()
        screenshot_bytes = await page.screenshot(full_page=True, type='png')
        screenshots_dir = os.path.join(".", "screenshots")
        os.makedirs(screenshots_dir, exist_ok=True)
        screenshot_path = os.path.join(screenshots_dir, f"{time.time()}.png")
        with open(screenshot_path, "wb") as f:
            f.write(screenshot_bytes)
        print(f"Screenshot saved to {screenshot_path}")
    except Exception as e:
        print(f"Screenshot failed: {e}")

async def main():
    # Create E2B sandbox instance
    sandbox = Sandbox.create(template="browser") # A container with Chrome already running
    try:
        # Create Browser-use session
        browser_session = BrowserSession(cdp_url=f"https://api.{sandbox.sandbox_domain}/browser/{sandbox.sandbox_id}") # Connect to the browser in the remote sandbox using the cdp protocol
        await browser_session.start()
        print("Browser-use session created successfully")

        # Create AI Agent
        agent = Agent(
            task="""
            Make a brief introduction to the projects of the OpenKruise family.
            """,
            llm=ChatOpenAI(
                api_key=os.getenv("LLM_API_KEY"),
                base_url=os.getenv("LLM_BASE_URL"),
                model="qwen-plus",
                temperature=1,
            ),
            browser_session=browser_session,
        )

        # Run Agent task
        print("Starting Agent task execution...")
        await agent.run(
            on_step_end=screenshot, # Call screenshot at the end of each step
        )

        # Close browser session
        await browser_session.close()
        print("Task execution completed")

    finally:
        # Clean up sandbox resources
        sandbox.kill()
        print("Sandbox resources cleaned up")

if __name__ == "__main__":
    asyncio.run(main())
```