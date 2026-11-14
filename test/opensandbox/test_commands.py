"""Command scenarios from test/e2b/test_commands_run.py plus native-aligned
behavior assertions: login shell, argv boundaries, execution timeout, PID-based
interrupt error mapping and post-disconnect process survival.

The omitted/null timeout wire mapping (default 60s / no deadline) is verified by
the targeted Go commandTimeout tests; the SDK exercises positive and explicit-zero
timeouts here.
"""
import json
import time
from datetime import timedelta

import httpx
import pytest
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.execd import RunCommandOpts
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


def test_commands_login_shell_and_argv(sandbox):
    # String commands run through a login shell (/bin/bash -l -c).
    assert "login-shell" in stdout(sandbox.commands.run("shopt -q login_shell && echo login-shell"))
    # argv preserves independent argument boundaries instead of shell joining.
    result = sandbox.commands.run(["/bin/echo", "hello world", "x"])
    assert result.error is None
    assert stdout(result) == "hello world x\n"


def test_commands_timeout(sandbox):
    # A positive timeout terminates a long command well before it finishes.
    start = time.monotonic()
    result = sandbox.commands.run("sleep 30", opts=RunCommandOpts(timeout=timedelta(seconds=1)))
    elapsed = time.monotonic() - start
    assert elapsed < 10, f"timeout did not terminate the command early: {elapsed:.1f}s"
    assert result.error is not None
    # Explicit zero adds no deadline; a short command still completes normally.
    ok = sandbox.commands.run("echo no-deadline", opts=RunCommandOpts(timeout=timedelta(0)))
    assert ok.error is None and stdout(ok) == "no-deadline\n"


def test_commands_interrupt_invalid_id(sandbox):
    for bad_id in ("not-a-pid", "0", "-1"):
        with pytest.raises(SandboxApiException) as exc:
            sandbox.commands.interrupt(bad_id)
        assert exc.value.status_code == 400


def test_commands_interrupt_ended_id(sandbox):
    result = sandbox.commands.run("echo finished")
    assert result.error is None and result.id
    with pytest.raises(SandboxApiException) as exc:
        sandbox.commands.interrupt(result.id)
    assert exc.value.status_code == 404


def test_commands_disconnect_survival_and_interrupt(sandbox):
    endpoint = sandbox.get_endpoint(44772)
    protocol = sandbox.connection_config.protocol
    url = f"{protocol}://{endpoint.endpoint}/command"
    headers = {**endpoint.headers, "Accept": "text/event-stream"}
    pid = None
    with httpx.Client(timeout=httpx.Timeout(30, read=None)) as client:
        with client.stream("POST", url, json={"command": "sleep 30"}, headers=headers) as response:
            assert response.status_code == 200
            for line in response.iter_lines():
                if not line.startswith("data: "):
                    continue
                event = json.loads(line[len("data: "):])
                if event.get("type") == "init":
                    pid = event["text"]
                    break
    assert pid and pid.isdigit(), "expected a PID execution id from the init event"
    # Closing the client stream must not terminate the native process.
    assert sandbox.commands.run(f"kill -0 {pid}").exit_code == 0
    # Interrupt by PID after disconnection terminates the process.
    sandbox.commands.interrupt(pid)
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if sandbox.commands.run(f"kill -0 {pid}").exit_code != 0:
            break
        time.sleep(0.2)
    assert sandbox.commands.run(f"kill -0 {pid}").exit_code != 0, "process survived interrupt"
    # A second interrupt of the same ended PID returns 404.
    with pytest.raises(SandboxApiException) as exc:
        sandbox.commands.interrupt(pid)
    assert exc.value.status_code == 404
