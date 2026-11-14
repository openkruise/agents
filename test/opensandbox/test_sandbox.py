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
from threading import Barrier
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


def submit(client, sandbox_id, operation, accepted=(202,)):
    response = operation.sync_detailed(sandbox_id, client=client)
    assert response.status_code in accepted, (response.status_code, response.content)
    return response.status_code


def kill(client, sandbox_id):
    response = delete_sandbox.sync_detailed(sandbox_id, client=client)
    assert response.status_code == 204, (response.status_code, response.content)
    wait_for_deleted(client, sandbox_id)


def concurrent_transitions(client, sandbox_id, operations):
    # Use E2B's existing request counts; start each one-shot request together.
    barrier = Barrier(len(operations))

    def request(operation):
        barrier.wait(timeout=10)
        return submit(client, sandbox_id, operation, accepted=(202, 409))

    with ThreadPoolExecutor(max_workers=len(operations)) as executor:
        statuses = list(executor.map(request, operations))
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


def test_list_by_state(os_create, os_client):
    """E2B test_list_by_state: the created instance is visible in the live-state filter."""
    created = os_create()
    items = listed(os_client, state=["Running", "Paused"],
                   metadata=urlencode({"test_case": created["metadata"]["test_case"]}))
    assert created["id"] in [item["id"] for item in items]
    assert all(item["status"]["state"] in ("Running", "Paused") for item in items)


def test_timeout(os_create, os_client):
    """E2B test_timeout, mapped to OpenSandbox's extend-only expiration contract."""
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


def test_is_running(os_create, os_client):
    """E2B test_is_running: a live instance becomes unavailable after kill; no runtime health API."""
    created = os_create()
    assert describe(os_client, created["id"])["status"]["state"] == "Running"
    kill(os_client, created["id"])


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
    statuses = concurrent_transitions(
        os_client, created["id"], (pause_sandbox,) * 2 + (resume_sandbox,) * 2,
    )
    # A rejected resume does not change state; only accepted intent promises progress.
    expected = "Running" if 202 in statuses[2:] else "Paused"
    wait_for_state(os_client, created["id"], expected, os_kubectl)


def test_concurrent_pause_resume_on_running_sandbox(os_create, os_client, os_kubectl):
    created = os_create({"timeout": 6000})
    statuses = concurrent_transitions(
        os_client, created["id"], (pause_sandbox,) * 2 + (resume_sandbox,) * 2,
    )
    expected = "Paused" if 202 in statuses[:2] else "Running"
    wait_for_state(os_client, created["id"], expected, os_kubectl)
