"""Equivalent scenarios from test/e2b/test_commands_run.py."""
import time

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
    output, errors, events = [], [], []
    received, completed = [], []
    result = sandbox.commands.run(
        "echo hello; echo error >&2; sleep 1; echo done",
        handlers=ExecutionHandlersSync(
            on_stdout=lambda msg: (output.append(msg.text), events.append("stdout"), received.append(time.monotonic())),
            on_stderr=lambda msg: errors.append(msg.text),
            on_execution_complete=lambda _: (events.append("complete"), completed.append(time.monotonic())),
        ),
    )
    assert result.error is None
    assert "hello" in "".join(output) and "done" in "".join(output)
    assert "error" in "".join(errors)
    assert events[0] == "stdout" and events[-1] == "complete"
    assert completed[0] - received[0] >= 0.5, "output was buffered until command completion"


def test_commands_interrupt(sandbox):
    # Reuse the existing background kill scenario's assertions for a live
    # foreground request; this delivery has no retained background model.
    ids, interrupted = [], []

    def on_stdout(message):
        if "start" in message.text and not interrupted:
            sandbox.commands.interrupt(ids[0])
            interrupted.append(True)

    result = sandbox.commands.run(
        "echo start; sleep 60; echo end",
        handlers=ExecutionHandlersSync(on_init=lambda event: ids.append(event.id), on_stdout=on_stdout),
    )
    assert interrupted and result.error is not None
    assert "end" not in stdout(result)
