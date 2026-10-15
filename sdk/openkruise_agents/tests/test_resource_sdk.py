# Copyright 2026.
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

"""Unit tests for the resource management SDK (Template/SandboxSet/PoolAutoscaler).

All HTTP traffic is mocked with httpx.MockTransport; no server needed.
"""

from __future__ import annotations

import json

import httpx
import pytest

import openkruise_agents.client as client_module
from openkruise_agents import (
    BadRequestError,
    ConflictError,
    KruiseApiError,
    NotFoundError,
    PoolAutoscaler,
    SandboxSet,
    Template,
    TemplateBuildFailedError,
    UnsupportedOperationError,
    WaitTimeoutError,
)
from openkruise_agents.client import API_PREFIX
from openkruise_agents.models import (
    BusinessProcessOptions,
    BusinessStartPolicy,
    CapacityPolicy,
    CapacityScalingRules,
    Container,
    PersistentContent,
    PoolAutoscalerSpec,
    ResourceRequirements,
    RuntimeConfig,
    SandboxSetSpec,
    SandboxTemplatePhase,
    SandboxTemplateSpec,
    TemplateBackend,
    TemplateBuildOptions,
)

API_BASE = "http://testserver"


def make_client(handler) -> httpx.Client:
    return httpx.Client(transport=httpx.MockTransport(handler))


def make_sdk_client(handler):
    from openkruise_agents import KruiseClient

    return KruiseClient(api_key="test-key", base_url=API_BASE, http_client=make_client(handler))


class TestDefaultClient:
    """Users may omit ``client`` entirely: a lazy process-wide client is
    built from the E2B environment variables — no separate Kruise variables.
    """

    def test_default_client_reuses_e2b_env_and_is_singleton(self, monkeypatch):
        monkeypatch.setenv("E2B_API_KEY", "e2b-key")
        # The customized E2B SDK sets E2B_API_URL to the E2B API path; the
        # origin is kept.
        monkeypatch.setenv("E2B_API_URL", "http://env-server/kruise/api")
        monkeypatch.setattr(client_module, "_default_client", None)

        first = client_module.default_client()
        assert first.api_key == "e2b-key"
        assert first.base_url == "http://env-server"
        assert client_module.default_client() is first
        first.close()
        monkeypatch.setattr(client_module, "_default_client", None)

    def test_default_client_accepts_bare_origin(self, monkeypatch):
        monkeypatch.setenv("E2B_API_URL", "http://env-server:8997/")
        monkeypatch.setattr(client_module, "_default_client", None)

        first = client_module.default_client()
        assert first.base_url == "http://env-server:8997"
        first.close()
        monkeypatch.setattr(client_module, "_default_client", None)

    def test_default_client_defaults_without_env(self, monkeypatch):
        monkeypatch.delenv("E2B_API_KEY", raising=False)
        monkeypatch.delenv("E2B_API_URL", raising=False)
        monkeypatch.setattr(client_module, "_default_client", None)

        first = client_module.default_client()
        assert first.api_key == ""
        assert first.base_url == client_module.DEFAULT_BASE_URL
        first.close()
        monkeypatch.setattr(client_module, "_default_client", None)

    def test_explicit_args_override_env(self, monkeypatch):
        monkeypatch.setenv("E2B_API_KEY", "env-key")
        monkeypatch.setenv("E2B_API_URL", "http://env-server/kruise/api")
        explicit = client_module.KruiseClient(api_key="explicit", base_url="http://explicit-server")
        assert explicit.api_key == "explicit"
        assert explicit.base_url == "http://explicit-server"
        explicit.close()

    def test_build_without_client_uses_default_client(self, monkeypatch):
        captured = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["auth"] = request.headers.get("X-API-Key")
            captured["url"] = str(request.url)
            return httpx.Response(
                201, json={"name": "no-client", "status": {"phase": "Building"}}
            )

        env_client = client_module.KruiseClient(
            api_key="env-key",
            base_url="http://env-server",
            http_client=make_client(handler),
        )
        monkeypatch.setattr("openkruise_agents.template.default_client", lambda: env_client)

        build = Template.build(
            name="no-client",
            spec=SandboxTemplateSpec(containers=[Container(name="main", image="i")]),
        )
        assert captured["url"] == f"http://env-server{API_PREFIX}/templates"
        assert captured["auth"] == "env-key"
        assert build.template_id == "no-client"
        assert build.status is not None and build.status.value == "Building"
        env_client.close()

    def test_sandboxset_and_autoscaler_also_fall_back(self, monkeypatch):
        seen_paths = []

        def handler(request: httpx.Request) -> httpx.Response:
            seen_paths.append((request.method, request.url.path))
            if request.url.path.endswith("/sandboxsets"):
                return httpx.Response(201, json={"name": "p", "ready": True})
            return httpx.Response(201, json={"name": "pa", "active": True})

        env_client = client_module.KruiseClient(
            api_key="env-key",
            base_url="http://env-server",
            http_client=make_client(handler),
        )
        monkeypatch.setattr("openkruise_agents.sandboxset.default_client", lambda: env_client)
        monkeypatch.setattr("openkruise_agents.poolautoscaler.default_client", lambda: env_client)

        pool = SandboxSet.create(name="p", template_ref="t", spec=SandboxSetSpec(replicas=3))
        pa = PoolAutoscaler.create(name="pa", scale_target_ref="p")
        assert pool.ready is True
        assert pa.active is True
        assert seen_paths == [
            ("POST", f"{API_PREFIX}/sandboxsets"),
            ("POST", f"{API_PREFIX}/poolautoscalers"),
        ]
        env_client.close()


class TestTemplateBuild:
    def test_build_keyword_style_request_body(self):
        captured = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["url"] = str(request.url)
            captured["auth"] = request.headers.get("X-API-Key")
            captured["body"] = json.loads(request.content)
            return httpx.Response(
                201,
                json={
                    "name": "code-interpreter-v2",
                    "status": {"phase": "Building"},
                },
            )

        client = make_sdk_client(handler)
        build = Template.build(
            name="code-interpreter-v2",
            spec=SandboxTemplateSpec(
                containers=[
                    Container(
                        name="runtime",
                        image="example/runtime@sha256:abc",
                        command=["python", "-m", "runtime.server"],
                        resources=ResourceRequirements(
                            requests={"cpu": "1", "memory": "2Gi"},
                            limits={"cpu": "2", "memory": "4Gi"},
                        ),
                    )
                ],
                runtimes=[RuntimeConfig(name="agent-runtime")],
            ),
            options=TemplateBuildOptions(
                checkpoint=True,
                image_cache=True,
                business_process=BusinessProcessOptions(
                    start_policy=BusinessStartPolicy.AFTER_CLAIM_INITIALIZATION
                ),
            ),
            client=client,
        )

        assert captured["url"] == f"{API_BASE}{API_PREFIX}/templates"
        assert captured["auth"] == "test-key"
        body = captured["body"]
        # Kubernetes default backend is omitted from the wire request.
        assert "backend" not in body
        assert body["name"] == "code-interpreter-v2"
        assert body["spec"]["containers"][0]["resources"]["requests"]["cpu"] == "1"
        assert body["spec"]["runtimes"][0]["name"] == "agent-runtime"
        assert body["options"]["checkpoint"] is True
        assert body["options"]["businessProcess"]["startPolicy"] == "after_claim_initialization"
        assert build.template_id == "code-interpreter-v2"
        assert build.status is not None

    def test_build_substrate_sends_backend(self):
        captured = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["body"] = json.loads(request.content)
            return httpx.Response(201, json={"name": "python-runtime", "status": {"phase": "Building"}})

        build = Template.build(
            name="python-runtime",
            backend=TemplateBackend.SUBSTRATE,
            spec=SandboxTemplateSpec(containers=[Container(name="main", image="python:3.11@sha256:abc")]),
            client=make_sdk_client(handler),
        )
        assert captured["body"]["backend"] == "substrate"
        assert build.template_id == "python-runtime"

    def test_build_duplicate_returns_conflict(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(409, json={"message": "template already exists"})

        with pytest.raises(ConflictError):
            Template.build(
                name="dup",
                spec=SandboxTemplateSpec(containers=[Container(name="main", image="python:3.11")]),
                client=make_sdk_client(handler),
            )

    def test_build_chain_run_cmd(self):
        captured = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["body"] = json.loads(request.content)
            return httpx.Response(201, json={"name": "python-agent:production", "status": {"phase": "Building"}})

        template = (
            Template(
                spec=SandboxTemplateSpec(
                    containers=[Container(name="business", image="registry/python-agent@sha256:d")],
                    persistent_contents=[PersistentContent.FILESYSTEM],
                ),
                options=TemplateBuildOptions(checkpoint=True),
            )
            .run_cmd(["python -m pip install -r /app/requirements.txt", "python -m compileall /app"], user="root")
            .run_cmd("echo done")
        )
        build = Template.build(template, "python-agent:production", client=make_sdk_client(handler))

        assert captured["body"]["name"] == "python-agent:production"
        commands = captured["body"]["options"]["runCommands"]
        assert commands[0] == {"command": ["python -m pip install -r /app/requirements.txt", "python -m compileall /app"], "user": "root"}
        assert commands[1] == {"command": "echo done"}
        assert build.template_id == "python-agent:production"

    def test_build_missing_name_or_spec(self):
        with pytest.raises(ValueError):
            Template.build(spec=SandboxTemplateSpec(containers=[Container(name="a", image="i")]))
        with pytest.raises(ValueError):
            Template.build(name="x")

    def test_wait_until_ready_polling(self, monkeypatch):
        phases = iter(["Building", "Building", "Ready"])
        requests_seen = []

        def handler(request: httpx.Request) -> httpx.Response:
            requests_seen.append(str(request.url))
            phase = next(phases)
            return httpx.Response(
                200,
                json={
                    "name": "rt",
                    "status": {
                        "phase": phase,
                        "artifactRef": {
                            "backend": "substrate",
                            "actorTemplateName": "rt",
                        },
                    },
                },
            )

        def fast_sleep(_seconds: float) -> None:
            return None

        monkeypatch.setattr("openkruise_agents.template.time.sleep", fast_sleep)
        build = Template.build(
            name="rt",
            spec=SandboxTemplateSpec(containers=[Container(name="main", image="i")]),
            client=make_sdk_client(handler),
        )
        result = build.wait_until_ready(timeout=5, interval=0)
        assert result.status is not None and result.status.value == "Ready"
        assert result.artifact is not None and result.artifact.backend == "substrate"
        assert result.artifact.actor_template_name == "rt"
        assert len(requests_seen) == 3

    def test_wait_until_ready_pending_then_ready(self, monkeypatch):
        phases = iter(["Pending", "Building", "Ready"])

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(200, json={"name": "rt", "status": {"phase": next(phases)}})

        monkeypatch.setattr("openkruise_agents.template.time.sleep", lambda _: None)
        build = Template.build(
            name="rt",
            spec=SandboxTemplateSpec(containers=[Container(name="main", image="i")]),
            client=make_sdk_client(handler),
        )
        result = build.wait_until_ready(timeout=5, interval=0)
        assert result.status is not None and result.status.value == "Ready"

    def test_wait_until_ready_failed_raises(self, monkeypatch):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(
                200,
                json={"name": "rt", "status": {"phase": "Failed", "message": "golden snapshot error"}},
            )

        monkeypatch.setattr("openkruise_agents.template.time.sleep", lambda _: None)
        build = Template.build(
            name="rt",
            spec=SandboxTemplateSpec(containers=[Container(name="main", image="i")]),
            client=make_sdk_client(handler),
        )
        with pytest.raises(TemplateBuildFailedError):
            build.wait_until_ready(timeout=1, interval=0)

    def test_wait_until_ready_timeout(self, monkeypatch):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(200, json={"name": "rt", "status": {"phase": "Building"}})

        monkeypatch.setattr("openkruise_agents.template.time.sleep", lambda _: None)
        build = Template.build(
            name="rt",
            spec=SandboxTemplateSpec(containers=[Container(name="main", image="i")]),
            client=make_sdk_client(handler),
        )
        with pytest.raises(WaitTimeoutError):
            build.wait_until_ready(timeout=0, interval=0)

    def test_build_second_positional_arg_not_string_raises(self):
        template = Template(spec=SandboxTemplateSpec(containers=[Container(name="a", image="i")]))
        with pytest.raises(TypeError):
            Template.build(template, 123, client=make_sdk_client(lambda r: httpx.Response(201, json={})))

    def test_run_cmd_requires_checkpoint_and_filesystem(self):
        with pytest.raises(ValueError, match="checkpoint"):
            Template(
                spec=SandboxTemplateSpec(
                    containers=[Container(name="a", image="i")],
                    persistent_contents=[PersistentContent.FILESYSTEM],
                ),
            ).run_cmd("echo hi")
        with pytest.raises(ValueError, match="FILESYSTEM"):
            Template(
                spec=SandboxTemplateSpec(containers=[Container(name="a", image="i")]),
                options=TemplateBuildOptions(checkpoint=True),
            ).run_cmd("echo hi")

    def test_run_cmd_does_not_mutate_shared_options(self):
        shared = TemplateBuildOptions(checkpoint=True)
        spec = SandboxTemplateSpec(
            containers=[Container(name="a", image="i")],
            persistent_contents=[PersistentContent.FILESYSTEM],
        )
        t1 = Template(spec=spec, options=shared).run_cmd("cmd one")
        t2 = Template(spec=spec, options=shared)
        assert len(t1.options.run_commands) == 1
        assert len(t2.options.run_commands) == 0
        assert shared.run_commands == []

    def test_wait_until_ready_retries_transient_5xx(self, monkeypatch):
        get_responses = [
            httpx.Response(503, json={"message": "upstream unavailable"}),
            httpx.Response(503, json={"message": "upstream unavailable"}),
            httpx.Response(200, json={"name": "rt", "status": {"phase": "Ready"}}),
        ]
        attempts = []

        def handler(request: httpx.Request) -> httpx.Response:
            attempts.append(request.method)
            if request.method == "POST":
                return httpx.Response(201, json={"name": "rt", "status": {"phase": "Building"}})
            return get_responses.pop(0)

        monkeypatch.setattr("openkruise_agents.template.time.sleep", lambda _: None)
        build = Template.build(
            name="rt",
            spec=SandboxTemplateSpec(containers=[Container(name="main", image="i")]),
            client=make_sdk_client(handler),
        )
        result = build.wait_until_ready(timeout=5, interval=0)
        assert result.status is not None and result.status.value == "Ready"
        # POST + two retried 503 polls + final Ready poll
        assert attempts == ["POST", "GET", "GET", "GET"]

    def test_wait_until_ready_gives_up_after_consecutive_5xx(self, monkeypatch):
        def handler(request: httpx.Request) -> httpx.Response:
            if request.method == "POST":
                return httpx.Response(201, json={"name": "rt", "status": {"phase": "Building"}})
            return httpx.Response(503, json={"message": "upstream unavailable"})

        monkeypatch.setattr("openkruise_agents.template.time.sleep", lambda _: None)
        build = Template.build(
            name="rt",
            spec=SandboxTemplateSpec(containers=[Container(name="main", image="i")]),
            client=make_sdk_client(handler),
        )
        with pytest.raises(KruiseApiError) as exc_info:
            build.wait_until_ready(timeout=5, interval=0)
        assert exc_info.value.status_code == 503

    def test_wait_until_ready_non_retryable_error_raises_immediately(self, monkeypatch):
        polls = []

        def handler(request: httpx.Request) -> httpx.Response:
            if request.method == "POST":
                return httpx.Response(201, json={"name": "rt", "status": {"phase": "Building"}})
            polls.append("GET")
            return httpx.Response(404, json={"message": "template not found"})

        monkeypatch.setattr("openkruise_agents.template.time.sleep", lambda _: None)
        build = Template.build(
            name="rt",
            spec=SandboxTemplateSpec(containers=[Container(name="main", image="i")]),
            client=make_sdk_client(handler),
        )
        with pytest.raises(NotFoundError):
            build.wait_until_ready(timeout=5, interval=0)
        assert polls == ["GET"]

    def test_wait_until_ready_backoff_resets_after_success(self, monkeypatch):
        responses = iter(
            [
                httpx.Response(503, json={"message": "upstream unavailable"}),
                httpx.Response(200, json={"name": "rt", "status": {"phase": "Building"}}),
                httpx.Response(503, json={"message": "upstream unavailable"}),
                httpx.Response(200, json={"name": "rt", "status": {"phase": "Ready"}}),
            ]
        )
        sleeps = []

        def handler(request: httpx.Request) -> httpx.Response:
            if request.method == "POST":
                return httpx.Response(201, json={"name": "rt", "status": {"phase": "Building"}})
            return next(responses)

        monkeypatch.setattr("openkruise_agents.template.time.sleep", sleeps.append)
        build = Template.build(
            name="rt",
            spec=SandboxTemplateSpec(containers=[Container(name="main", image="i")]),
            client=make_sdk_client(handler),
        )
        build.wait_until_ready(timeout=60, interval=2)
        # After a successful poll the backoff restarts from the base
        # interval, so the second failure sleeps 2s again instead of 4s.
        assert sleeps == [2, 2, 2]

    def test_non_json_error_body_preserved(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(
                500,
                content=b"<html>502 Bad Gateway from nginx</html>",
                headers={"content-type": "text/html"},
            )

        with pytest.raises(KruiseApiError) as exc_info:
            Template.build(
                name="rt",
                spec=SandboxTemplateSpec(containers=[Container(name="main", image="i")]),
                client=make_sdk_client(handler),
            )
        assert "nginx" in exc_info.value.message

    def test_auth_and_permission_error_mapping(self):
        from openkruise_agents import AuthenticationError, PermissionDeniedError

        for status, exc_type in [(401, AuthenticationError), (403, PermissionDeniedError)]:
            def handler(request: httpx.Request, status=status) -> httpx.Response:
                return httpx.Response(status, json={"message": "denied"})

            with pytest.raises(exc_type):
                Template.build(
                    name="rt",
                    spec=SandboxTemplateSpec(containers=[Container(name="main", image="i")]),
                    client=make_sdk_client(handler),
                )

    def test_resource_name_url_encoded_in_get(self):
        seen_paths = []

        def handler(request: httpx.Request) -> httpx.Response:
            seen_paths.append(request.url.raw_path.decode())
            return httpx.Response(200, json={"name": "t", "status": {"phase": "Ready"}})

        build = Template.build(
            name="a:b/c",
            spec=SandboxTemplateSpec(containers=[Container(name="main", image="i")]),
            client=make_sdk_client(handler),
        )
        build.wait_until_ready(timeout=1, interval=0)
        assert seen_paths[-1].endswith("/templates/a%3Ab%2Fc")

    def test_on_build_logs_invoked(self):
        logs = []

        def handler(request: httpx.Request) -> httpx.Response:
            if request.method == "POST":
                return httpx.Response(
                    201,
                    json={
                        "name": "rt",
                        "status": {"phase": "Building"},
                        "buildLogs": ["step 1", "step 2"],
                    },
                )
            return httpx.Response(200, json={"name": "rt", "status": {"phase": "Ready"}})

        build = Template.build(
            name="rt",
            spec=SandboxTemplateSpec(containers=[Container(name="main", image="i")]),
            client=make_sdk_client(handler),
            on_build_logs=logs.append,
        )
        assert logs == ["step 1", "step 2"]
        build.wait_until_ready(timeout=1, interval=0)


class TestTemplateGetDelete:
    def test_get_returns_handle_with_parsed_status(self):
        def handler(request: httpx.Request) -> httpx.Response:
            assert request.method == "GET"
            assert request.url.path == f"{API_PREFIX}/templates/code-interpreter"
            assert request.headers["X-API-Key"] == "test-key"
            return httpx.Response(
                200,
                json={
                    "name": "code-interpreter",
                    "status": {
                        "phase": "Ready",
                        "artifactRef": {"backend": "kubernetes"},
                    },
                },
            )

        handle = Template.get("code-interpreter", client=make_sdk_client(handler))
        assert handle.template_id == "code-interpreter"
        assert handle.status is SandboxTemplatePhase.READY
        assert handle.artifact is not None
        assert handle.artifact.backend == "kubernetes"

    def test_get_missing_template_raises_not_found(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(404, json={"message": "not found"})

        with pytest.raises(NotFoundError):
            Template.get("missing", client=make_sdk_client(handler))

    def test_get_url_encodes_name(self):
        seen_paths = []

        def handler(request: httpx.Request) -> httpx.Response:
            seen_paths.append(request.url.raw_path.decode())
            return httpx.Response(200, json={"name": "t", "status": {"phase": "Building"}})

        handle = Template.get("a:b/c", client=make_sdk_client(handler))
        assert seen_paths[-1].endswith("/templates/a%3Ab%2Fc")
        assert handle.status is SandboxTemplatePhase.BUILDING

    def test_delete_sends_delete_and_accepts_204(self):
        seen: list[tuple[str, str]] = []

        def handler(request: httpx.Request) -> httpx.Response:
            seen.append((request.method, request.url.path))
            return httpx.Response(204)

        Template.delete("old-template", client=make_sdk_client(handler))
        assert seen == [("DELETE", f"{API_PREFIX}/templates/old-template")]

    def test_delete_referenced_template_raises_conflict(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(
                409,
                json={"message": "template referenced by sandboxsets: ['code-interpreter']"},
            )

        with pytest.raises(ConflictError):
            Template.delete("referenced", client=make_sdk_client(handler))


class TestSandboxSet:
    def test_create_body_only_has_template_ref_and_pool_fields(self):
        captured = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["body"] = json.loads(request.content)
            return httpx.Response(
                201, json={"name": "python-runtime-pool", "spec": {"templateRef": "python-runtime", "replicas": 5}}
            )

        pool = SandboxSet.create(
            name="python-runtime-pool",
            template_ref="python-runtime",
            spec=SandboxSetSpec(replicas=5),
            client=make_sdk_client(handler),
        )
        body = captured["body"]
        assert set(body.keys()) == {"name", "spec"}
        assert body["spec"] == {"templateRef": "python-runtime", "replicas": 5}
        assert pool.name == "python-runtime-pool"
        assert pool.resource.replicas == 5

    def test_create_requires_template_ref(self):
        with pytest.raises(ValueError):
            SandboxSet.create(name="p", template_ref="", client=make_sdk_client(lambda r: httpx.Response(201, json={})))

    def test_wait_until_ready(self, monkeypatch):
        states = iter([{"ready": False}, {"ready": True}])

        def handler(request: httpx.Request) -> httpx.Response:
            if request.method == "POST":
                return httpx.Response(201, json={"name": "p"})
            state = next(states)
            return httpx.Response(200, json={"name": "p", "ready": state["ready"], "replicas": 5})

        monkeypatch.setattr("openkruise_agents.sandboxset.time.sleep", lambda _: None)
        pool = SandboxSet.create(
            name="p", template_ref="t", spec=SandboxSetSpec(replicas=5), client=make_sdk_client(handler)
        )
        pool.wait_until_ready(timeout=5, interval=0)
        assert pool.ready is True

    def test_get_returns_resource_and_encodes_name(self):
        seen_paths = []

        def handler(request: httpx.Request) -> httpx.Response:
            seen_paths.append(request.url.raw_path.decode())
            return httpx.Response(
                200,
                json={"name": "a:b", "spec": {"templateRef": "t", "replicas": 3}, "ready": True},
            )

        pool = SandboxSet.get("a:b", client=make_sdk_client(handler))
        assert pool.name == "a:b"
        assert pool.resource.template_ref == "t"
        assert pool.resource.replicas == 3
        assert pool.ready is True
        assert seen_paths == [f"{API_PREFIX}/sandboxsets/a%3Ab"]

    def test_from_api_merges_nested_spec_shape(self):
        from openkruise_agents.models import SandboxSetResource, PoolAutoscalerResource

        # CR-shaped response: pool fields nested under spec.
        cr_shaped = {
            "name": "p",
            "spec": {"templateRef": "t", "replicas": 7, "scaleStrategy": {"maxUnavailable": "10%"}},
            "ready": True,
        }
        resource = SandboxSetResource.from_api(cr_shaped)
        assert resource.template_ref == "t"
        assert resource.replicas == 7
        assert resource.ready is True

        # Flat projection response also accepted.
        flat = {"name": "p", "templateRef": "t", "replicas": 7}
        assert SandboxSetResource.from_api(flat).replicas == 7

        # Fallback name when the response carries none.
        assert SandboxSetResource.from_api({"replicas": 1}, fallback_name="fb").name == "fb"

        pa_resource = PoolAutoscalerResource.from_api(
            {"name": "pa", "spec": {"suspend": True}, "active": True}
        )
        assert pa_resource.suspend is True
        assert pa_resource.active is True

    def test_scale_strategy_serializes_in_spec(self):
        from openkruise_agents.models import SandboxSetScaleStrategy

        captured = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["body"] = json.loads(request.content)
            return httpx.Response(201, json={"name": "p"})

        pool = SandboxSet.create(
            name="p",
            template_ref="t",
            spec=SandboxSetSpec(
                replicas=10,
                scale_strategy=SandboxSetScaleStrategy(max_unavailable="10%"),
            ),
            client=make_sdk_client(handler),
        )
        assert pool.name == "p"
        assert captured["body"]["spec"]["scaleStrategy"] == {"maxUnavailable": "10%"}

    def test_create_omits_unset_replicas(self):
        captured = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["body"] = json.loads(request.content)
            return httpx.Response(201, json={"name": "p"})

        SandboxSet.create(name="p", template_ref="t", client=make_sdk_client(handler))
        assert captured["body"]["spec"] == {"templateRef": "t"}

    def test_create_does_not_mutate_caller_spec(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(201, json={"name": "p"})

        spec = SandboxSetSpec(replicas=3)
        SandboxSet.create(name="p", template_ref="t", spec=spec, client=make_sdk_client(handler))
        assert spec.template_ref is None
        assert spec.replicas == 3

    def test_backend_validation_error_maps_to_bad_request(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(400, json={"message": "spec.containers[0].env is not supported on substrate"})

        with pytest.raises(BadRequestError) as exc_info:
            SandboxSet.create(name="p", template_ref="t", client=make_sdk_client(handler))
        assert "env" in exc_info.value.message


class TestPoolAutoscaler:
    def test_create_body(self):
        captured = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["body"] = json.loads(request.content)
            return httpx.Response(201, json={"name": "pa", "active": True})

        pa = PoolAutoscaler.create(
            name="pa",
            scale_target_ref="pool",
            spec=PoolAutoscalerSpec(
                min_replicas=1,
                max_replicas=50,
                capacity_policy=CapacityPolicy(
                    target_available=5,
                    tolerance="10%",
                    scale_up=CapacityScalingRules(stabilization_window_seconds=60),
                    scale_down=CapacityScalingRules(stabilization_window_seconds=300),
                ),
                cron_policies=[],
                suspend=False,
            ),
            client=make_sdk_client(handler),
        )
        body = captured["body"]
        assert body["name"] == "pa"
        assert body["scaleTargetRef"] == "pool"
        assert body["spec"]["minReplicas"] == 1
        assert body["spec"]["capacityPolicy"]["targetAvailable"] == 5
        assert body["spec"]["capacityPolicy"]["scaleUp"]["stabilizationWindowSeconds"] == 60
        assert pa.active is True

    def test_get_returns_resource(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(
                200,
                json={"name": "pa", "spec": {"suspend": False}, "active": True},
            )

        pa = PoolAutoscaler.get("pa", client=make_sdk_client(handler))
        assert pa.name == "pa"
        assert pa.active is True

    def test_cron_policies_typed(self):
        from openkruise_agents.models import CronScalingPolicy

        captured = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["body"] = json.loads(request.content)
            return httpx.Response(201, json={"name": "pa"})

        PoolAutoscaler.create(
            name="pa",
            scale_target_ref="pool",
            spec=PoolAutoscalerSpec(
                cron_policies=[
                    CronScalingPolicy(
                        name="work-hours",
                        schedule="0 9 * * 1-5",
                        target_replicas=20,
                        time_zone="Asia/Shanghai",
                    )
                ],
            ),
            client=make_sdk_client(handler),
        )
        policy = captured["body"]["spec"]["cronPolicies"][0]
        assert policy == {
            "name": "work-hours",
            "schedule": "0 9 * * 1-5",
            "targetReplicas": 20,
            "timeZone": "Asia/Shanghai",
        }

    def test_substrate_target_unsupported(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(501, json={"message": "poolautoscaler is not implemented for substrate"})

        with pytest.raises(UnsupportedOperationError):
            PoolAutoscaler.create(name="pa", scale_target_ref="pool", client=make_sdk_client(handler))

    def test_wait_until_active(self, monkeypatch):
        states = iter([{"active": False}, {"active": True}])

        def handler(request: httpx.Request) -> httpx.Response:
            if request.method == "POST":
                return httpx.Response(201, json={"name": "pa"})
            return httpx.Response(200, json={"name": "pa", "active": next(states)["active"]})

        monkeypatch.setattr("openkruise_agents.poolautoscaler.time.sleep", lambda _: None)
        pa = PoolAutoscaler.create(name="pa", scale_target_ref="pool", client=make_sdk_client(handler))
        pa.wait_until_active(timeout=5, interval=0)
        assert pa.active is True
