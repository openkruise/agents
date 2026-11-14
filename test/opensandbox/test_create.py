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

"""Creation portions of the six corresponding E2B sandbox test groups."""

from datetime import datetime, timezone


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
    assert box["metadata"]["annotations"]["agents.kruise.io/owner"]
    assert box["metadata"]["annotations"]["userId"] == "123"
    for obj in (box["spec"]["template"], os_kubectl(
            "get", "pod", box["metadata"]["name"], "-n", os_config["namespace"])):
        container = next(c for c in obj["spec"]["containers"] if c["name"] == "sandbox")
        assert container["image"] == os_config["image"]
        assert container["command"] == body["entrypoint"]
        assert {"name": "CREATE_E2E", "value": "image"} in container["env"]
        assert container["resources"]["requests"]["cpu"] == "100m"
        assert container["resources"]["requests"]["memory"] == "128Mi"
        assert container["resources"]["limits"]["memory"] == "256Mi"


def test_sandbox_with_pod_ip(os_create, os_kubectl):
    body = os_create({"metadata": {RETURN_POD_IP_KEY: "true"}})
    box = sandbox_json(body, os_kubectl)
    assert body["metadata"][POD_IP_KEY] == box["status"]["podInfo"]["podIP"]
    assert body["metadata"][POD_IP_KEY]
    assert RETURN_POD_IP_KEY not in body["metadata"]


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


def test_sandbox_with_labels_and_command(os_create, os_kubectl):
    body = os_create({"metadata": {"userId": "123", "label:app": "my-test-app", "label:env": "e2e"}})
    box = sandbox_json(body, os_kubectl)
    assert body["metadata"]["userId"] == box["metadata"]["annotations"]["userId"] == "123"
    for name, value in {"app": "my-test-app", "env": "e2e"}.items():
        assert body["metadata"][name] == value
        assert box["metadata"]["labels"][name] == value
        assert box["spec"]["template"]["metadata"]["labels"][name] == value
        assert f"label:{name}" not in body["metadata"]
