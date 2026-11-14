"""Default file scenarios, one per E2B equivalent in test/e2b/test_filesystem.py.
The runner executes this file unchanged on both the ingress and the direct Pod
IP path.

Attribute, path-resolution and byte-Range assertions live in `test_contract.py`.
Mode-omitted/null and overwrite-preserves-mode are wire-level contracts: the
OpenSandbox SDK always carries WriteEntry.mode (default 755), so those cases are
covered by the targeted Go upload tests rather than the high-level SDK here.
Duplicate-target and alias determination is left to the native upload request.
"""
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
