"""Default foreground command scenarios, one per E2B equivalent in
test/e2b/test_commands_run.py. The runner executes this file unchanged on both
the ingress and the direct Pod IP path.

Behavior beyond these four cases lives in `test_contract.py`; the omitted/null
timeout wire mapping (default 60s / no deadline) is verified by the targeted Go
commandTimeout tests.
"""
from opensandbox.models.execd_sync import ExecutionHandlersSync


def stdout(result):
    return "".join(message.text for message in result.logs.stdout)


def test_commands_run(sandbox):
    result = sandbox.commands.run("echo 'hello world'")
    assert result.error is None and result.exit_code == 0
    assert stdout(result) == "hello world\n"
    assert "/home/user" in stdout(sandbox.commands.run("pwd"))
    assert sandbox.commands.run("touch test_file.txt; mkdir test_dir").error is None
    result = sandbox.commands.run("ls")
    assert "test_file.txt" in stdout(result) and "test_dir" in stdout(result)


def test_commands_run_error_handling(sandbox):
    result = sandbox.commands.run("nonexistentcommand12345")
    assert result.exit_code == 127 and result.error is not None
    assert "command not found" in "".join(message.text for message in result.logs.stderr)
    result = sandbox.commands.run("ls /nonexistent_directory")
    assert result.exit_code != 0 and result.error is not None
    assert result.logs.stderr


def test_commands_realtime_callbacks(sandbox):
    output, errors = [], []
    sandbox.commands.run(
        "echo hello; echo error >&2",
        handlers=ExecutionHandlersSync(
            on_stdout=lambda msg: output.append(msg.text),
            on_stderr=lambda msg: errors.append(msg.text),
        ),
    )
    assert "hello" in "".join(output)
    assert "error" in "".join(errors)


def test_commands_interrupt(sandbox):
    execution_id = None
    output = ""
    interrupted = False

    def on_init(event):
        nonlocal execution_id
        execution_id = event.id

    def on_stdout(message):
        nonlocal output, interrupted
        output += message.text
        if not interrupted and "start\n" in output:
            assert execution_id, "expected an execution ID before interrupt"
            sandbox.commands.interrupt(execution_id)
            interrupted = True

    sandbox.commands.run(
        "echo start; sleep 60; echo end",
        handlers=ExecutionHandlersSync(on_init=on_init, on_stdout=on_stdout),
    )
    assert interrupted, "expected interrupt to succeed after the command emitted start"
