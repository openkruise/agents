# test_commands_run.py
from e2b.sandbox.commands.command_handle import CommandExitException
from e2b_code_interpreter import Sandbox


def execute_shell_command(s: Sandbox, command: str, expect_stdout: list[str] = None, expect_stderr: list[str] = None):
    """
    Execute shell commands inside the sandbox

    Args:
        s: Sandbox instance
        command: Shell command to execute
        expect_stdout: Expected stdout lines
        expect_stderr: Expected stderr lines
    """
    # Execute shell command inside the sandbox
    result = s.commands.run(command)

    if result.error:
        raise Exception(result.error)

    if expect_stdout is not None:
        # Fix: Convert stdout string to list for comparison
        actual_stdout = [result.stdout] if result.stdout else []
        assert actual_stdout == expect_stdout

    if expect_stderr is not None:
        # Fix: Convert stderr string to list for comparison
        actual_stderr = [result.stderr] if result.stderr else []
        assert actual_stderr == expect_stderr

    return result


def test_commands_run(sandbox_context):
    """Test basic shell command execution"""
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        metadata={"test_case": "test_commands_run"},
    ))

    # Test simple echo command
    execute_shell_command(sbx, "echo 'hello world'", ["hello world\n"])

    # Test pwd command
    result = execute_shell_command(sbx, "pwd")
    assert "/home/user" in result.stdout  # Fix: result.stdout is string

    # Test ls command in empty directory
    execute_shell_command(sbx, "ls", [])  # Should have no output

    # Test file creation and listing
    execute_shell_command(sbx, "touch test_file.txt")
    result = execute_shell_command(sbx, "ls")
    assert "test_file.txt" in result.stdout  # Fix: Check content in stdout string

    # Test directory creation
    execute_shell_command(sbx, "mkdir test_dir")
    execute_shell_command(sbx, "ls -la | grep test_dir")  # Just check it doesn't error


def test_commands_run_error_handling(sandbox_context):
    """Test error handling for invalid commands"""
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        metadata={"test_case": "test_commands_run_error_handling"},
    ))

    # Test non-existent command - should catch CommandExitException
    try:
        result = sbx.commands.run("nonexistentcommand12345")
        assert False, "Should have raised CommandExitException"
    except CommandExitException as e:
        assert e.exit_code == 127
        assert "command not found" in e.stderr

    # Test command with stderr output
    try:
        result = sbx.commands.run("ls /nonexistent_directory")
        assert False, "Should have raised CommandExitException"
    except CommandExitException as e:
        assert e.exit_code != 0
        assert len(e.stderr) > 0


def test_commands_background_execution(sandbox_context):
    """Test background command execution and killing"""
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        metadata={"test_case": "test_commands_background_execution"},
    ))

    # Start a long-running command in background
    command = sbx.commands.run('echo start; sleep 5; echo end', background=True)

    # Collect output through iteration
    stdout_output = []
    stderr_output = []

    try:
        for stdout, stderr, _ in command:
            if stdout:
                stdout_output.append(stdout)
            if stderr:
                stderr_output.append(stderr)
    except Exception:
        pass  # Command might be killed

    # Kill the command
    command.kill()


def test_commands_realtime_callbacks(sandbox_context):
    """Test real-time output callbacks"""
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        metadata={"test_case": "test_commands_realtime_callbacks"},
    ))

    stdout_lines = []
    stderr_lines = []

    # Execute command with real-time callbacks
    result = sbx.commands.run(
        'echo hello; echo error >&2',
        on_stdout=lambda data: stdout_lines.append(data),
        on_stderr=lambda data: stderr_lines.append(data)
    )

    assert len(stdout_lines) > 0
    assert "hello" in "".join(stdout_lines)
    assert len(stderr_lines) > 0
    assert "error" in "".join(stderr_lines)
