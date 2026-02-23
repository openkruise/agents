import os

import pytest
import requests
from e2b_code_interpreter import Sandbox


def test_read_write_file(sandbox_context):
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        metadata={"test_case": "test_read_write_file"},
    ))
    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "joke.txt"), "rb") as file:
        # Upload file to the sandbox to absolute path '/home/user/my-file'
        content = file.read()
        sbx.files.write("/home/user/my-file", content)
    file_content = sbx.files.read("/home/user/my-file")
    assert file_content == content.decode('utf-8')


def test_read_write_multifile(sandbox_context):
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        metadata={"test_case": "test_read_write_multifile"},
    ))
    sbx.files.write_files([
        {"path": "/path/to/a", "data": "file a content"},
        {"path": "/path/to/b", "data": "file b content"}
    ])
    file_content = sbx.files.read("/path/to/a")
    assert file_content == "file a content"

    file_content = sbx.files.read("/path/to/b")
    assert file_content == "file b content"


def test_upload_with_signed_url(sandbox_context):
    pytest.skip("Not implemented yet")
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=3000,
        metadata={"test_case": "test_upload_with_signed_url"},
    ))
    signed_url = sbx.upload_url(path="demo.txt", user="user", use_signature_expiration=10_000)
    print(signed_url)
    form_data = {"file": "uploaded content"}
    resp = requests.post(signed_url, data=form_data)
    print(resp)
    file_content = sbx.files.read("demo.txt")
    assert file_content == "uploaded content"


def test_download_with_signed_url(sandbox_context):
    pytest.skip("Not implemented yet")
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=3000,
        metadata={"test_case": "test_download_with_signed_url"},
    ))
    signed_url = sbx.download_url(path="demo.txt", user="user", use_signature_expiration=10_000)
    print(signed_url)
