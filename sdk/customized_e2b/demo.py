import time
import sys

from dotenv import load_dotenv
load_dotenv()

# Patch and import the E2B SDK
from kruise_agents.e2b import patch_e2b
from e2b_code_interpreter import Sandbox
patch_e2b()

try:
    start = time.time()
    sbx: Sandbox = Sandbox.create(template="code-interpreter", timeout=300, metadata={
        "foo": "bar"
    })
    end = time.time()
    print(f"sandbox id: {sbx.sandbox_id}", file=sys.stderr)
    print(f"sandbox url: {sbx.sandbox_domain}")
    # Connect to the sandbox (it will automatically resume the sandbox, if paused)
    same_sbx = Sandbox.connect(sbx.sandbox_id)
    print('Connected to the sandbox', same_sbx.sandbox_id, file=sys.stderr)

except Exception as e:
    print(f"Error creating sandbox: {e}", file=sys.stderr)
    raise e


def execute_python_code(s: Sandbox, code: str):
    # Execute Python code inside the sandbox
    execution = s.run_code(code)
    if execution.error:
        print(f"Error executing code: {execution.error}", file=sys.stderr)
    else:
        print(execution.logs.stdout)


try:
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
except Exception as e:
    print(f"Error executing code: {e}", file=sys.stderr)
    raise e

# List files in the sandbox
try:
    sbx.files.write("/home/user/my-file", "Hello, World")
    file_content = sbx.files.read("/home/user/my-file")
    print(file_content)
except Exception as e:
    print(f"Error listing files: {e}", file=sys.stderr)
    raise e
finally:
    sbx.kill()
    print(f"{end - start:.4f}")
