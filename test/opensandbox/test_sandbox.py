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

"""Implemented lifecycle routes, matching test/e2b/test_sandbox.py scenarios."""

from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone
import json
import time
from urllib.parse import urlencode
import uuid

from opensandbox.api.lifecycle.api.sandboxes import (
    delete_sandboxes_sandbox_id as delete_sandbox,
    get_sandboxes as list_sandboxes,
    get_sandboxes_sandbox_id as get_sandbox,
    post_sandboxes_sandbox_id_pause as pause_sandbox,
    post_sandboxes_sandbox_id_renew_expiration as renew_expiration,
    post_sandboxes_sandbox_id_resume as resume_sandbox,
)
from opensandbox.api.lifecycle.models import RenewSandboxExpirationRequest


def describe(client, sandbox_id):
    response = get_sandbox.sync_detailed(sandbox_id, client=client)
    assert response.status_code == 200, (response.status_code, response.content)
    return response.parsed.to_dict()


def listed(client, **query):
    response = list_sandboxes.sync_detailed(client=client, **query)
    assert response.status_code == 200, (response.status_code, response.content)
    return response.parsed.to_dict()["items"]


def wait_for_state(client, sandbox_id, expected, kubectl, timeout=120):
    deadline = time.monotonic() + timeout
    while True:
        info = describe(client, sandbox_id)
        if info["status"]["state"] == expected:
            namespace, name = info["metadata"]["opensandbox.kruise.io/sandbox-resource"].split("/", 1)
            sandbox = kubectl("get", "sandbox", name, "-n", namespace)
            condition = "SandboxPaused" if expected == "Paused" else "Ready"
            # E2B pause/connect waits for these conditions before returning.
            if any(item["type"] == condition and item["status"] == "True"
                   for item in sandbox.get("status", {}).get("conditions", [])):
                return info
        assert time.monotonic() < deadline, (sandbox_id, expected, info)
        time.sleep(1)


def wait_for_deleted(client, sandbox_id, timeout=60):
    deadline = time.monotonic() + timeout
    while True:
        response = get_sandbox.sync_detailed(sandbox_id, client=client)
        if response.status_code == 404:
            return
        assert response.status_code == 200, (response.status_code, response.content)
        assert time.monotonic() < deadline, f"sandbox {sandbox_id} remains visible after deletion/expiry"
        time.sleep(1)


def submit(client, sandbox_id, operation, accepted=(202,), retry_on_pausing=False):
    # Match E2B's connect_sandbox helper only where that helper is used.
    # Other conflicts and the running-start mixed case remain one-shot.
    attempts = 30 if retry_on_pausing else 1
    for attempt in range(attempts):
        response = operation.sync_detailed(sandbox_id, client=client)
        if retry_on_pausing and operation is resume_sandbox and response.status_code == 409:
            error = json.loads(response.content)
            if (error.get("code") == "CONFLICT"
                    and "sandbox is not resumable, reason: SandboxIsPausing" in error.get("message", "")):
                assert attempt < attempts - 1, f"sandbox {sandbox_id} still pausing after {attempts} attempts"
                print("OpenSandbox resume while pausing:", sandbox_id, "attempt", attempt + 1, error)
                time.sleep(2)
                continue
        assert response.status_code in accepted, (response.status_code, response.content)
        if response.status_code == 409:
            print("OpenSandbox lifecycle conflict:", sandbox_id, operation.__name__, response.content)
        return response.status_code


def kill(client, sandbox_id):
    response = delete_sandbox.sync_detailed(sandbox_id, client=client)
    assert response.status_code == 204, (response.status_code, response.content)
    wait_for_deleted(client, sandbox_id)


def concurrent_transitions(client, sandbox_id, operations, retry_resume_while_pausing=False):
    def request(operation):
        return submit(client, sandbox_id, operation, accepted=(202, 409),
                      retry_on_pausing=retry_resume_while_pausing and operation is resume_sandbox)

    with ThreadPoolExecutor(max_workers=len(operations)) as executor:
        # Match E2B's submission order: pause requests precede resume requests.
        futures = [executor.submit(request, operation) for operation in operations]
        statuses = [future.result() for future in futures]
    print("OpenSandbox concurrent results:", [
        (operation.__name__.rsplit("_", 1)[-1], status)
        for operation, status in zip(operations, statuses)
    ])
    assert 202 in statuses, statuses
    return statuses


def test_lifecycle(os_create, os_client):
    """E2B test_lifecycle: creation followed by instance information."""
    created = os_create({"metadata": {"userId": "123"}})
    info = describe(os_client, created["id"])
    assert info["id"] == created["id"]
    assert info["status"]["state"] == "Running"
    assert info["metadata"]["userId"] == "123"
    assert info["createdAt"] == created["createdAt"]
    assert "e2b.agents.kruise.io/sandbox-ip" not in info["metadata"]


def test_list_by_metadata(os_create, os_client):
    """E2B test_list_by_metadata: a unique metadata value selects one instance."""
    user_id = uuid.uuid4().hex
    created = os_create({"metadata": {"userId": user_id}})
    items = listed(os_client, metadata=urlencode({"userId": user_id}))
    assert [item["id"] for item in items] == [created["id"]]


def test_list_metadata_combines_filters(os_create, os_client):
    """Match E2B metadata filtering with more than one live candidate."""
    group = uuid.uuid4().hex
    matching = os_create({"metadata": {"group": group, "role": "selected"}})
    os_create({"metadata": {"group": group, "role": "other"}})
    os_create({"metadata": {"group": uuid.uuid4().hex, "role": "selected"}})
    items = listed(os_client, metadata=urlencode({"group": group, "role": "selected"}))
    assert [item["id"] for item in items] == [matching["id"]]


def test_list_pagination(os_create, os_client):
    """Walk the OpenSandbox pages of the same owned set E2B lists."""
    created = [os_create() for _ in range(3)]
    metadata = urlencode({"test_case": created[0]["metadata"]["test_case"]})
    pages = []
    for page, count in ((1, 2), (2, 1), (3, 0)):
        response = list_sandboxes.sync_detailed(
            client=os_client, metadata=metadata, page=page, page_size=2,
        )
        assert response.status_code == 200, (response.status_code, response.content)
        body = response.parsed.to_dict()
        assert len(body["items"]) == count
        assert body["pagination"] == {
            "page": page, "pageSize": 2, "totalItems": 3, "totalPages": 2,
            "hasNextPage": page == 1,
        }
        pages.extend(item["id"] for item in body["items"])
    assert len(pages) == len(set(pages)) == 3
    assert set(pages) == {item["id"] for item in created}


def test_list_by_state(os_create, os_client, os_kubectl):
    """Exercise E2B's running/paused filters with both states present."""
    running = os_create({"timeout": 6000})
    paused = os_create({"timeout": 6000})
    submit(os_client, paused["id"], pause_sandbox)
    wait_for_state(os_client, paused["id"], "Paused", os_kubectl)
    metadata = urlencode({"test_case": running["metadata"]["test_case"]})
    for states, expected in (
            (["Running"], {running["id"]}),
            (["Paused"], {paused["id"]}),
            (["Running", "Paused"], {running["id"], paused["id"]})):
        items = listed(os_client, state=states, metadata=metadata)
        assert {item["id"] for item in items} == expected
        assert all(item["status"]["state"] in states for item in items)


def test_timeout(os_create, os_client):
    """E2B test_timeout: extend a finite expiration and observe its eventual deletion."""
    control = os_create({"timeout": 1200})
    created = os_create({"timeout": 30})
    original = datetime.fromisoformat(created["expiresAt"].replace("Z", "+00:00"))
    target = original + timedelta(seconds=15)
    response = renew_expiration.sync_detailed(
        created["id"], client=os_client,
        body=RenewSandboxExpirationRequest(expires_at=target),
    )
    assert response.status_code == 200, (response.status_code, response.content)
    renewed = datetime.fromisoformat(response.parsed.to_dict()["expiresAt"].replace("Z", "+00:00"))
    assert renewed == target
    info = describe(os_client, created["id"])
    assert datetime.fromisoformat(info["expiresAt"].replace("Z", "+00:00")) == target
    # Prove extension beyond the old deadline, then actual expiry, as E2B does.
    remaining = (original - datetime.now(timezone.utc)).total_seconds() + 1
    if remaining > 0:
        time.sleep(remaining)
    assert describe(os_client, created["id"])["status"]["state"] == "Running"
    wait_for_deleted(os_client, created["id"])
    assert describe(os_client, control["id"])["status"]["state"] == "Running"


def test_shorten_expiration_and_repeat_deadline(os_create, os_client, os_kubectl):
    """E2B set_timeout: shorten a finite lifetime, accept the same deadline, then reap."""
    created = os_create({"timeout": 1200})
    control = os_create({"timeout": 1200})
    target = (datetime.now(timezone.utc) + timedelta(seconds=20)).replace(microsecond=0)
    assert target < datetime.fromisoformat(created["expiresAt"].replace("Z", "+00:00"))
    namespace, name = created["metadata"]["opensandbox.kruise.io/sandbox-resource"].split("/", 1)
    for _ in range(2):
        response = renew_expiration.sync_detailed(
            created["id"], client=os_client, body=RenewSandboxExpirationRequest(expires_at=target),
        )
        assert response.status_code == 200, (response.status_code, response.content)
        renewed = datetime.fromisoformat(response.parsed.to_dict()["expiresAt"].replace("Z", "+00:00"))
        assert renewed == target
        info = describe(os_client, created["id"])
        assert datetime.fromisoformat(info["expiresAt"].replace("Z", "+00:00")) == target
        box = os_kubectl("get", "sandbox", name, "-n", namespace)
        assert datetime.fromisoformat(box["spec"]["shutdownTime"].replace("Z", "+00:00")) == target
        assert not box["spec"].get("pauseTime")
    wait_for_deleted(os_client, created["id"])
    assert describe(os_client, control["id"])["status"]["state"] == "Running"


def test_pause_kill(os_create, os_client, os_kubectl):
    """E2B test_pause_kill: clean up a paused instance through the implemented delete route."""
    created = os_create({"timeout": 6000})
    submit(os_client, created["id"], pause_sandbox)
    wait_for_state(os_client, created["id"], "Paused", os_kubectl)
    kill(os_client, created["id"])


def test_pause_state(os_create, os_client, os_kubectl):
    """E2B test_pause_state: pause acceptance followed by the final list state."""
    created = os_create({"timeout": 6000})
    submit(os_client, created["id"], pause_sandbox)
    wait_for_state(os_client, created["id"], "Paused", os_kubectl)
    items = listed(os_client, state=["Paused"],
                   metadata=urlencode({"test_case": created["metadata"]["test_case"]}))
    assert created["id"] in [item["id"] for item in items]
    assert all(item["status"]["state"] == "Paused" for item in items)


def test_resume_state(os_create, os_client, os_kubectl):
    """E2B test_resume_state: resume acceptance followed by the final list state."""
    created = os_create({"timeout": 6000})
    submit(os_client, created["id"], pause_sandbox)
    wait_for_state(os_client, created["id"], "Paused", os_kubectl)
    submit(os_client, created["id"], resume_sandbox)
    wait_for_state(os_client, created["id"], "Running", os_kubectl)
    items = listed(os_client, state=["Running"],
                   metadata=urlencode({"test_case": created["metadata"]["test_case"]}))
    assert created["id"] in [item["id"] for item in items]
    assert all(item["status"]["state"] == "Running" for item in items)


def test_pause_resume_preserves_expiration(os_create, os_client, os_kubectl):
    """The implemented OpenSandbox resume contract preserves the existing deadline."""
    created = os_create({"timeout": 6000})
    original = describe(os_client, created["id"])["expiresAt"]
    for operation, state in ((pause_sandbox, "Paused"), (resume_sandbox, "Running")):
        # Repeat a settled intent as in E2B's concurrent/idempotent lifecycle use.
        for _ in range(2):
            submit(os_client, created["id"], operation)
            info = wait_for_state(os_client, created["id"], state, os_kubectl)
            assert info["id"] == created["id"]
            assert info["expiresAt"] == original


def test_delete_removes_description_and_list_entry(os_create, os_client):
    """Verify deletion through control-plane reads; runtime health is a separate interface."""
    created = os_create()
    assert describe(os_client, created["id"])["status"]["state"] == "Running"
    kill(os_client, created["id"])
    assert listed(os_client, metadata=urlencode({"test_case": created["metadata"]["test_case"]})) == []
    # Describe hides terminating objects before DELETE's owner lookup loses
    # them. Repeated deletion is accepted until the backing object is gone.
    deadline = time.monotonic() + 60
    while True:
        response = delete_sandbox.sync_detailed(created["id"], client=os_client)
        if response.status_code == 404:
            break
        assert response.status_code == 204, (response.status_code, response.content)
        assert time.monotonic() < deadline, "deleted sandbox remains addressable"
        time.sleep(1)


def test_never_timeout(os_create, os_client, os_kubectl):
    """E2B test_never_timeout: preserve the supported native extension across pause/resume."""
    created = os_create({"timeout": 6000, "metadata": {"e2b.agents.kruise.io/never-timeout": "true"}})
    assert "expiresAt" not in describe(os_client, created["id"])
    submit(os_client, created["id"], pause_sandbox)
    assert "expiresAt" not in wait_for_state(os_client, created["id"], "Paused", os_kubectl)
    submit(os_client, created["id"], resume_sandbox)
    assert "expiresAt" not in wait_for_state(os_client, created["id"], "Running", os_kubectl)


def test_concurrent_pause_running_sandbox(os_create, os_client, os_kubectl):
    created = os_create({"timeout": 6000})
    concurrent_transitions(os_client, created["id"], (pause_sandbox,) * 3)
    wait_for_state(os_client, created["id"], "Paused", os_kubectl)


def test_concurrent_resume_paused_sandbox(os_create, os_client, os_kubectl):
    created = os_create({"timeout": 6000})
    submit(os_client, created["id"], pause_sandbox)
    wait_for_state(os_client, created["id"], "Paused", os_kubectl)
    concurrent_transitions(os_client, created["id"], (resume_sandbox,) * 3)
    wait_for_state(os_client, created["id"], "Running", os_kubectl)


def test_concurrent_pause_resume_on_paused_sandbox(os_create, os_client, os_kubectl):
    created = os_create({"timeout": 6000})
    submit(os_client, created["id"], pause_sandbox)
    wait_for_state(os_client, created["id"], "Paused", os_kubectl)
    concurrent_transitions(
        os_client, created["id"], (pause_sandbox,) * 2 + (resume_sandbox,) * 2,
        retry_resume_while_pausing=True,
    )
    wait_for_state(os_client, created["id"], "Running", os_kubectl)


def test_concurrent_pause_resume_on_running_sandbox(os_create, os_client, os_kubectl):
    created = os_create({"timeout": 6000})
    concurrent_transitions(
        os_client, created["id"], (pause_sandbox,) * 2 + (resume_sandbox,) * 2,
    )
    wait_for_state(os_client, created["id"], "Paused", os_kubectl)
