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

"""Shared API-key quota admission through the OpenSandbox creation route."""

from concurrent.futures import ThreadPoolExecutor
import json
import os
from threading import Barrier
import time

from opensandbox.api.lifecycle.api.sandboxes import (
    post_sandboxes_sandbox_id_pause as pause_sandbox,
    post_sandboxes_sandbox_id_resume as resume_sandbox,
)
import pytest

from quota_observer import (
    cleanup_wave, metrics_snapshot, recover_admission, requires_recovery, wait_for_zero_count,
)
from test_create import sandbox_json
from test_sandbox import describe, kill, submit, wait_for_state


@pytest.fixture(autouse=True)
def quota_profile(request, os_config):
    profile = os_config["quota_profile"]
    if profile == "none":
        pytest.skip("set OPENSANDBOX_QUOTA_PROFILE to enable quota E2E")
    assert profile in {"redis", "redis-clone", "redis-fault", "no-redis"}, profile
    no_redis_case = request.node.originalname == "test_quota_unenforced_without_redis"
    if (profile == "no-redis") != no_redis_case:
        pytest.skip("requires no-Redis profile" if no_redis_case else "requires Redis quota profile")


def quota_spec(dimension="sandbox.count", scope="all", limit=1):
    return {"limits": [{"dimension": dimension, "scope": scope, "limit": limit}]}


def assert_quota_response(response):
    assert response.status_code == 403, (response.status_code, response.content)
    error = json.loads(response.content)
    assert error["code"] == "FORBIDDEN", error
    assert "quota" in error["message"].lower(), error


def create_after_release(create, client, timeout=120, overrides=None):
    # Pause updates quota through the existing watch path, so observe admission
    # becoming available rather than assuming its cache update is synchronous.
    deadline = time.monotonic() + timeout
    while True:
        response = create({"timeout": 600, **(overrides or {})}, client=client, expected_status=(202, 403))
        if response.status_code == 202:
            return response.parsed.to_dict()
        assert_quota_response(response)
        assert time.monotonic() < deadline, "quota was not released before the deadline"
        time.sleep(1)


def assert_owner_and_lock(created, key_id, kubectl):
    box = sandbox_json(created, kubectl)
    annotations = box["metadata"].get("annotations", {})
    assert annotations.get("agents.kruise.io/owner") == key_id
    assert annotations.get("agents.kruise.io/lock")
    return box


def wait_for_pool_stock(kubectl, namespace, pool, minimum=3, timeout=180):
    # Match E2B's pre-warmed concurrency case: every admitted request should
    # claim a ready delivery instead of waiting for an unrelated cold start.
    deadline = time.monotonic() + timeout
    while True:
        budget = deadline - time.monotonic()
        assert budget > 0, f"{pool}: Ready stock did not recover within {timeout} seconds"
        boxes = kubectl("get", "sandboxes", "-n", namespace,
                        "-l", f"agents.kruise.io/sandbox-pool={pool}",
                        timeout=min(30, budget))["items"]
        available = []
        for box in boxes:
            metadata = box["metadata"]
            annotations = metadata.get("annotations", {})
            if (metadata.get("deletionTimestamp")
                    or annotations.get("agents.kruise.io/owner")
                    or annotations.get("agents.kruise.io/lock")):
                continue
            if box.get("status", {}).get("phase") == "Running" and any(
                    item["type"] == "Ready" and item["status"] == "True"
                    for item in box.get("status", {}).get("conditions", [])):
                available.append(box)
        if len(available) >= minimum:
            return
        assert time.monotonic() < deadline, f"{pool}: only {len(available)} ready unclaimed deliveries"
        time.sleep(1)


def test_count_all_limit_blocks_then_delete_releases(
        os_key_factory, os_client_factory, os_create):
    key = os_key_factory(quota=quota_spec())
    client = os_client_factory(key["key"])
    first = os_create({"timeout": 600}, client=client)
    assert describe(client, first["id"])["status"]["state"] == "Running"
    assert_quota_response(os_create(client=client, expected_status=403))

    kill(client, first["id"])
    replacement = create_after_release(os_create, client)
    assert replacement["id"] != first["id"]
    assert describe(client, replacement["id"])["status"]["state"] == "Running"


def test_zero_count_limit_rejects_without_claiming_pool_candidates(
        os_key_factory, os_client_factory, os_create, os_config, os_kubectl):
    key = os_key_factory(quota=quota_spec(limit=0))
    client = os_client_factory(key["key"])
    namespace = os_config["namespace"]
    wait_for_pool_stock(os_kubectl, namespace, os_config["pool"], minimum=1)
    before = os_kubectl("get", "sandboxes", "-n", namespace,
                        "-l", f"agents.kruise.io/sandbox-pool={os_config['pool']}")["items"]
    candidates = {
        box["metadata"]["uid"]: box["metadata"]["name"] for box in before
        if not box["metadata"].get("deletionTimestamp")
        and not box["metadata"].get("annotations", {}).get("agents.kruise.io/owner")
        and not box["metadata"].get("annotations", {}).get("agents.kruise.io/lock")
        and any(item["type"] == "Ready" and item["status"] == "True"
                for item in box.get("status", {}).get("conditions", []))
    }
    assert candidates, "hard-zero quota case requires a ready, unclaimed Pool candidate"
    assert_quota_response(os_create(client=client, expected_status=403))

    after = os_kubectl("get", "sandboxes", "-n", namespace)["items"]
    owned = [box["metadata"]["name"] for box in after
             if box["metadata"].get("annotations", {}).get("agents.kruise.io/owner") == key["id"]]
    assert owned == [], owned
    by_uid = {box["metadata"]["uid"]: box for box in after}
    assert candidates.keys() <= by_uid.keys(), "a rejected claim removed a Pool candidate"
    for uid, name in candidates.items():
        annotations = by_uid[uid]["metadata"].get("annotations", {})
        assert not annotations.get("agents.kruise.io/owner"), name
        assert not annotations.get("agents.kruise.io/lock"), name


def test_no_stock_path_stamps_owner_lock_and_counts_quota(
        os_key_factory, os_client_factory, os_create, os_config, os_kubectl):
    key = os_key_factory(quota=quota_spec())
    client = os_client_factory(key["key"])
    pool, namespace = os_config["empty_pool"], os_config["namespace"]
    source = os_kubectl("get", "sandboxset", pool, "-n", namespace)
    assert source["spec"]["replicas"] == 0
    assert source.get("status", {}).get("availableReplicas", 0) == 0

    payload = {"extensions": {"poolRef": pool}, "timeout": 600}
    created = os_create(payload, client=client)
    box = assert_owner_and_lock(created, key["id"], os_kubectl)
    assert box["metadata"]["labels"]["agents.kruise.io/sandbox-pool"] == pool
    assert describe(client, created["id"])["status"]["state"] == "Running"
    assert_quota_response(os_create(payload, client=client, expected_status=403))

    after = os_kubectl("get", "sandboxset", pool, "-n", namespace)
    assert after["metadata"]["uid"] == source["metadata"]["uid"]
    assert after["spec"] == source["spec"]


@pytest.mark.skipif(not os.environ.get("OPENSANDBOX_CHECKPOINT_REF"),
                    reason="requires an existing OpenSandbox-compatible Checkpoint and restore backend")
def test_checkpoint_clone_stamps_owner_lock_and_counts_quota(
        os_key_factory, os_client_factory, os_create, os_config, os_kubectl):
    namespace = os_config["namespace"]
    checkpoint_ref = os.environ["OPENSANDBOX_CHECKPOINT_REF"]
    checkpoints = os_kubectl("get", "checkpoints", "-n", namespace)["items"]
    matching = [checkpoint for checkpoint in checkpoints
                if checkpoint["metadata"]["name"] == checkpoint_ref
                or checkpoint.get("status", {}).get("checkpointId") == checkpoint_ref]
    assert len(matching) == 1, f"expected one Checkpoint for {checkpoint_ref}, found {len(matching)}"
    checkpoint = matching[0]
    assert checkpoint.get("status", {}).get("phase") == "Succeeded"
    checkpoint_id = checkpoint["status"]["checkpointId"]
    template_name = checkpoint["metadata"]["name"]
    source = os_kubectl("get", "sandboxtemplate", template_name, "-n", namespace)

    key = os_key_factory(quota=quota_spec())
    client = os_client_factory(key["key"])
    payload = {"extensions": {}, "snapshotId": checkpoint_id, "timeout": 6000}
    created = os_create(payload, client=client)
    box = assert_owner_and_lock(created, key["id"], os_kubectl)
    assert box["metadata"]["annotations"]["agents.kruise.io/restore-from"] == checkpoint_id
    assert box["metadata"]["labels"]["agents.kruise.io/sandbox-template"] == template_name
    assert describe(client, created["id"])["status"]["state"] == "Running"
    assert_quota_response(os_create(payload, client=client, expected_status=403))

    after = os_kubectl("get", "sandboxtemplate", template_name, "-n", namespace)
    assert after["metadata"]["uid"] == source["metadata"]["uid"]
    assert after["spec"] == source["spec"]


def test_concurrent_creates_do_not_exceed_count_limit(
        os_key_factory, os_client_factory, os_create, os_config, os_kubectl, os_kubectl_text):
    key = os_key_factory(quota=quota_spec(limit=3))
    client = os_client_factory(key["key"])
    namespace = os_config["namespace"]
    wait_for_pool_stock(os_kubectl, namespace, os_config["pool"])
    for burst in range(2):
        barrier = Barrier(10)

        def attempt(_):
            barrier.wait(timeout=10)
            return os_create({"timeout": 600}, client=client, expected_status=(202, 403))

        before = metrics_snapshot(os_kubectl, os_kubectl_text)
        with ThreadPoolExecutor(max_workers=10) as executor:
            responses = list(executor.map(attempt, range(10)))
        after = metrics_snapshot(os_kubectl, os_kubectl_text)
        for response in responses:
            if response.status_code == 403:
                assert_quota_response(response)
        created = [response.parsed.to_dict() for response in responses if response.status_code == 202]
        if not requires_recovery(len(created), len(responses) - len(created), before, after, burst):
            break
        probes = recover_admission(os_create, client, os_kubectl, os_kubectl_text,
                                   before, after, assert_quota_response)
        cleanup_wave([*created, *probes], client, os_config, os_kubectl)
        wait_for_zero_count(os_kubectl, os_kubectl_text, key["id"])
        wait_for_pool_stock(os_kubectl, namespace, os_config["pool"])

    assert len({item["id"] for item in created}) == 3
    for item in created:
        assert_owner_and_lock(item, key["id"], os_kubectl)
        assert describe(client, item["id"])["status"]["state"] == "Running"
    # Compare all live CRs for this fresh owner with the exact accepted set so
    # rejected attempts cannot leave an additional claimed delivery behind.
    owned = [box for box in os_kubectl("get", "sandboxes", "-n", namespace)["items"]
             if box["metadata"].get("annotations", {}).get("agents.kruise.io/owner") == key["id"]]
    assert {f"{box['metadata']['namespace']}/{box['metadata']['name']}" for box in owned} == {
        item["metadata"]["opensandbox.kruise.io/sandbox-resource"] for item in created
    }


def test_count_all_limit_still_counts_paused_sandbox(
        os_key_factory, os_client_factory, os_create, os_kubectl):
    key = os_key_factory(quota=quota_spec())
    client = os_client_factory(key["key"])
    created = os_create({"timeout": 6000}, client=client)
    submit(client, created["id"], pause_sandbox)
    wait_for_state(client, created["id"], "Paused", os_kubectl)
    assert_quota_response(os_create(client=client, expected_status=403))


def test_running_pause_releases_and_resume_can_exceed_limit(
        os_key_factory, os_client_factory, os_create, os_kubectl):
    key = os_key_factory(quota=quota_spec(scope="running"))
    client = os_client_factory(key["key"])
    first = os_create({"timeout": 6000}, client=client)
    assert_quota_response(os_create(client=client, expected_status=403))

    submit(client, first["id"], pause_sandbox)
    wait_for_state(client, first["id"], "Paused", os_kubectl)
    second = create_after_release(os_create, client)
    submit(client, first["id"], resume_sandbox)
    wait_for_state(client, first["id"], "Running", os_kubectl)
    assert describe(client, second["id"])["status"]["state"] == "Running"
    # Native resume is permitted above the running limit; new claims are not.
    assert_quota_response(os_create(client=client, expected_status=403))


def test_running_resource_limits_pause_and_resume(
        os_key_factory, os_client_factory, os_create, os_config, os_kubectl):
    quota = {"limits": [{"dimension": dimension, "scope": "running", "limit": limit}
                        for dimension, limit in (("limits.cpu", 750), ("limits.memory", 768))]}
    key = os_key_factory(quota=quota)
    client = os_client_factory(key["key"])
    payload = {
        "extensions": {}, "image": {"uri": os_config["image"]},
        "entrypoint": ["sleep", "infinity"], "timeout": 6000,
        "resourceRequests": {"cpu": "100m", "memory": "128Mi"},
        "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
    }
    first = os_create(payload, client=client)
    assert_owner_and_lock(first, key["id"], os_kubectl)
    assert describe(client, first["id"])["status"]["state"] == "Running"
    assert_quota_response(os_create(payload, client=client, expected_status=403))

    submit(client, first["id"], pause_sandbox)
    wait_for_state(client, first["id"], "Paused", os_kubectl)
    second = create_after_release(os_create, client, overrides=payload)
    assert second["id"] != first["id"]
    assert_owner_and_lock(second, key["id"], os_kubectl)
    submit(client, first["id"], resume_sandbox)
    wait_for_state(client, first["id"], "Running", os_kubectl)
    assert describe(client, second["id"])["status"]["state"] == "Running"
    assert_quota_response(os_create(payload, client=client, expected_status=403))


@pytest.mark.parametrize("dimension,limit", [("limits.cpu", 750), ("limits.memory", 768)])
def test_resource_all_limit_uses_workload_limits(
        os_key_factory, os_client_factory, os_create, os_config, os_kubectl,
        dimension, limit):
    # Match E2B's 500m / 512Mi workload and 750 millicore / 768 MiB limits.
    # An image source fixes the footprint without changing shared pool assets.
    key = os_key_factory(quota=quota_spec(dimension=dimension, limit=limit))
    client = os_client_factory(key["key"])
    payload = {
        "extensions": {}, "image": {"uri": os_config["image"]},
        "entrypoint": ["sleep", "infinity"], "timeout": 600,
        "resourceRequests": {"cpu": "100m", "memory": "128Mi"},
        "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
    }
    created = os_create(payload, client=client)
    namespace, name = created["metadata"]["opensandbox.kruise.io/sandbox-resource"].split("/", 1)
    box = os_kubectl("get", "sandbox", name, "-n", namespace)
    workload = next(container for container in box["spec"]["template"]["spec"]["containers"]
                    if container["name"] == "sandbox")
    assert workload["resources"]["limits"] == payload["resourceLimits"]
    assert box["metadata"]["annotations"]["agents.kruise.io/owner"] == key["id"]
    assert_quota_response(os_create(payload, client=client, expected_status=403))


def test_quota_unenforced_without_redis(os_key_factory, os_client_factory, os_create):
    key = os_key_factory(quota=quota_spec(limit=0))
    client = os_client_factory(key["key"])
    first = os_create({"timeout": 600}, client=client)
    second = os_create({"timeout": 600}, client=client)
    assert first["id"] != second["id"]
    for created in (first, second):
        assert describe(client, created["id"])["status"]["state"] == "Running"
