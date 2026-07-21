"""E2E tests for sandbox-gateway TrafficAccessToken JWT authentication."""

import os
import shlex
import subprocess
import time

import pytest
import requests
from e2b_code_interpreter import Sandbox

from gateway_utils import get_sandbox_access_token, get_sandbox_uid


TOKEN_COMMAND = os.environ.get("JWT_E2E_TOKEN_COMMAND", "")
JWT_AUTH_METADATA_KEY = "security.agents.kruise.io/enable-jwt-auth"
pytestmark = [
    pytest.mark.jwt_auth,
    pytest.mark.skipif(
        os.environ.get("TRAFFIC_ACCESS_TOKEN_JWT_E2E", "").lower() != "true"
        or not TOKEN_COMMAND,
        reason="requires a JWT-enabled gateway and a token issuer command",
    ),
]


def issue_traffic_access_token(sandbox_id, sandbox_uid, expired=False):
    command = shlex.split(TOKEN_COMMAND) + [
        "--sandbox-id",
        sandbox_id,
        "--sandbox-uid",
        sandbox_uid,
    ]
    if expired:
        command.append("--expired")
    result = subprocess.run(
        command,
        capture_output=True,
        text=True,
        check=True,
    )
    token = result.stdout.strip()
    assert token, "token issuer command returned an empty token"
    return token


def gateway_request(
    config, sandbox_id, runtime_access_token, traffic_access_token=None
):
    headers = {
        "e2b-sandbox-id": sandbox_id,
        "e2b-sandbox-port": "49983",
        "x-access-token": runtime_access_token,
    }
    if traffic_access_token is not None:
        headers["x-traffic-access-token"] = traffic_access_token
    return requests.get(f"{config.gateway_url}/", headers=headers, timeout=10)


def gateway_request_eventually(
    config, sandbox_id, runtime_access_token, traffic_access_token
):
    deadline = time.monotonic() + 30
    response = None
    while time.monotonic() < deadline:
        response = gateway_request(
            config, sandbox_id, runtime_access_token, traffic_access_token
        )
        if response.status_code not in (502, 503):
            return response
        time.sleep(0.5)
    raise AssertionError(
        f"gateway route was not ready for {sandbox_id}: "
        f"{response.status_code if response is not None else 'no response'} "
        f"{response.text if response is not None else ''}"
    )


def test_gateway_traffic_access_token_jwt(sandbox_context, config):
    """Verify route-selective JWT authentication and token validation."""
    first: Sandbox = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=120,
            metadata={JWT_AUTH_METADATA_KEY: "true"},
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    second: Sandbox = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=120,
            metadata={JWT_AUTH_METADATA_KEY: "true"},
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    public: Sandbox = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=120,
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    first_token = issue_traffic_access_token(
        first.sandbox_id, get_sandbox_uid(first.sandbox_id)
    )
    first_runtime_token = get_sandbox_access_token(first.sandbox_id)
    second_runtime_token = get_sandbox_access_token(second.sandbox_id)
    public_runtime_token = get_sandbox_access_token(public.sandbox_id)
    assert first_runtime_token, "first Sandbox is missing its runtime access token"
    assert second_runtime_token, "second Sandbox is missing its runtime access token"
    assert public_runtime_token, "public Sandbox is missing its runtime access token"

    public_response = gateway_request_eventually(
        config, public.sandbox_id, public_runtime_token, None
    )
    assert public_response.status_code in (200, 404), public_response.text

    valid = gateway_request_eventually(
        config, first.sandbox_id, first_runtime_token, first_token
    )
    assert valid.status_code in (200, 404), valid.text

    missing = gateway_request(config, first.sandbox_id, first_runtime_token)
    assert missing.status_code == 401, missing.text

    malformed = gateway_request(
        config, first.sandbox_id, first_runtime_token, "not-a-jwt"
    )
    assert malformed.status_code == 401, malformed.text

    expired_token = issue_traffic_access_token(
        first.sandbox_id, get_sandbox_uid(first.sandbox_id), expired=True
    )
    expired = gateway_request(
        config, first.sandbox_id, first_runtime_token, expired_token
    )
    assert expired.status_code == 401, expired.text

    second_ready = gateway_request_eventually(
        config, second.sandbox_id, second_runtime_token, "not-a-jwt"
    )
    assert second_ready.status_code == 401, second_ready.text

    replayed = gateway_request(
        config, second.sandbox_id, second_runtime_token, first_token
    )
    assert replayed.status_code == 401, replayed.text
