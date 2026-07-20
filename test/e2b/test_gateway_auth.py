"""Tests for sandbox-gateway access token authentication."""
import logging
import time

import pytest
import requests
from e2b_code_interpreter import Sandbox

from gateway_utils import get_sandbox_access_token

logger = logging.getLogger(__name__)
pytestmark = pytest.mark.gateway_uuid_auth


def test_gateway_uuid_auth(sandbox_context, config):
    """Verify UUID authentication for header-based and host-based routing."""
    sandbox: Sandbox = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=120,
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    sandbox_id = sandbox.sandbox_id
    logger.info("sandbox-id: %s", sandbox_id)

    # Wait for gateway registry to sync
    time.sleep(3)

    access_token = get_sandbox_access_token(sandbox_id)
    assert access_token != "", "Sandbox should have a runtime-access-token annotation"

    base_headers = {
        "e2b-sandbox-id": sandbox_id,
        "e2b-sandbox-port": "49983",
    }
    valid_response = requests.get(
        f"{config.gateway_url}/",
        headers={**base_headers, "X-Access-Token": access_token},
        timeout=10,
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
