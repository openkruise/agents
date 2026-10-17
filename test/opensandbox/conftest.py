# Copyright 2026 The OpenKruise Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Official SDK and Kubernetes fixtures for OpenSandbox create compatibility."""

from importlib.metadata import version
import json
import os
import subprocess
import time
import uuid

import httpx
from opensandbox.api.lifecycle import AuthenticatedClient
from opensandbox.api.lifecycle.api.sandboxes import post_sandboxes
from opensandbox.api.lifecycle.models import CreateSandboxRequest, CreateSandboxResponse
import pytest
import requests


@pytest.fixture(scope="session")
def os_config(record_testsuite_property):
    sdk_version = version("opensandbox")
    print(f"OpenSandbox SDK version: {sdk_version}")
    record_testsuite_property("opensandbox_sdk_version", sdk_version)
    return {
        "url": os.environ.get("OPENSANDBOX_API_URL", "http://127.0.0.1:18080/v1").rstrip("/"),
        "context": os.environ.get("OPENSANDBOX_KUBE_CONTEXT", ""),
        "namespace": os.environ.get("TEST_NAMESPACE", "default"),
        "pool": os.environ.get("OPENSANDBOX_POOL_REF", "code-interpreter"),
        "empty_pool": os.environ.get("OPENSANDBOX_EMPTY_POOL_REF", "code-interpreter-0"),
        "image": os.environ.get("OPENSANDBOX_IMAGE", os.environ.get(
            "CODE_INTERPRETER_IMG", "registry-ap-southeast-1.ack.aliyuncs.com/acs/code-interpreter:v1.6")),
    }


@pytest.fixture(scope="session")
def os_kubectl(os_config):
    def read(*args):
        command = ["kubectl"]
        if os_config["context"]:
            command += ["--context", os_config["context"]]
        result = subprocess.run(
            [*command, "--request-timeout=15s", *args, "-o", "json"],
            capture_output=True, text=True, timeout=30,
        )
        assert result.returncode == 0, f"kubectl {' '.join(args)}: {result.stderr}"
        return json.loads(result.stdout)

    return read


@pytest.fixture
def os_create(os_config, os_kubectl):
    key = os.environ.get("OPENSANDBOX_API_KEY") or os.environ.get("E2B_API_KEY")
    assert key, "Set OPENSANDBOX_API_KEY or E2B_API_KEY before running the suite"
    marker = f"opensandbox-create-{uuid.uuid4().hex}"
    created_ids = set()
    namespace = os_config["namespace"]

    def owned_sandboxes():
        return [box for box in os_kubectl("get", "sandboxes", "-n", namespace)["items"]
                if box["metadata"].get("annotations", {}).get("test_case") == marker]

    with AuthenticatedClient(
        base_url=os_config["url"], token=key, prefix="",
        auth_header_name="OPEN-SANDBOX-API-KEY",
        headers={"X-Request-ID": uuid.uuid4().hex}, timeout=httpx.Timeout(180, connect=5),
        raise_on_unexpected_status=True,
    ) as sdk, requests.Session() as http:
        def create(overrides=None):
            payload = {"extensions": {"poolRef": os_config["pool"]}, "timeout": 120,
                       **(overrides or {})}
            payload["metadata"] = {**payload.get("metadata", {}), "test_case": marker}
            response = post_sandboxes.sync_detailed(
                client=sdk, body=CreateSandboxRequest.from_dict(payload),
            )
            assert response.status_code == 202, f"create: {response.status_code}: {response.content!r}"
            assert isinstance(response.parsed, CreateSandboxResponse), response.parsed
            body = response.parsed.to_dict()
            assert isinstance(body.get("id"), str) and body["id"], body
            created_ids.add(body["id"])
            return body

        try:
            yield create
        finally:
            # Discover marked objects too, so a failed/lost create response cannot leak them.
            boxes = owned_sandboxes()
            for box in boxes:
                meta = box["metadata"]
                created_ids.add(meta.get("labels", {}).get("agents.kruise.io/sandbox-id")
                                or f"{meta['namespace']}--{meta['name']}")
            print("OpenSandbox cleanup:", json.dumps([
                {"name": box["metadata"]["name"], "phase": box.get("status", {}).get("phase"),
                 "podIP": box.get("status", {}).get("podInfo", {}).get("podIP")}
                for box in boxes]))
            errors = []
            native_url = os_config["url"].removesuffix("/v1") + "/kruise/api"
            for sandbox_id in created_ids:
                try:
                    response = http.delete(
                        f"{native_url}/sandboxes/{sandbox_id}",
                        headers={"X-API-Key": key}, timeout=(5, 30),
                    )
                    if response.status_code not in (204, 404):
                        errors.append(f"delete {sandbox_id}: HTTP {response.status_code}")
                except requests.RequestException as exc:
                    errors.append(f"delete {sandbox_id}: {type(exc).__name__}")
            deadline = time.monotonic() + 60
            remaining = owned_sandboxes()
            while remaining and time.monotonic() < deadline:
                time.sleep(1)
                remaining = owned_sandboxes()
            assert not remaining, f"leaked sandboxes: {[b['metadata']['name'] for b in remaining]}"
            assert not errors, "; ".join(errors)
