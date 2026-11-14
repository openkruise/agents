"""File scenarios from test/e2b/test_filesystem.py plus native-aligned attribute,
path-resolution and byte-Range assertions.

Mode-omitted/null and overwrite-preserves-mode are wire-level contracts: the
OpenSandbox SDK always carries WriteEntry.mode (default 755), so those cases are
covered by the targeted Go upload tests rather than the high-level SDK here.
Duplicate-target and alias determination is left to the native upload request.
"""
import shlex
from pathlib import Path

import pytest
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.filesystem import WriteEntry


def stdout(result):
    return "".join(message.text for message in result.logs.stdout)


def stat_field(sandbox, path, fmt):
    result = sandbox.commands.run(f"stat -c {shlex.quote(fmt)} {shlex.quote(path)}")
    assert result.error is None, f"stat {path}: {result.error}"
    return stdout(result).strip()


def test_read_write_file(sandbox):
    content = Path(__file__).with_name("joke.txt").read_bytes()
    sandbox.files.write_file("/home/user/my-file", content)
    assert sandbox.files.read_file("/home/user/my-file") == content.decode("utf-8")


def test_read_write_multifile(sandbox):
    sandbox.files.write_files([
        WriteEntry(path="/path/to/a", data="file a content"),
        WriteEntry(path="/path/to/b", data="file b content"),
    ])
    assert sandbox.files.read_file("/path/to/a") == "file a content"
    assert sandbox.files.read_file("/path/to/b") == "file b content"


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
