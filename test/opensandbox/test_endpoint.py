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

"""Control-plane connection discovery without contacting execution services."""

import json
import time
from urllib.parse import urlsplit
import uuid

from opensandbox.api.lifecycle.api.sandboxes import (
    get_sandboxes_sandbox_id_endpoints_port as get_endpoint,
    post_sandboxes_sandbox_id_pause as pause_sandbox,
    post_sandboxes_sandbox_id_resume as resume_sandbox,
)
from opensandbox.api.lifecycle.models import Endpoint

from test_sandbox import kill, submit, wait_for_state


def endpoint(client, sandbox_id, port=44772, expected=200):
    response = get_endpoint.sync_detailed(sandbox_id, port, client=client)
    assert response.status_code == expected, (response.status_code, response.content)
    if expected == 200:
        assert isinstance(response.parsed, Endpoint), response.parsed
        return response.parsed.to_dict()
    return json.loads(response.content)


def assert_discovery(client, created, config, kubectl):
    namespace, name = created["metadata"]["opensandbox.kruise.io/sandbox-resource"].split("/", 1)
    box = kubectl("get", "sandbox", name, "-n", namespace)
    ip = box["status"]["podInfo"]["podIP"]
    assert ip, "endpoint discovery requires an assigned Pod IP"
    annotations = box["metadata"].get("annotations", {})
    token = (annotations.get("agents.kruise.io/runtime-access-token")
             or annotations.get("e2b.agents.kruise.io/envd-access-token"))
    assert token, "the configured runtime must have an execution credential"
    mode = config["access_mode"]
    assert mode in ("ingress", "direct"), mode
    if mode == "ingress":
        origin = urlsplit(config["gateway_url"])
        assert origin.scheme in ("http", "https") and origin.netloc, \
            "set OPENSANDBOX_GATEWAY_URL to the Manager's configured ingress origin"

    for requested_port, actual_port in ((44772, 44772), (18080, 44772), (8080, 8080)):
        result = endpoint(client, created["id"], requested_port)
        headers = result["headers"]
        if requested_port in (44772, 18080):
            assert headers.get("X-EXECD-ACCESS-TOKEN") == token
        else:
            assert "X-EXECD-ACCESS-TOKEN" not in headers
        if mode == "direct":
            host = f"[{ip}]" if ":" in ip else ip
            assert result["endpoint"] == f"{host}:{actual_port}"
            assert not {"e2b-sandbox-id", "e2b-sandbox-port", "X-Access-Token",
                        "e2b-traffic-access-token"}.intersection(headers)
        else:
            assert result["endpoint"] == origin.netloc
            assert headers["e2b-sandbox-id"] == created["id"]
            assert headers["e2b-sandbox-port"] == str(actual_port)
            assert headers.get("X-Access-Token") == token
            if annotations.get("security.agents.kruise.io/enable-jwt-auth") == "true":
                assert headers.get("e2b-traffic-access-token")
            else:
                assert "e2b-traffic-access-token" not in headers


def test_endpoint_discovery(os_create, os_client, os_config, os_kubectl):
    """Discover execution aliases and an application port using the deployment's access mode."""
    created = os_create()
    assert_discovery(os_client, created, os_config, os_kubectl)
    for port in (0, 65536):
        error = endpoint(os_client, created["id"], port, expected=400)
        assert error["code"] == "INVALID_REQUEST"


def test_endpoint_owner_isolation(os_create, os_client, os_foreign_client, os_config):
    """A different valid key sees the same endpoint error as a missing delivery."""
    created = os_create()
    endpoint(os_client, created["id"])
    hidden = endpoint(os_foreign_client, created["id"], expected=404)
    missing_id = f"{os_config['namespace']}--missing-{uuid.uuid4().hex}"
    missing = endpoint(os_foreign_client, missing_id, expected=404)
    assert hidden == missing == {"code": "NOT_FOUND", "message": "sandbox not found"}


def test_endpoint_after_pause_resume_and_delete(os_create, os_client, os_config, os_kubectl):
    """Reconnect by rediscovering a resumed endpoint; paused and deleted deliveries stay unavailable."""
    created = os_create({"timeout": 6000})
    assert_discovery(os_client, created, os_config, os_kubectl)
    submit(os_client, created["id"], pause_sandbox)
    wait_for_state(os_client, created["id"], "Paused", os_kubectl)
    assert endpoint(os_client, created["id"], expected=409) == {
        "code": "CONFLICT", "message": "sandbox is not running",
    }
    submit(os_client, created["id"], resume_sandbox)
    wait_for_state(os_client, created["id"], "Running", os_kubectl)
    assert_discovery(os_client, created, os_config, os_kubectl)
    kill(os_client, created["id"])
    # Description can hide a terminating delivery before endpoint lookup does.
    deadline = time.monotonic() + 60
    while True:
        response = get_endpoint.sync_detailed(created["id"], 44772, client=os_client)
        error = json.loads(response.content)
        if response.status_code == 404:
            assert error == {"code": "NOT_FOUND", "message": "sandbox not found"}
            break
        assert response.status_code == 409, (response.status_code, error)
        assert error == {"code": "CONFLICT", "message": "sandbox is not running"}
        assert time.monotonic() < deadline, f"sandbox {created['id']} endpoint remains after deletion"
        time.sleep(1)
