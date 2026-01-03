# 运行 E2B desktop 沙箱

该示例演示了如何通过 OpenKruise Agents 部署 [E2B](https://e2b.dev/) desktop 沙箱并通过 E2B SDK 进行调用。

关于基础概念，请参考 [运行 E2B 代码执行沙箱](../code_interpreter)

## 1. 定义模板

类似 code-interpreter 模板，我们可以通过 `SandboxSet` 定义一个使用官方 E2B Desktop 镜像的模板并创建预热池。

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
          image: e2bdev/desktop:latest # 使用 E2B 官方的 desktop 镜像
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
        - name: agent-runtime-volume # 定义 agent-runtime 与主容器的共享目录
          emptyDir: { }
```

## 2. 通过 E2B Python SDK 使用沙箱

指定 E2B 后端地址：

```shell
export E2B_DOMAIN=your.domain
export E2B_API_TOKEN=your-token
```

您可以通过以下命令安装 E2B desktop Python SDK:

```shell
pip install e2b-desktop
```

### 2.1 E2B 标准能力

#### 2.1.1 串流远程桌面

通过以下代码，您可以打开一个可交互的远程桌面串流。

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

#### 2.1.2 控制远程桌面

以下代码演示了打开浏览器、输入网址等操作。

```python

from e2b_desktop import Sandbox

with Sandbox.create(template="desktop") as desktop:
    desktop.launch('google-chrome')
    desktop.write('https://openkruise.io')
    desktop.press('enter')
    input("press ENTER to exit")

```

#### 2.1.3 其他基础

除了上述的桌面功能，包括命令执行、文件操作、休眠唤醒等基础功能同样支持。

### 2.2 OpenKruise Agents 扩展能力

除了 E2B 标准能力外，OpenKruise Agents 还提供了一系列扩展功能。

#### 2.2.1 CDP（Chrome DevTools Protocol）支持

`sandbox-manager` 提供了 CDP 握手支持，使得 Agent 能够直接通过 CDP 协议远程操作沙箱中的 Chrome 浏览器。这在使用
[browser-use](https://browser-use.com/) 等框架时十分有用。下面是一个使用 browser-use 的例子，这个例子将会创建一个操作浏览器的 Agent。

```python
import asyncio
import os
import time

from browser_use import Agent, BrowserSession
from browser_use.llm import ChatOpenAI
from e2b_code_interpreter import Sandbox

async def screenshot(agent: Agent):
    try:
        print("开始截图...")
        page = await agent.browser_session.get_current_page()
        screenshot_bytes = await page.screenshot(full_page=True, type='png')
        screenshots_dir = os.path.join(".", "screenshots")
        os.makedirs(screenshots_dir, exist_ok=True)
        screenshot_path = os.path.join(screenshots_dir, f"{time.time()}.png")
        with open(screenshot_path, "wb") as f:
            f.write(screenshot_bytes)
        print(f"截图已保存至 {screenshot_path}")
    except Exception as e:
        print(f"截图失败: {e}")

async def main():
    # 创建 E2B 沙箱实例
    sandbox = Sandbox.create(template="browser") # 一个已经运行 Chrome 的容器
    try:
        # 创建 Browser-use 会话
        browser_session = BrowserSession(cdp_url=f"https://api.{sandbox.sandbox_domain}/browser/{sandbox.sandbox_id}") # 使用 cdp 协议连接远程沙箱中的浏览器
        await browser_session.start()
        print("Browser-use 会话创建成功")

        # 创建 AI Agent
        agent = Agent(
            task="""
            简要介绍一下 OpenKruise 家族的各个子项目。
            """,
            llm=ChatOpenAI(
                api_key=os.getenv("LLM_API_KEY"),
                base_url=os.getenv("LLM_BASE_URL"),
                model="qwen-plus",
                temperature=1,
            ),
            browser_session=browser_session,
        )

        # 运行 Agent 任务
        print("开始执行 Agent 任务...")
        await agent.run(
            on_step_end=screenshot, # 在每个步骤结束时调用 screenshot 截图
        )

        # 关闭浏览器会话
        await browser_session.close()
        print("任务执行完成")

    finally:
        # 清理沙箱资源
        sandbox.kill()
        print("沙箱资源已清理")

if __name__ == "__main__":
    asyncio.run(main())
```