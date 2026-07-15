"""E2E tests for sandbox-gateway TrafficAccessToken JWT authentication."""

import json
import os
import shlex
import subprocess
import time

import pytest
import requests
from e2b_code_interpreter import Sandbox


TOKEN_COMMAND = os.environ.get("JWT_E2E_TOKEN_COMMAND", "")
pytestmark = pytest.mark.skipif(
    os.environ.get("TRAFFIC_ACCESS_TOKEN_JWT_E2E", "").lower() != "true"
    or not TOKEN_COMMAND,
    reason="requires a JWT-enabled gateway and an external token issuer command",
)


def get_sandbox_uid(sandbox_id):
    namespace, name = sandbox_id.split("--", 1)
    result = subprocess.run(
        ["kubectl", "get", "sandbox", name, "-n", namespace, "-o", "json"],
        capture_output=True,
        text=True,
        check=True,
    )
    return json.loads(result.stdout)["metadata"]["uid"]


def issue_traffic_access_token(sandbox_id, sandbox_uid):
    environment = os.environ.copy()
    environment["JWT_E2E_SANDBOX_ID"] = sandbox_id
    environment["JWT_E2E_SANDBOX_UID"] = sandbox_uid
    result = subprocess.run(
        shlex.split(TOKEN_COMMAND),
        capture_output=True,
        text=True,
        check=True,
        env=environment,
    )
    token = result.stdout.strip()
    assert token, "external token issuer command returned an empty token"
    return token


def gateway_request(config, sandbox_id, traffic_access_token=None):
    headers = {
        "e2b-sandbox-id": sandbox_id,
        "e2b-sandbox-port": "49983",
        # UUID authentication must be bypassed while JWT mode is enabled.
        "x-access-token": "deliberately-invalid-uuid-token",
    }
    if traffic_access_token is not None:
        headers["x-traffic-access-token"] = traffic_access_token
    return requests.get(f"{config.gateway_url}/", headers=headers, timeout=10)


def gateway_request_eventually(config, sandbox_id, traffic_access_token):
    deadline = time.monotonic() + 30
    response = None
    while time.monotonic() < deadline:
        response = gateway_request(config, sandbox_id, traffic_access_token)
        if response.status_code not in (502, 503):
            return response
        time.sleep(0.5)
    raise AssertionError(
        f"gateway route was not ready for {sandbox_id}: "
        f"{response.status_code if response is not None else 'no response'} "
        f"{response.text if response is not None else ''}"
    )


def test_gateway_traffic_access_token_jwt(sandbox_context, config):
    """Verify valid, missing, malformed, and cross-sandbox JWT behavior."""
    first: Sandbox = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=120,
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    second: Sandbox = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=120,
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    first_token = issue_traffic_access_token(
        first.sandbox_id, get_sandbox_uid(first.sandbox_id)
    )

    valid = gateway_request_eventually(config, first.sandbox_id, first_token)
    assert valid.status_code in (200, 404), valid.text

    missing = gateway_request(config, first.sandbox_id)
    assert missing.status_code == 401, missing.text

    malformed = gateway_request(config, first.sandbox_id, "not-a-jwt")
    assert malformed.status_code == 401, malformed.text

    second_ready = gateway_request_eventually(config, second.sandbox_id, "not-a-jwt")
    assert second_ready.status_code == 401, second_ready.text

    replayed = gateway_request(config, second.sandbox_id, first_token)
    assert replayed.status_code == 401, replayed.text
