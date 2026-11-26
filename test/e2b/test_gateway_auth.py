"""Tests for sandbox-gateway access token authentication."""
import json
import logging
import time
from uuid import UUID

import conftest_base
import pytest
import requests
from e2b_code_interpreter import Sandbox
from gateway_utils import (
    WORKLOAD_PORT,
    assert_workload_reached,
    gateway_request,
    gateway_request_eventually,
    get_sandbox_access_token,
    get_sandbox_resource,
    start_workload_server,
)
from utils import kubectl

logger = logging.getLogger(__name__)
pytestmark = pytest.mark.gateway_uuid_auth


@pytest.mark.parametrize(
    "skip_init_runtime",
    [
        pytest.param(False, id="runtime-init"),
        pytest.param(True, id="skip-init-runtime"),
    ],
)
def test_gateway_uuid_auth(sandbox_context, config, skip_init_runtime: bool):
    """Verify UUID authentication for header-based and host-based routing."""
    sandbox: Sandbox = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=120,
            metadata={
                "e2b.agents.kruise.io/skip-init-runtime": str(skip_init_runtime).lower(),
            },
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    sandbox_id = sandbox.sandbox_id
    logger.info("sandbox-id: %s", sandbox_id)

    access_token = sandbox._envd_access_token
    assert access_token, "CreateSandbox should return an access token"
    assert UUID(access_token).version == 4
    token_matches_annotation = get_sandbox_access_token(sandbox_id) == access_token
    assert token_matches_annotation, "Returned access token does not match the annotation"
    if skip_init_runtime:
        annotations = get_sandbox_resource(sandbox_id)["metadata"].get("annotations", {})
        assert "agents.kruise.io/init-runtime-request" not in annotations

    base_headers = {
        "e2b-sandbox-id": sandbox_id,
        "e2b-sandbox-port": "49983",
    }
    valid_response = gateway_request_eventually(
        config, sandbox_id, access_token
    )
    assert valid_response.status_code not in (401, 502, 503), (
        f"Gateway rejected a valid UUID token: {valid_response.status_code}"
    )

    missing_response = requests.get(
        f"{config.gateway_url}/",
        headers=base_headers,
        timeout=10,
    )
    assert missing_response.status_code == 401, (
        f"Expected 401 without UUID token, got {missing_response.status_code}"
    )

    invalid_response = requests.get(
        f"{config.gateway_url}/",
        headers={**base_headers, "X-Access-Token": "wrong-token-value"},
        timeout=10,
    )
    assert invalid_response.status_code == 401, (
        f"Expected 401 with invalid UUID token, got {invalid_response.status_code}"
    )

    host = f"49983-{sandbox_id}.{config.e2b_domain}"
    host_missing_response = requests.get(
        f"{config.gateway_url}/",
        headers={"Host": host},
        timeout=10,
    )
    assert host_missing_response.status_code == 401, (
        f"Expected 401 without UUID token via host routing, got {host_missing_response.status_code}"
    )

    host_valid_response = requests.get(
        f"{config.gateway_url}/",
        headers={"Host": host, "X-Access-Token": access_token},
        timeout=10,
    )
    assert host_valid_response.status_code not in (401, 502, 503), (
        f"Gateway rejected valid UUID token via host routing: {host_valid_response.status_code}"
    )

    # Repeat the matrix on a port served by the workload instead of
    # agent-runtime. agent-runtime authenticates x-access-token on its own, so a
    # 401 on the runtime port cannot be attributed to the gateway; on this port
    # nothing else authenticates, so the rejections below are unambiguously the
    # gateway's and a 200 with the fixed body proves end-to-end reach.
    start_workload_server(sandbox)
    assert_workload_reached(
        gateway_request_eventually(
            config, sandbox_id, access_token, None, port=WORKLOAD_PORT
        )
    )

    workload_missing = gateway_request(
        config, sandbox_id, None, None, port=WORKLOAD_PORT
    )
    assert workload_missing.status_code == 401, (
        f"Expected 401 without UUID token on the workload port, "
        f"got {workload_missing.status_code}: {workload_missing.text}"
    )

    workload_invalid = gateway_request(
        config, sandbox_id, "wrong-token-value", None, port=WORKLOAD_PORT
    )
    assert workload_invalid.status_code == 401, (
        f"Expected 401 with invalid UUID token on the workload port, "
        f"got {workload_invalid.status_code}: {workload_invalid.text}"
    )

    workload_host = f"{WORKLOAD_PORT}-{sandbox_id}.{config.e2b_domain}"
    workload_host_missing = requests.get(
        f"{config.gateway_url}/",
        headers={"Host": workload_host},
        timeout=10,
    )
    assert workload_host_missing.status_code == 401, (
        f"Expected 401 without UUID token via host routing on the workload port, "
        f"got {workload_host_missing.status_code}"
    )

    workload_host_invalid = requests.get(
        f"{config.gateway_url}/",
        headers={"Host": workload_host, "X-Access-Token": "wrong-token-value"},
        timeout=10,
    )
    assert workload_host_invalid.status_code == 401, (
        f"Expected 401 with invalid UUID token via host routing on the workload port, "
        f"got {workload_host_invalid.status_code}"
    )

    workload_host_valid = requests.get(
        f"{config.gateway_url}/",
        headers={"Host": workload_host, "X-Access-Token": access_token},
        timeout=10,
    )
    assert_workload_reached(workload_host_valid)


@pytest.mark.parametrize(
    "route_token",
    [
        pytest.param(None, id="missing-route-token"),
        pytest.param("", id="empty-route-token"),
    ],
)
@pytest.mark.parametrize(
    "host_routing",
    [pytest.param(False, id="header-routing"), pytest.param(True, id="host-routing")],
)
def test_gateway_uuid_auth_rejects_tokenless_route(
    sandbox_context: conftest_base.SandboxContext,
    config: conftest_base.TestConfig,
    route_token: str | None,
    host_routing: bool,
) -> None:
    # Given a reachable workload that does not authenticate requests itself.
    sandbox = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=120,
            metadata={"e2b.agents.kruise.io/skip-init-runtime": "true"},
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    sandbox_id = sandbox.sandbox_id
    access_token = sandbox._envd_access_token
    assert access_token, "CreateSandbox should return an access token"
    ready = gateway_request_eventually(config, sandbox_id, access_token)
    assert ready.status_code != 401, ready.text
    start_workload_server(sandbox)
    routing_headers = (
        {"Host": f"{WORKLOAD_PORT}-{sandbox_id}.{config.e2b_domain}"}
        if host_routing
        else {"e2b-sandbox-id": sandbox_id, "e2b-sandbox-port": str(WORKLOAD_PORT)}
    )
    assert_workload_reached(
        requests.get(
            f"{config.gateway_url}/",
            headers={**routing_headers, "x-access-token": access_token},
            timeout=10,
        )
    )

    # When both credential annotations resolve to an empty route token.
    resource = get_sandbox_resource(sandbox_id)
    kubectl(
        "patch", "sandbox", resource["metadata"]["name"],
        "-n", resource["metadata"]["namespace"],
        "--type=merge", "-p", json.dumps({
            "metadata": {"annotations": {
                "agents.kruise.io/runtime-access-token": route_token,
                "e2b.agents.kruise.io/envd-access-token": None,
            }},
        }),
    )

    # Then even the formerly valid token is rejected once the registry catches up.
    deadline = time.monotonic() + 30
    responses: list[requests.Response] = []
    while time.monotonic() < deadline:
        responses = []
        for request_token in (None, "", "wrong-token-value", access_token):
            headers = dict(routing_headers)
            if request_token is not None:
                headers["x-access-token"] = request_token
            responses.append(requests.get(
                f"{config.gateway_url}/", headers=headers, timeout=10,
            ))
        if all(response.status_code == 401 for response in responses):
            break
        time.sleep(0.5)
    assert len(responses) == 4
    for response in responses:
        assert response.status_code == 401, response.text
        assert response.text == "unauthorized: invalid or missing access token"


@pytest.mark.parametrize("initial_status", [401, 502, 503])
def test_gateway_readiness_retries_stale_routes(monkeypatch, initial_status: int) -> None:
    stale = requests.Response()
    stale.status_code = initial_status
    ready = requests.Response()
    ready.status_code = 200
    responses = iter((stale, ready))
    monkeypatch.setattr("gateway_utils.gateway_request", lambda *args: next(responses))
    monkeypatch.setattr("gateway_utils.time.sleep", lambda seconds: None)

    response = gateway_request_eventually(None, "sandbox-id", "valid-token")

    assert response is ready


def test_gateway_readiness_preserves_jwt_rejection(monkeypatch) -> None:
    rejected = requests.Response()
    rejected.status_code = 403
    monkeypatch.setattr("gateway_utils.gateway_request", lambda *args: rejected)

    response = gateway_request_eventually(None, "sandbox-id", "runtime-token", "bad-jwt")

    assert response is rejected
