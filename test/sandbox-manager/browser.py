from utils import  disable_ssl_verification

disable_ssl_verification()

import asyncio
import os
import time

from browser_use import Agent, BrowserSession
from browser_use.llm import ChatOpenAI
from e2b_code_interpreter import Sandbox

async def screenshot(agent: Agent):
    # 截图功能
    try:
        print("开始截图...")
        page = await agent.browser_session.get_current_page()
        screenshot_bytes = await page.screenshot(full_page=True, type='png')
        # screenshot 方法返回图像的二进制数据，将其保存为 PNG 文件
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
    sandbox = Sandbox.create(api_key="GG", domain="e2b-demo.ultramarines.cn", template="browser")
    print(f"sandboxID: {sandbox.sandbox_id}")

    try:
        # 创建 Browser-use 会话
        browser_session = BrowserSession(cdp_url=f"https://api.{sandbox.sandbox_domain}/browser/{sandbox.sandbox_id}") # 使用 cdp 协议连接远程沙箱中的浏览器
        await browser_session.start()
        print("Browser-use 会话创建成功")

        # 创建 AI Agent
        agent = Agent(
            task="""
            从阿里云 ACS 产品计费官方文档目录（https://help.aliyun.com/zh/cs/product-overview/billing/）找到计费说明，总结不同地域、计算类型、算力质量的费用差别
            """,
            llm=ChatOpenAI(
                api_key=os.getenv("LLM_API_KEY"),
                base_url=os.getenv("LLM_BASE_URL"),
                model=os.getenv("LLM_MODEL"),
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