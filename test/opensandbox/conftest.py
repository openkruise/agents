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

"""Official SDK and Kubernetes fixtures for OpenSandbox control-plane E2E."""

from importlib.metadata import version
import json
import os
import subprocess
import time
import uuid

import httpx
from opensandbox.api.lifecycle import AuthenticatedClient
from opensandbox.api.lifecycle.api.sandboxes import delete_sandboxes_sandbox_id, post_sandboxes
from opensandbox.api.lifecycle.errors import UnexpectedStatus
from opensandbox.api.lifecycle.models import CreateSandboxRequest, CreateSandboxResponse
import pytest
import requests


import ssl
from datetime import timedelta
from pathlib import Path
from urllib.parse import urlsplit
from opensandbox.config import ConnectionConfigSync
from opensandbox.sync import SandboxSync


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
def os_client(os_config):
    key = os.environ.get("OPENSANDBOX_API_KEY") or os.environ.get("E2B_API_KEY")
    assert key, "Set OPENSANDBOX_API_KEY or E2B_API_KEY before running the suite"
    with AuthenticatedClient(
        base_url=os_config["url"], token=key, prefix="",
        auth_header_name="OPEN-SANDBOX-API-KEY",
        headers={"X-Request-ID": uuid.uuid4().hex}, timeout=httpx.Timeout(180, connect=5),
        raise_on_unexpected_status=True,
    ) as sdk:
        yield sdk


@pytest.fixture
def os_create(os_client, os_config, os_kubectl):
    marker = f"opensandbox-{uuid.uuid4().hex}"
    created_ids = set()
    namespace = os_config["namespace"]

    def owned_sandboxes():
        return [box for box in os_kubectl("get", "sandboxes", "-n", namespace)["items"]
                if box["metadata"].get("annotations", {}).get("test_case") == marker]

    with requests.Session() as http:
        def create(overrides=None):
            payload = {"extensions": {"poolRef": os_config["pool"]}, "timeout": 120,
                       **(overrides or {})}
            payload["metadata"] = {**payload.get("metadata", {}), "test_case": marker}
            response = post_sandboxes.sync_detailed(
                client=os_client, body=CreateSandboxRequest.from_dict(payload),
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
                    response = delete_sandboxes_sandbox_id.sync_detailed(
                        sandbox_id, client=os_client,
                    )
                    if response.status_code in (204, 404):
                        continue
                    errors.append(f"OpenSandbox delete {sandbox_id}: HTTP {response.status_code}")
                except (httpx.HTTPError, UnexpectedStatus) as exc:
                    errors.append(f"OpenSandbox delete {sandbox_id}: {type(exc).__name__}")
                # Keep cleanup failures visible while using native deletion to avoid leaks.
                try:
                    response = http.delete(
                        f"{native_url}/sandboxes/{sandbox_id}",
                        headers={"X-API-Key": os_client.token}, timeout=(5, 30),
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


class RecordingTransport(httpx.BaseTransport):
    """Observe real SDK requests without changing their destination or headers."""

    def __init__(self):
        self.inner = httpx.HTTPTransport()
        self.created = []
        self.execution_requests = []

    def handle_request(self, request):
        response = self.inner.handle_request(request)
        if request.method == "POST" and request.url.path == "/v1/sandboxes" and response.status_code == 202:
            response.read()
            self.created.append(response.json()["id"])
        if request.url.path in ("/command", "/files/upload", "/files/download"):
            self.execution_requests.append((request.url.host, request.url.port, dict(request.headers)))
        return response

    def close(self):
        self.inner.close()


@pytest.fixture(scope="session")
def kube():
    account = Path("/var/run/secrets/kubernetes.io/serviceaccount")
    context = ssl.create_default_context(cafile=str(account / "ca.crt"))
    with httpx.Client(
        base_url="https://kubernetes.default.svc",
        verify=context,
        headers={"Authorization": f"Bearer {(account / 'token').read_text().strip()}"},
        timeout=20,
    ) as client:
        yield client


def sandbox_resource(kube, sandbox_id):
    namespace = os.environ.get("TEST_NAMESPACE", "default")
    response = kube.get(f"/apis/agents.kruise.io/v1alpha1/namespaces/{namespace}/sandboxes")
    response.raise_for_status()
    for item in response.json()["items"]:
        meta = item["metadata"]
        public_id = meta.get("labels", {}).get("agents.kruise.io/sandbox-id")
        if public_id == sandbox_id or f"{meta['namespace']}--{meta['name']}" == sandbox_id:
            return item
    raise AssertionError(f"Sandbox {sandbox_id} was not found")


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(item, call):
    outcome = yield
    setattr(item, f"rep_{call.when}", outcome.get_result())


@pytest.fixture
def sandbox(request, kube):
    transport = RecordingTransport()
    api = urlsplit(os.environ.get("OPENSANDBOX_API_URL", "http://sandbox-manager.sandbox-system.svc:8080"))
    config = ConnectionConfigSync(
        domain=api.netloc,
        protocol=api.scheme,
        api_key=os.environ.get("OPENSANDBOX_API_KEY", "some-api-key"),
        headers={"x-request-id": uuid.uuid4().hex},
        transport=transport,
        disable_metrics=True,
    )
    instance = None
    try:
        instance = SandboxSync.create(
            os.environ.get("CODE_INTERPRETER_IMG", "registry-ap-southeast-1.ack.aliyuncs.com/acs/code-interpreter:v1.6"),
            resource={"cpu": "100m", "memory": "256Mi"},
            timeout=timedelta(minutes=3),
            ready_timeout=timedelta(seconds=120),
            metadata={"test_case": request.node.name},
            connection_config=config,
        )
        # Discovery and health are setup, not additional business test matrices.
        endpoint = instance.get_endpoint(44772)
        egress = instance.get_endpoint(18080)
        assert endpoint.endpoint == egress.endpoint
        info = sandbox_resource(kube, instance.id)
        ip = info["status"]["podInfo"]["podIP"]
        target = urlsplit(f"{api.scheme}://{endpoint.endpoint}")
        mode = os.environ.get("OPENSANDBOX_ACCESS_MODE", "ingress")
        if mode == "direct":
            assert api.scheme == "http"
            assert target.hostname == ip and target.port == 44772
            assert "e2b-sandbox-id" not in endpoint.headers
        else:
            gateway = urlsplit(os.environ["OPENSANDBOX_GATEWAY_URL"])
            assert target.netloc == gateway.netloc
            assert endpoint.headers["e2b-sandbox-id"] == instance.id
            assert endpoint.headers["e2b-sandbox-port"] == "44772"
        yield instance
        assert transport.execution_requests, "SDK did not send an execution request"
        for host, port, headers in transport.execution_requests:
            assert host == target.hostname
            assert port == (target.port or (443 if api.scheme == "https" else 80))
            assert headers.get("x-execd-access-token"), "SDK lost endpoint credentials"
    finally:
        if instance is not None:
            instance.close()
        # Keep fixture cleanup independent of command/file protocol assertions.
        # Kubernetes cleanup also handles a failed SDK create/setup response.
        failed = getattr(request.node, "rep_call", None)
        for sandbox_id in transport.created:
            info = sandbox_resource(kube, sandbox_id)
            namespace, name = info["metadata"]["namespace"], info["metadata"]["name"]
            if failed is None or failed.failed:
                print(f"Sandbox diagnostics: {namespace}/{name} {json.dumps(info.get('status', {}))}")
                logs = kube.get(f"/api/v1/namespaces/{namespace}/pods/{name}/log", params={"container": "opensandbox-runtime", "tailLines": "100"})
                print(logs.text)
            response = kube.delete(f"/apis/agents.kruise.io/v1alpha1/namespaces/{namespace}/sandboxes/{name}")
            assert response.status_code in (200, 202, 404), response.status_code
        transport.close()
