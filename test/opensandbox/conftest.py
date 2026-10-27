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

from contextlib import ExitStack
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


@pytest.fixture(scope="session")
def os_config(record_testsuite_property):
    sdk_version = version("opensandbox")
    print(f"OpenSandbox SDK version: {sdk_version}")
    record_testsuite_property("opensandbox_sdk_version", sdk_version)
    url = os.environ.get("OPENSANDBOX_API_URL", "http://127.0.0.1:18080/v1").rstrip("/")
    return {
        "url": url,
        "native_url": os.environ.get("E2B_API_URL", url.removesuffix("/v1") + "/kruise/api").rstrip("/"),
        "context": os.environ.get("OPENSANDBOX_KUBE_CONTEXT", ""),
        "namespace": os.environ.get("TEST_NAMESPACE", "default"),
        "auth_enabled": os.environ.get("OPENSANDBOX_AUTH_ENABLED", "true").lower() == "true",
        "access_mode": os.environ.get("OPENSANDBOX_ACCESS_MODE", "ingress"),
        "gateway_url": os.environ.get("OPENSANDBOX_GATEWAY_URL", ""),
        "quota_profile": os.environ.get("OPENSANDBOX_QUOTA_PROFILE", os.environ.get("QUOTA_E2E_PROFILE", "none")),
        "key_sync_timeout": int(os.environ.get("OPENSANDBOX_KEY_SYNC_TIMEOUT", "600")),
        "pool": os.environ.get("OPENSANDBOX_POOL_REF", "code-interpreter"),
        "empty_pool": os.environ.get("OPENSANDBOX_EMPTY_POOL_REF", "code-interpreter-0"),
        "image": os.environ.get("OPENSANDBOX_IMAGE", os.environ.get(
            "CODE_INTERPRETER_IMG", "registry-ap-southeast-1.ack.aliyuncs.com/acs/code-interpreter:v1.6")),
    }


@pytest.fixture(scope="session")
def os_kubectl_text(os_config):
    """Read raw metrics/redis-cli output with the same context and time bounds."""
    def read(*args, timeout=30):
        assert timeout > 0, "kubectl read exceeded its deadline"
        command = ["kubectl"]
        if os_config["context"]:
            command += ["--context", os_config["context"]]
        result = subprocess.run(
            [*command, f"--request-timeout={min(15, timeout):g}s", *args],
            capture_output=True, text=True, timeout=timeout,
        )
        assert result.returncode == 0, f"kubectl {' '.join(args)}: {result.stderr}"
        return result.stdout

    return read


@pytest.fixture(scope="session")
def os_kubectl(os_kubectl_text):
    def read(*args, **kwargs):
        return json.loads(os_kubectl_text(*args, "-o", "json", **kwargs))

    return read


@pytest.fixture
def os_client_factory(os_config):
    with ExitStack() as stack:
        def client(key):
            return stack.enter_context(AuthenticatedClient(
                base_url=os_config["url"], token=key, prefix="",
                auth_header_name="OPEN-SANDBOX-API-KEY",
                headers={"X-Request-ID": uuid.uuid4().hex}, timeout=httpx.Timeout(180, connect=5),
                raise_on_unexpected_status=False,
            ))

        yield client


@pytest.fixture
def os_client(os_client_factory, os_config):
    key = os.environ.get("OPENSANDBOX_API_KEY") or os.environ.get("E2B_API_KEY")
    if not os_config["auth_enabled"]:
        key = key or "anonymous-e2e"
    assert key, "Set OPENSANDBOX_API_KEY or E2B_API_KEY before running the suite"
    return os_client_factory(key)


@pytest.fixture
def os_key_factory(os_client, os_client_factory, os_config):
    """Prepare native API keys; behavior under test uses the OpenSandbox routes."""
    created_ids = []
    headers = {"X-API-Key": os_client.token, "X-Request-ID": uuid.uuid4().hex}
    with requests.Session() as http:
        def create(quota=None, namespace=None):
            if not os_config["auth_enabled"]:
                pytest.skip("API-key isolation requires sandbox-manager authentication")
            payload = {"name": f"opensandbox-e2e-{uuid.uuid4().hex}",
                       "teamName": namespace or os_config["namespace"]}
            if quota is not None:
                payload["quota"] = quota
            response = http.post(f"{os_config['native_url']}/api-keys", json=payload,
                                 headers=headers, timeout=(5, 30))
            assert response.status_code == 201, f"prepare API key: HTTP {response.status_code}"
            body = response.json()
            created_ids.append(body["id"])
            assert body.get("key"), "API-key creation did not return a credential"
            sdk = os_client_factory(body["key"])
            deadline = time.monotonic() + os_config["key_sync_timeout"]
            while True:
                ready = sdk.get_httpx_client().get("/sandboxes")
                if ready.status_code == 200:
                    return body
                assert ready.status_code == 401, f"prepare API key: HTTP {ready.status_code}"
                assert time.monotonic() < deadline, "API-key cache did not observe the new key"
                time.sleep(1)

        try:
            yield create
        finally:
            errors = []
            for key_id in created_ids:
                try:
                    response = http.delete(f"{os_config['native_url']}/api-keys/{key_id}",
                                           headers=headers, timeout=(5, 30))
                    if response.status_code not in (204, 404):
                        errors.append(f"cleanup API key {key_id}: HTTP {response.status_code}")
                except requests.RequestException as exc:
                    errors.append(f"cleanup API key {key_id}: {type(exc).__name__}")
            assert not errors, "; ".join(errors)


@pytest.fixture
def os_foreign_client(os_key_factory, os_client_factory):
    return os_client_factory(os_key_factory()["key"])


@pytest.fixture
def os_create(os_client, os_config, os_kubectl, os_key_factory):
    # Depend on os_key_factory so delivery cleanup precedes API-key deletion.
    deliveries = {}

    with requests.Session() as http:
        def create(overrides=None, *, client=None, namespace=None, expected_status=202):
            sdk = client if client is not None else os_client
            namespace = namespace or os_config["namespace"]
            delivery = deliveries.setdefault((namespace, id(sdk)), {
                "client": sdk, "namespace": namespace,
                "marker": f"opensandbox-{uuid.uuid4().hex}", "ids": set(),
            })
            payload = {"extensions": {"poolRef": os_config["pool"]}, "timeout": 120,
                       **(overrides or {})}
            payload["metadata"] = {**payload.get("metadata", {}), "test_case": delivery["marker"]}
            response = post_sandboxes.sync_detailed(
                client=sdk, body=CreateSandboxRequest.from_dict(payload),
            )
            statuses = expected_status if isinstance(expected_status, tuple) else (expected_status,)
            assert response.status_code in statuses, f"create: {response.status_code}: {response.content!r}"
            if response.status_code != 202:
                return response
            assert isinstance(response.parsed, CreateSandboxResponse), response.parsed
            body = response.parsed.to_dict()
            assert isinstance(body.get("id"), str) and body["id"], body
            delivery["ids"].add(body["id"])
            return response if isinstance(expected_status, tuple) else body

        try:
            yield create
        finally:
            # Discover marked objects too, so a failed/lost create response cannot leak them.
            errors = []
            for delivery in deliveries.values():
                sdk, namespace, marker = delivery["client"], delivery["namespace"], delivery["marker"]

                def owned_sandboxes():
                    return [box for box in os_kubectl("get", "sandboxes", "-n", namespace)["items"]
                            if box["metadata"].get("annotations", {}).get("test_case") == marker]

                try:
                    boxes = owned_sandboxes()
                except (AssertionError, subprocess.TimeoutExpired, ValueError, OSError) as exc:
                    # A discovery failure must not prevent deleting returned IDs
                    # or cleaning another owner's deliveries.
                    errors.append(f"discover deliveries in {namespace}: {type(exc).__name__}")
                    boxes = []
                for box in boxes:
                    meta = box["metadata"]
                    delivery["ids"].add(meta.get("labels", {}).get("agents.kruise.io/sandbox-id")
                                        or f"{meta['namespace']}--{meta['name']}")
                print("OpenSandbox cleanup:", json.dumps([
                    {"name": box["metadata"]["name"], "phase": box.get("status", {}).get("phase"),
                     "podIP": box.get("status", {}).get("podInfo", {}).get("podIP")}
                    for box in boxes]))
                for sandbox_id in delivery["ids"]:
                    try:
                        response = delete_sandboxes_sandbox_id.sync_detailed(sandbox_id, client=sdk)
                        if response.status_code in (204, 404):
                            continue
                        errors.append(f"OpenSandbox delete {sandbox_id}: HTTP {response.status_code}")
                    except (httpx.HTTPError, UnexpectedStatus) as exc:
                        errors.append(f"OpenSandbox delete {sandbox_id}: {type(exc).__name__}")
                    # Keep original failures visible while native deletion prevents leaks.
                    try:
                        response = http.delete(
                            f"{os_config['native_url']}/sandboxes/{sandbox_id}",
                            headers={"X-API-Key": sdk.token}, timeout=(5, 30),
                        )
                        if response.status_code not in (204, 404):
                            errors.append(f"delete {sandbox_id}: HTTP {response.status_code}")
                    except requests.RequestException as exc:
                        errors.append(f"delete {sandbox_id}: {type(exc).__name__}")
                try:
                    deadline = time.monotonic() + 60
                    remaining = owned_sandboxes()
                    while remaining and time.monotonic() < deadline:
                        time.sleep(1)
                        remaining = owned_sandboxes()
                    if remaining:
                        errors.append(f"leaked sandboxes: {[b['metadata']['name'] for b in remaining]}")
                except (AssertionError, subprocess.TimeoutExpired, ValueError, OSError) as exc:
                    errors.append(f"verify cleanup in {namespace}: {type(exc).__name__}")
            assert not errors, "; ".join(errors)
