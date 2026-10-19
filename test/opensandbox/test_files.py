"""Equivalent scenarios from test/e2b/test_filesystem.py."""
from pathlib import Path

from opensandbox.models.filesystem import WriteEntry


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
