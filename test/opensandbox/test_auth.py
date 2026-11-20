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

"""Shared E2B API-key identity on the implemented OpenSandbox interfaces."""

from datetime import datetime, timedelta, timezone
from urllib.parse import urlencode
import uuid

from opensandbox.api.lifecycle.api.sandboxes import get_sandboxes as list_sandboxes
from opensandbox.api.lifecycle.api.sandboxes import get_sandboxes_sandbox_id as get_sandbox
import pytest


def sandbox_requests(sandbox_id):
    return (
        ("GET", f"/sandboxes/{sandbox_id}", None),
        ("DELETE", f"/sandboxes/{sandbox_id}", None),
        ("POST", f"/sandboxes/{sandbox_id}/pause", None),
        ("POST", f"/sandboxes/{sandbox_id}/resume", None),
        ("POST", f"/sandboxes/{sandbox_id}/renew-expiration", {
            "expiresAt": (datetime.now(timezone.utc) + timedelta(hours=1)).isoformat(),
        }),
        ("GET", f"/sandboxes/{sandbox_id}/endpoints/44772", None),
    )


@pytest.mark.parametrize("missing_key", [True, False], ids=["missing-key", "invalid-key"])
def test_authentication_required(os_client_factory, os_config, os_kubectl, missing_key):
    """An unauthenticated request cannot allocate or read a delivery on any route."""
    if not os_config["auth_enabled"]:
        pytest.skip("requires sandbox-manager authentication")
    client = os_client_factory(f"invalid-{uuid.uuid4().hex}").get_httpx_client()
    if missing_key:
        del client.headers["OPEN-SANDBOX-API-KEY"]
    marker = f"unauthenticated-{uuid.uuid4().hex}"
    operations = (
        ("POST", "/sandboxes", {"extensions": {"poolRef": os_config["pool"]},
                                 "metadata": {"test_case": marker}, "timeout": 120}),
        ("GET", "/sandboxes", None),
        *sandbox_requests(f"{os_config['namespace']}--missing-{uuid.uuid4().hex}"),
    )
    for method, path, body in operations:
        response = client.request(method, path, json=body)
        assert response.status_code == 401, (method, path, response.status_code)
        assert response.json() == {"code": "UNAUTHORIZED", "message": "Invalid API Key"}
    boxes = os_kubectl("get", "sandboxes", "-n", os_config["namespace"])["items"]
    assert not [box["metadata"]["name"] for box in boxes
                if box["metadata"].get("annotations", {}).get("test_case") == marker]


def test_namespace_and_owner_isolation(os_create, os_key_factory, os_client_factory,
                                       os_client, os_config, os_kubectl):
    """Same-namespace keys and the admin caller retain independent ownership."""
    key = os_key_factory()
    owner = os_client_factory(key["key"])
    other = os_client_factory(os_key_factory()["key"])
    created = os_create(client=owner)
    namespace, name = created["metadata"]["opensandbox.kruise.io/sandbox-resource"].split("/", 1)
    assert namespace == os_config["namespace"]
    box = os_kubectl("get", "sandbox", name, "-n", namespace)
    assert box["metadata"]["annotations"]["agents.kruise.io/owner"] == key["id"]
    own = get_sandbox.sync_detailed(created["id"], client=owner)
    assert own.status_code == 200, (own.status_code, own.content)

    missing_id = f"{namespace}--missing-{uuid.uuid4().hex}"
    for outsider in (other, os_client):
        http = outsider.get_httpx_client()
        for hidden, missing in zip(sandbox_requests(created["id"]), sandbox_requests(missing_id)):
            method, path, body = hidden
            actual = http.request(method, path, json=body)
            absent = http.request(missing[0], missing[1], json=missing[2])
            assert actual.status_code == absent.status_code == 404, (method, actual.status_code, absent.status_code)
            assert actual.json() == absent.json() == {"code": "NOT_FOUND", "message": "sandbox not found"}
        response = list_sandboxes.sync_detailed(
            client=outsider, metadata=urlencode({"test_case": created["metadata"]["test_case"]}),
        )
        assert response.status_code == 200, (response.status_code, response.content)
        assert response.parsed.to_dict()["items"] == []

    # Failed foreign mutations must leave the owned delivery live and unchanged.
    after = get_sandbox.sync_detailed(created["id"], client=owner)
    assert after.status_code == 200, (after.status_code, after.content)
    assert after.parsed.to_dict() == own.parsed.to_dict()


def test_auth_disabled_uses_anonymous_owner(os_create, os_client_factory, os_config, os_kubectl):
    """Match E2B's auth-disabled profile: different headers share the anonymous owner."""
    if os_config["auth_enabled"]:
        pytest.skip("requires --e2b-enable-auth=false and OPENSANDBOX_AUTH_ENABLED=false")
    anonymous = os_client_factory("")
    del anonymous.get_httpx_client().headers["OPEN-SANDBOX-API-KEY"]
    created = os_create(client=anonymous)
    other = os_client_factory(f"ignored-{uuid.uuid4().hex}")
    info = get_sandbox.sync_detailed(created["id"], client=other)
    assert info.status_code == 200, (info.status_code, info.content)
    assert info.parsed.to_dict()["id"] == created["id"]
    namespace, name = created["metadata"]["opensandbox.kruise.io/sandbox-resource"].split("/", 1)
    box = os_kubectl("get", "sandbox", name, "-n", namespace)
    # Both protocols use keys.AdminKeyID for the canonical anonymous caller.
    assert box["metadata"]["annotations"]["agents.kruise.io/owner"] == "550e8400-e29b-41d4-a716-446655440000"
