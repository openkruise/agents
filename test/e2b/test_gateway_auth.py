"""Tests for sandbox-gateway access token authentication."""
import logging
from uuid import UUID

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
    assert get_sandbox_access_token(sandbox_id) == access_token
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
