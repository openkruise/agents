import os

import pytest
import requests
from e2b_code_interpreter import Sandbox

import logging

logger = logging.getLogger(__name__)


def test_read_write_file(sandbox_context, config):
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=30,
        metadata={"test_case": "test_read_write_file"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "joke.txt"), "rb") as file:
        # Upload file to the sandbox to absolute path '/home/user/my-file'
        content = file.read()
        sbx.files.write("/home/user/my-file", content)
    file_content = sbx.files.read("/home/user/my-file")
    assert file_content == content.decode('utf-8')


def test_read_write_multifile(sandbox_context, config):
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=30,
        metadata={"test_case": "test_read_write_multifile"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    sbx.files.write_files([
        {"path": "/path/to/a", "data": "file a content"},
        {"path": "/path/to/b", "data": "file b content"}
    ])
    file_content = sbx.files.read("/path/to/a")
    assert file_content == "file a content"

    file_content = sbx.files.read("/path/to/b")
    assert file_content == "file b content"


@pytest.mark.skip(reason="not yet supported")
def test_upload_with_signed_url(sandbox_context, config):
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=3000,
        metadata={"test_case": "test_upload_with_signed_url"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    signed_url = sbx.upload_url(path="demo.txt", user="user", use_signature_expiration=10_000)
    logger.debug("signed_url: %s", signed_url)
    resp = requests.post(signed_url, files={"file": ("demo.txt", "uploaded content")})
    logger.debug("upload response: %s", resp)
    assert resp.status_code == 200, f"Upload failed with status {resp.status_code}: {resp.text}"
    file_content = sbx.files.read("demo.txt")
    assert file_content == "uploaded content"


@pytest.mark.skip(reason="not yet supported")
def test_download_with_signed_url(sandbox_context, config):
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=3000,
        metadata={"test_case": "test_download_with_signed_url"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    signed_url = sbx.download_url(path="demo.txt", user="user", use_signature_expiration=10_000)
    logger.debug("signed_url: %s", signed_url)
