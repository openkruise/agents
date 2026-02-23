import base64
import time

import pytest
from e2b_code_interpreter import Sandbox

from utils import run_code_sandbox


def execute_python_code(s: Sandbox, code: str, expect_stdout: list[str]):
    # Execute Python code inside the sandbox
    execution = run_code_sandbox(s, code)
    if execution.error:
        raise Exception(execution.error)
    assert execution.logs.stdout == expect_stdout


def test_run_code(sandbox_context):
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        metadata={"test_case": "test_run_code"},
    ))
    execute_python_code(sbx, "print('hello world')", ["hello world\n"])
    execute_python_code(sbx, "a = 1", [])
    execute_python_code(sbx, "b = 2", [])
    execute_python_code(sbx, "print(a + b)", ["3\n"])
    execute_python_code(sbx, """
    def bubble_sort(arr):
        n = len(arr)
        sorted_arr = arr.copy()

        for i in range(n):
            swapped = False
            for j in range(0, n - i - 1):
                if sorted_arr[j] > sorted_arr[j + 1]:
                    sorted_arr[j], sorted_arr[j + 1] = sorted_arr[j + 1], sorted_arr[j]
                    swapped = True
            if not swapped:
                break

        return sorted_arr
    print(bubble_sort([1,6,4,2,3,7,5]))
    """, ["[1, 2, 3, 4, 5, 6, 7]\n"])


def test_static_charts(sandbox_context):
    code_to_run = """
    import matplotlib.pyplot as plt

    plt.plot([1, 2, 3, 4])
    plt.ylabel('some numbers')
    plt.show()
    """

    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        metadata={"test_case": "test_static_charts"},
    ))
    # Run the code inside the sandbox
    execution = run_code_sandbox(sandbox, code_to_run)

    assert len(execution.results) == 1
    # There's only one result in this case - the plot displayed with `plt.show()`
    first_result = execution.results[0]
    assert first_result.png is not None

    # Save the png to a file. The png is in base64 format.
    with open('chart.png', 'wb') as f:
        f.write(base64.b64decode(first_result.png))
    print('Chart saved as chart.png')


def test_code_stream(sandbox_context):
    code_to_run = """
    import matplotlib.pyplot as plt

    # Prepare data
    categories = ['Category A', 'Category B', 'Category C', 'Category D']
    values = [10, 20, 15, 25]

    # Create and customize the bar chart
    plt.figure(figsize=(10, 6))
    plt.bar(categories, values, color='green')
    plt.xlabel('Categories')
    plt.ylabel('Values')
    plt.title('Values by Category')

    # Display the chart
    plt.show()
    """

    def on_result_callback(result):
        nonlocal code_result
        code_result = result
        print('result:', result)

    code_result = None
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        metadata={"test_case": "test_code_stream"},
    ))
    run_code_sandbox(
        sandbox,
        code_to_run,
        on_result=on_result_callback,
    )
    assert code_result is not None


def test_pause_resume_code_context(sandbox_context):
    pytest.skip("Not implemented yet")
    """Test creating a code context, running code, pausing and resuming, then running more code"""
    # 1) Create a code context (sandbox)
    sbx: Sandbox = Sandbox.create(
        template="code-interpreter",
        timeout=6000,
    )
    print(f"Sandbox created: {sbx.sandbox_id}")

    # Create a code context and save the context ID
    context = sbx.create_code_context()
    print(f"Code context created: {context.id}")

    # 2) Run some Python code in the context
    execution = run_code_sandbox(sbx, "x = 10", context=context)
    assert not execution.error
    execution = run_code_sandbox(sbx, "y = 20", context=context)
    assert not execution.error
    execution = run_code_sandbox(sbx, "print(f'x = {x}, y = {y}')", context=context)
    assert not execution.error
    assert execution.logs.stdout == ["x = 10, y = 20\n"]

    # 3) Pause and resume (connect)
    print("Pausing sandbox...")
    sbx.beta_pause()
    print(f"Sandbox paused: {sbx.sandbox_id}")

    print("Sleeping...")
    time.sleep(120)

    print("Resuming (connecting) to sandbox...")
    sbx = sbx.connect()
    print(f"Connected to sandbox: {sbx.sandbox_id}")

    # 4) Run some Python code in the context (created before)
    # Variables and functions defined before pause should still exist
    # Use the same context ID
    execution = run_code_sandbox(sbx, "print(f'x = {x}, y = {y}')", context=context)
    assert not execution.error
    assert execution.logs.stdout == ["x = 10, y = 20\n"]

    assert sbx.kill()
