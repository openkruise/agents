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

"""SDK creation contracts and native E2B Pool/metadata behavior."""

from datetime import datetime, timezone
import os
import time
from urllib.parse import urlencode

from opensandbox.api.lifecycle.api.sandboxes import (
    get_sandboxes_sandbox_id,
    post_sandboxes_sandbox_id_pause as pause_sandbox,
    post_sandboxes_sandbox_id_resume as resume_sandbox,
)
import pytest

from test_sandbox import describe, listed, submit, wait_for_state


RESOURCE_KEY = "opensandbox.kruise.io/sandbox-resource"
POD_IP_KEY = "e2b.agents.kruise.io/sandbox-ip"
RETURN_POD_IP_KEY = "e2b.agents.kruise.io/return-sandbox-ip"


def sandbox_json(body, kubectl):
    namespace, name = body["metadata"][RESOURCE_KEY].split("/", 1)
    return kubectl("get", "sandbox", name, "-n", namespace)


def timestamp(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def test_lifecycle(os_create, os_kubectl, os_config):
    body = os_create({
        "extensions": {}, "image": {"uri": os_config["image"]},
        "entrypoint": ["sleep", "infinity"], "env": {"CREATE_E2E": "image"},
        "resourceRequests": {"cpu": "100m", "memory": "128Mi"},
        "resourceLimits": {"cpu": "1", "memory": "256Mi"},
        "metadata": {"userId": "123"},
    })
    assert {"id", "status", "createdAt", "entrypoint"} <= body.keys()
    assert body["status"]["state"] == "Running"
    assert body["entrypoint"] == ["sleep", "infinity"]
    assert timestamp(body["createdAt"]).tzinfo is not None
    assert body["metadata"]["userId"] == "123"
    assert POD_IP_KEY not in body["metadata"]
    box = sandbox_json(body, os_kubectl)
    assert box["metadata"]["namespace"] == os_config["namespace"]
    assert not box["metadata"]["labels"].get("agents.kruise.io/sandbox-pool")
    assert box["metadata"]["annotations"]["agents.kruise.io/owner"]
    assert box["metadata"]["annotations"]["userId"] == "123"
    assert any(condition["type"] == "Ready" and condition["status"] == "True"
               for condition in box.get("status", {}).get("conditions", []))
    for obj in (box["spec"]["template"], os_kubectl(
            "get", "pod", box["metadata"]["name"], "-n", os_config["namespace"])):
        container = next(c for c in obj["spec"]["containers"] if c["name"] == "sandbox")
        assert container["image"] == os_config["image"]
        assert container["command"] == body["entrypoint"]
        assert {"name": "CREATE_E2E", "value": "image"} in container["env"]
        assert container["resources"]["requests"]["cpu"] == "100m"
        assert container["resources"]["requests"]["memory"] == "128Mi"
        assert container["resources"]["limits"]["cpu"] == "1"
        assert container["resources"]["limits"]["memory"] == "256Mi"


def test_pool_source_inherits_template(os_create, os_kubectl, os_config):
    """E2B template creation keeps its workload when no overrides are supplied."""
    pool = os_config["pool"]
    source = os_kubectl("get", "sandboxset", pool, "-n", os_config["namespace"])
    template = source["spec"].get("template")
    if template is None:
        reference = os_kubectl("get", "sandboxtemplate", source["spec"]["templateRef"]["name"],
                               "-n", os_config["namespace"])
        template = reference["spec"]["template"]
    expected = next(container for container in template["spec"]["containers"]
                    if container["name"] == "sandbox")
    body = os_create({"extensions": {"poolRef": pool}, "metadata": {"userId": "123"}})
    assert body["status"]["state"] == "Running"
    assert body["entrypoint"] == []
    assert body["metadata"]["userId"] == "123"
    assert POD_IP_KEY not in body["metadata"]
    box = sandbox_json(body, os_kubectl)
    assert box["metadata"]["labels"]["agents.kruise.io/sandbox-pool"] == pool
    pod = os_kubectl("get", "pod", box["metadata"]["name"], "-n", os_config["namespace"])
    for workload in (box["spec"]["template"], pod):
        container = next(item for item in workload["spec"]["containers"] if item["name"] == "sandbox")
        for field in ("image", "command", "args"):
            assert container.get(field) == expected.get(field), field
        for kind, values in expected.get("resources", {}).items():
            for resource, value in values.items():
                assert container["resources"][kind][resource] == value, (kind, resource)
    after = os_kubectl("get", "sandboxset", pool, "-n", os_config["namespace"])
    assert after["metadata"]["uid"] == source["metadata"]["uid"]
    assert after["spec"] == source["spec"]


@pytest.mark.skipif(not os.environ.get("OPENSANDBOX_CHECKPOINT_REF"),
                    reason="requires an existing OpenSandbox-compatible Checkpoint and restore backend")
def test_checkpoint_source_inherits_template(os_create, os_kubectl, os_config):
    """Clone a real Checkpoint and inspect control-plane inheritance only."""
    checkpoint_ref = os.environ["OPENSANDBOX_CHECKPOINT_REF"]
    checkpoints = os_kubectl("get", "checkpoints", "-n", os_config["namespace"])["items"]
    matching = [checkpoint for checkpoint in checkpoints
                if checkpoint["metadata"]["name"] == checkpoint_ref
                or checkpoint.get("status", {}).get("checkpointId") == checkpoint_ref]
    assert len(matching) == 1, f"expected one Checkpoint for {checkpoint_ref}, found {len(matching)}"
    checkpoint = matching[0]
    assert checkpoint.get("status", {}).get("phase") == "Succeeded"
    checkpoint_id = checkpoint["status"]["checkpointId"]
    template_name = checkpoint["metadata"]["name"]
    source = os_kubectl("get", "sandboxtemplate", template_name, "-n", os_config["namespace"])
    body = os_create({"extensions": {}, "snapshotId": checkpoint_id,
                      "timeout": 6000, "metadata": {"userId": "123"}})
    assert body["status"]["state"] == "Running"
    assert body["entrypoint"] == []
    box = sandbox_json(body, os_kubectl)
    assert box["metadata"]["namespace"] == os_config["namespace"]
    assert box["metadata"]["labels"]["agents.kruise.io/sandbox-template"] == template_name
    annotations = box["metadata"]["annotations"]
    assert annotations["agents.kruise.io/restore-from"] == checkpoint_id
    assert annotations["agents.kruise.io/owner"]
    assert annotations["agents.kruise.io/lock"]
    assert annotations["userId"] == body["metadata"]["userId"] == "123"
    expected = next(container for container in source["spec"]["template"]["spec"]["containers"]
                    if container["name"] == "sandbox")
    actual = next(container for container in box["spec"]["template"]["spec"]["containers"]
                  if container["name"] == "sandbox")
    for field in ("image", "command", "args", "resources"):
        assert actual.get(field) == expected.get(field), field
    for field in ("persistentContents", "runtimes", "volumeClaimTemplates"):
        assert box["spec"].get(field) == source["spec"].get(field), field
    assert timestamp(body["expiresAt"]) == timestamp(box["spec"]["shutdownTime"])
    after = os_kubectl("get", "sandboxtemplate", template_name, "-n", os_config["namespace"])
    assert after["metadata"]["uid"] == source["metadata"]["uid"]
    assert after["spec"] == source["spec"]


def test_sandbox_with_pod_ip(os_create, os_client, os_kubectl):
    body = os_create({"timeout": 6000, "metadata": {RETURN_POD_IP_KEY: "true"}})
    box = sandbox_json(body, os_kubectl)
    assert body["metadata"][POD_IP_KEY] == box["status"]["podInfo"]["podIP"]
    assert body["metadata"][POD_IP_KEY]
    assert RETURN_POD_IP_KEY not in body["metadata"]
    owner = box["metadata"]["annotations"]["agents.kruise.io/owner"]
    metadata = urlencode({"test_case": body["metadata"]["test_case"]})

    def assert_readback(state):
        deadline = time.monotonic() + 60
        while True:
            info = describe(os_client, body["id"])
            items = listed(os_client, metadata=metadata)
            assert [item["id"] for item in items] == [body["id"]]
            current = sandbox_json(body, os_kubectl)
            annotations = current["metadata"]["annotations"]
            assert annotations[RETURN_POD_IP_KEY] == "true"
            assert annotations["agents.kruise.io/owner"] == owner
            ip = current.get("status", {}).get("podInfo", {}).get("podIP", "")
            for observed in (info, items[0]):
                assert RETURN_POD_IP_KEY not in observed["metadata"]
                assert observed["metadata"][RESOURCE_KEY] == body["metadata"][RESOURCE_KEY]
            if (ip or state == "Paused") and all(
                    observed["status"]["state"] == state
                    and observed["metadata"].get(POD_IP_KEY, "") == ip
                    and (ip or POD_IP_KEY not in observed["metadata"])
                    for observed in (info, items[0])):
                return
            assert time.monotonic() < deadline, (state, ip, [
                (observed["status"]["state"], observed["metadata"].get(POD_IP_KEY))
                for observed in (info, items[0])])
            time.sleep(1)

    assert_readback("Running")
    submit(os_client, body["id"], pause_sandbox)
    wait_for_state(os_client, body["id"], "Paused", os_kubectl)
    assert_readback("Paused")
    submit(os_client, body["id"], resume_sandbox)
    wait_for_state(os_client, body["id"], "Running", os_kubectl)
    assert_readback("Running")


def test_no_stock(os_create, os_kubectl, os_config):
    pool = os_config["empty_pool"]
    before = os_kubectl("get", "sandboxset", pool, "-n", os_config["namespace"])
    assert before["spec"]["replicas"] == 0
    assert before.get("status", {}).get("availableReplicas", 0) == 0
    body = os_create({"extensions": {"poolRef": pool}, "metadata": {"userId": "123"}})
    assert body["status"]["state"] == "Running"
    assert body["metadata"]["userId"] == "123"
    box = sandbox_json(body, os_kubectl)
    assert box["metadata"]["labels"]["agents.kruise.io/sandbox-pool"] == pool
    after = os_kubectl("get", "sandboxset", pool, "-n", os_config["namespace"])
    assert after["metadata"]["uid"] == before["metadata"]["uid"]
    assert after["spec"] == before["spec"]


def test_timeout(os_create, os_kubectl):
    for seconds in (120, 1200):
        started = datetime.now(timezone.utc)
        body = os_create({"timeout": seconds})
        finished = datetime.now(timezone.utc)
        box = sandbox_json(body, os_kubectl)
        expires = timestamp(body["expiresAt"])
        assert expires == timestamp(box["spec"]["shutdownTime"])
        assert seconds - 5 <= (expires - started).total_seconds()
        assert (expires - finished).total_seconds() <= seconds + 5


def test_never_timeout(os_create, os_kubectl):
    body = os_create({"timeout": 60, "metadata": {"e2b.agents.kruise.io/never-timeout": "true"}})
    assert "expiresAt" not in body
    assert not sandbox_json(body, os_kubectl)["spec"].get("shutdownTime")
    assert "e2b.agents.kruise.io/never-timeout" not in body["metadata"]


def test_sandbox_with_labels(os_create, os_client, os_kubectl, os_config):
    """E2B label metadata is visible on creation, description and the live Pod."""
    body = os_create({"metadata": {"userId": "123", "label:app": "my-test-app", "label:env": "e2e"}})
    box = sandbox_json(body, os_kubectl)
    labels = {"app": "my-test-app", "env": "e2e"}
    pod = os_kubectl("get", "pod", box["metadata"]["name"], "-n", os_config["namespace"])
    deadline = time.monotonic() + 60
    while any(pod["metadata"].get("labels", {}).get(name) != value for name, value in labels.items()):
        assert time.monotonic() < deadline, "created sandbox labels did not reach the Pod"
        time.sleep(1)
        pod = os_kubectl("get", "pod", box["metadata"]["name"], "-n", os_config["namespace"])
    response = get_sandboxes_sandbox_id.sync_detailed(body["id"], client=os_client)
    assert response.status_code == 200, (response.status_code, response.content)
    info = response.parsed.to_dict()
    assert info["id"] == body["id"]
    assert body["metadata"]["userId"] == box["metadata"]["annotations"]["userId"] == "123"
    assert info["metadata"]["userId"] == "123"
    for name, value in labels.items():
        assert body["metadata"][name] == value
        assert info["metadata"][name] == value
        assert box["metadata"]["labels"][name] == value
        assert box["spec"]["template"]["metadata"]["labels"][name] == value
        assert pod["metadata"]["labels"][name] == value
        assert f"label:{name}" not in body["metadata"]
        assert f"label:{name}" not in info["metadata"]
