"""Contract acceptance from the data-plane proposal: PID execution identity and
explicit interrupt, stream closure, single-batch file attributes, path
resolution and byte Range.

These scenarios go beyond the E2B-equivalent defaults in `test_commands.py` and
`test_files.py`, so the runner keeps them out of the dual-path default matrix
and executes them on one access path.
"""
import json
import shlex
import time
from datetime import timedelta

import httpx
import pytest
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.execd import RunCommandOpts
from opensandbox.models.filesystem import WriteEntry

pytestmark = pytest.mark.contract


def stdout(result):
    return "".join(message.text for message in result.logs.stdout)


def stat_field(sandbox, path, fmt):
    result = sandbox.commands.run(f"stat -c {shlex.quote(fmt)} {shlex.quote(path)}")
    assert result.error is None, f"stat {path}: {result.error}"
    return stdout(result).strip()


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


def test_files_default_mode(sandbox):
    path = "/home/user/default-mode"
    sandbox.files.write_file(path, "content")  # SDK default mode=755
    assert stat_field(sandbox, path, "%a") == "755"


def test_files_explicit_mode(sandbox):
    for mode in (644, 600):
        path = f"/home/user/mode-{mode}"
        sandbox.files.write_file(path, "content", mode=mode)
        assert stat_field(sandbox, path, "%a") == str(mode)


def test_files_owner_and_group(sandbox):
    cases = [
        ("owner-only", {"owner": "root"}, "%U", "root"),
        ("group-only", {"group": "root"}, "%G", "root"),
        ("both", {"owner": "root", "group": "root"}, "%U %G", "root root"),
    ]
    for name, attrs, fmt, expected in cases:
        path = f"/home/user/{name}"
        sandbox.files.write_file(path, "content", **attrs)
        assert stat_field(sandbox, path, fmt) == expected


def test_files_path_resolution(sandbox):
    # Relative and ~ paths resolve along the batch file user's HOME (/home/user).
    sandbox.commands.run("mkdir -p /home/user/paths-a /home/user/paths-b")
    sandbox.files.write_file("relative-file", "rel")
    assert sandbox.files.read_file("relative-file") == "rel"
    sandbox.files.write_file("~/tilde-file", "tilde")
    assert sandbox.files.read_file("/home/user/tilde-file") == "tilde"
    # Same basename in distinct directories stays independent.
    sandbox.files.write_files([
        WriteEntry(path="/home/user/paths-a/same.txt", data="alpha"),
        WriteEntry(path="/home/user/paths-b/same.txt", data="beta"),
    ])
    assert sandbox.files.read_file("/home/user/paths-a/same.txt") == "alpha"
    assert sandbox.files.read_file("/home/user/paths-b/same.txt") == "beta"


def test_files_download_range(sandbox):
    path = "/home/user/range-file"
    sandbox.files.write_file(path, "0123456789")
    assert sandbox.files.read_file(path, range_header="bytes=0-3") == "0123"
    assert sandbox.files.read_file(path, range_header="bytes=4-7") == "4567"
    # Line-based offset/limit download is not supported.
    with pytest.raises(SandboxApiException) as exc:
        sandbox.files.read_file(path, offset=1, limit=2)
    assert exc.value.status_code == 501
