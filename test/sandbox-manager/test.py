import os
import time

from utils import disable_ssl_verification

disable_ssl_verification()

from dotenv import load_dotenv

load_dotenv()
# Import the E2B SDK
from e2b_code_interpreter import Sandbox

# Create a sandbox using the E2B Python SDK
# Connect to the local E2B on Kubernetes server

sbx: Sandbox = Sandbox.create(api_key="GG", template="code-interpreter", timeout=300, metadata={
    "foo": "bar"
})

# sbx: Sandbox = Sandbox.connect("code-interpreter-b86c7d5c6-x5dq6", api_key="GG")

print(f"sandbox id: {sbx.sandbox_id}")
# sbx2: Sandbox = Sandbox.create(api_key="GG", template="code-interpreter", timeout=30, metadata={
#     "__auto_pause__": "10",
#     "foo": "bar"
# })
# # Sandbox.connect(sbx.sandbox_id)
# #
#
# print(f"sandbox2 id: {sbx2.sandbox_id}")
# def list_and_print_sandboxes(metadata: dict):
#     pager = Sandbox.list(api_key="GG", query=SandboxQuery(metadata=metadata))
#     all_sandboxes = pager.next_items()
#     print(f"listed sandboxes with metadata {metadata}: {all_sandboxes}")
#
# list_and_print_sandboxes({"foo": "bar"})
# list_and_print_sandboxes({"not": "exists"})
# list_and_print_sandboxes({})

def execute_python_code(s: Sandbox, code: str):
    # Execute Python code inside the sandbox
    execution = s.run_code(code)
    if execution.error:
        print(f"Error executing code: {execution.error}")
    else:
        print(execution.logs.stdout)


execute_python_code(sbx, "print('hello world')")
execute_python_code(sbx, "a = 1")
execute_python_code(sbx, "b = 2")
execute_python_code(sbx, "print(a + b)")

sbx.beta_pause(api_key="GG")
print('Sandbox paused')

# try:
#     execute_python_code(sbx, "print('I am paused')")
# except Exception as e:
#     print(f"Error executing code: {e}")

input("Press ENTER to resume the sandbox")

# Connect to the sandbox (it will automatically resume the sandbox, if paused)
same_sbx = sbx.connect(api_key="GG")
print('Connected to the sandbox', same_sbx.sandbox_id)
execute_python_code(sbx, """
def bubble_sort(arr):
    n = len(arr)
    # 创建数组副本，避免修改原数组
    sorted_arr = arr.copy()
    
    # 外层循环控制排序轮数
    for i in range(n):
        # 标记本轮是否发生交换
        swapped = False
        # 内层循环进行相邻元素比较
        for j in range(0, n - i - 1):
            # 如果前一个元素大于后一个元素，则交换
            if sorted_arr[j] > sorted_arr[j + 1]:
                sorted_arr[j], sorted_arr[j + 1] = sorted_arr[j + 1], sorted_arr[j]
                swapped = True
        # 如果本轮没有发生交换，说明数组已经有序，可以提前结束
        if not swapped:
            break
    
    return sorted_arr
print(bubble_sort([1,6,4,2,3,7,5]))
""")

# List files in the sandbox
try:
    # Read local file relative to the current working directory
    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "joke.txt"), "rb") as file:
        # Upload file to the sandbox to absolute path '/home/user/my-file'
        sbx.files.write("/home/user/my-file", file)
    file_content = sbx.files.read("/home/user/my-file")
    print(file_content)
except Exception as e:
    # Print the full stack trace for debugging
    import traceback

    traceback.print_exc()
    print(f"Error listing files: {e}")
    raise e
finally:
    input("Press ENTER to kill the sandbox")
    sbx.kill()
    print(f"sandbox {sbx.sandbox_id} killed")
