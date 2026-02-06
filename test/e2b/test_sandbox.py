import time
import uuid

import pytest
from e2b.exceptions import NotFoundException
from e2b_code_interpreter import Sandbox, SandboxQuery, SandboxState

from utils import list_sandbox, connect_sandbox, run_code_sandbox


# Link: https://e2b.dev/docs/sandbox
def test_lifecycle(sandbox_context):
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        metadata={
            'userId': '123',
        },
    ))
    print(f"sandbox-id: {sandbox.sandbox_id}")
    info = sandbox.get_info()
    print(info)
    assert info.template_id == "code-interpreter"
    assert info.state == SandboxState.RUNNING
    assert info.metadata["userId"] == "123"


def test_list_by_metadata(sandbox_context):
    random_user_id = str(uuid.uuid4())

    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        metadata={
            'userId': random_user_id,
        },
    ))
    print(f"sandbox-id: {sandbox.sandbox_id}")
    info = sandbox.get_info()
    print(info)
    # List sandboxes that are running or paused.
    sandboxes = list_sandbox(
        query=SandboxQuery(
            metadata={
                "userId": random_user_id,
            }
        ),
    )
    assert len(sandboxes) == 1
    assert sandboxes[0].sandbox_id == info.sandbox_id


def test_list_by_state(sandbox_context):
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        metadata={"test_case": "test_list_by_state"},
    ))
    print(f"sandbox-id: {sbx.sandbox_id}")
    info = sbx.get_info()
    print(info)
    # List sandboxes that are running or paused.
    sandboxes = list_sandbox(
        query=SandboxQuery(
            state=[SandboxState.RUNNING, SandboxState.PAUSED],
        ),
    )

    found = False
    for sandbox in sandboxes:
        if sandbox.sandbox_id == sbx.sandbox_id:
            found = True
            break
    if not found:
        raise AssertionError(
            f"Sandbox {sbx.sandbox_id} not found in running sandboxes list"
        )
    print("sandbox found in list")


def test_timeout(sandbox_context):
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        metadata={"case": "timeout"},
        timeout=30,
    ))
    sandbox2: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=1200,
        metadata={"test_case": "test_timeout"},
    ))
    sandbox2.set_timeout(10)
    print(f"wait 10s timeout and check sandbox2 {sandbox2.sandbox_id} deleted")
    time.sleep(10)
    with pytest.raises(NotFoundException):
        connect_sandbox(sandbox2)
    sandbox.get_info()  # still exists

    print(f"wait 20s again and check sandbox {sandbox.sandbox_id} deleted")
    time.sleep(20)
    with pytest.raises(NotFoundException):
        connect_sandbox(sandbox)


def test_pause_connect_kill(sandbox_context):
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=6000,
        metadata={"test_case": "test_pause_connect_kill"},
    ))
    sandbox.beta_pause()
    print(f"wait 30s and check sandbox {sandbox.sandbox_id} paused")
    time.sleep(30)
    print(f"trying to connect sandbox")
    connect_sandbox(sandbox)
    run_code_sandbox(sandbox, "Hello, world")  # check work
    print(f"sandbox is working after resume")


def test_pause_kill(sandbox_context):
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=6000,
        metadata={"test_case": "test_pause_kill"},
    ))
    sandbox.beta_pause()
    time.sleep(1)


def test_pause_state(sandbox_context):
    sbx = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=6000,
        metadata={"test_case": "test_pause_state"},
    ))
    print('Sandbox created', sbx.sandbox_id)

    sbx.beta_pause()
    print('Sandbox paused', sbx.sandbox_id)

    sandboxes = list_sandbox(SandboxQuery(state=[SandboxState.PAUSED]))

    found = False
    for sandbox in sandboxes:
        if sandbox.sandbox_id == sbx.sandbox_id:
            found = True
            break

    if not found:
        raise AssertionError(f"Sandbox {sbx.sandbox_id} not found in paused sandboxes list")
    print(f"sandbox found in paused list")


def test_resume_state(sandbox_context):
    sbx = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=6000,
        metadata={"test_case": "test_resume_state"},
    ))
    print('Sandbox created', sbx.sandbox_id)

    sbx.beta_pause()
    print('Sandbox paused', sbx.sandbox_id)
    time.sleep(30)
    same_sbx = connect_sandbox(sbx)
    print('Connected to the sandbox', same_sbx.sandbox_id)

    sandboxes = list_sandbox(SandboxQuery(state=[SandboxState.RUNNING]))

    found = False
    for sandbox in sandboxes:
        if sandbox.sandbox_id == sbx.sandbox_id:
            found = True
            break
    if not found:
        raise AssertionError(f"Sandbox {sbx.sandbox_id} not found in running sandboxes list")
    print(f"sandbox found in running list")


def test_is_running(sandbox_context):
    sbx = Sandbox.create(template="code-interpreter")
    assert sbx.is_running()  # Returns True

    sbx.kill()
    assert not sbx.is_running()  # Returns False


def test_inplace_update(sandbox_context):
    pytest.skip("inplace update is not supported yet")
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        metadata={
            "case": "inplace-update",
            "e2b.agents.kruise.io/image": "registry-ap-southeast-1.ack.aliyuncs.com/acs/code-interpreter:v1.6-new"
        },
    ))
    file = sbx.files.read("/root/test-file")
    assert file == "xxxx"
