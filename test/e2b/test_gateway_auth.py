"""Tests for sandbox-gateway access token authentication."""
import json
import subprocess
import time

import requests
from e2b_code_interpreter import Sandbox


GATEWAY_URL = "http://localhost:80"


def get_sandbox_access_token(sandbox_id: str) -> str:
    """Retrieve the runtime-access-token annotation from a Sandbox CR via kubectl.

    Args:
        sandbox_id: The sandbox ID in namespace--name format (e.g., default--my-sandbox).

    Returns:
        The access token string, or empty string if not set.
    """
    # sandbox_id format is "namespace--name", extract both parts
    if "--" in sandbox_id:
        parts = sandbox_id.split("--")
        namespace = parts[0]
        name = parts[1]
    else:
        namespace = "default"
        name = sandbox_id
    result = subprocess.run(
        ["kubectl", "get", "sandbox", name, "-n", namespace, "-o", "json"],
        capture_output=True,
        text=True,
        check=True,
    )
    sbx = json.loads(result.stdout)
    annotations = sbx.get("metadata", {}).get("annotations", {})
    return annotations.get("agents.kruise.io/runtime-access-token", "")


def test_gateway_auth_valid_token(sandbox_context):
    """Test that request with valid X-Access-Token header is forwarded successfully."""
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=120,
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    sandbox_id = sandbox.sandbox_id
    print(f"sandbox-id: {sandbox_id}")

    # Wait for gateway registry to sync
    time.sleep(3)

    # Retrieve the access token from the Sandbox CR annotation
    access_token = get_sandbox_access_token(sandbox_id)
    print(f"access-token: {access_token[:8]}..." if access_token else "access-token: (empty)")
    assert access_token != "", "Sandbox should have a runtime-access-token annotation"

    # Request with valid token should succeed (not 401 or 502)
    resp = requests.get(
        f"{GATEWAY_URL}/",
        headers={
            "e2b-sandbox-id": sandbox_id,
            "e2b-sandbox-port": "49983",
            "X-Access-Token": access_token,
        },
        timeout=10,
    )
    assert resp.status_code != 401, (
        f"Gateway returned 401 with valid token for sandbox {sandbox_id}"
    )
    assert resp.status_code != 502, (
        f"Gateway returned 502: sandbox {sandbox_id} not found or not running"
    )


def test_gateway_auth_missing_token(sandbox_context):
    """Test that request without X-Access-Token header returns 401."""
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=120,
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    sandbox_id = sandbox.sandbox_id
    print(f"sandbox-id: {sandbox_id}")

    # Wait for gateway registry to sync
    time.sleep(3)

    # Verify sandbox has an access token configured
    access_token = get_sandbox_access_token(sandbox_id)
    assert access_token != "", "Sandbox should have a runtime-access-token annotation"

    # Request without token should be rejected with 401
    resp = requests.get(
        f"{GATEWAY_URL}/",
        headers={
            "e2b-sandbox-id": sandbox_id,
            "e2b-sandbox-port": "49983",
        },
        timeout=10,
    )
    assert resp.status_code == 401, (
        f"Expected 401 without token, got {resp.status_code} for sandbox {sandbox_id}"
    )


def test_gateway_auth_invalid_token(sandbox_context):
    """Test that request with wrong X-Access-Token header returns 401."""
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=120,
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    sandbox_id = sandbox.sandbox_id
    print(f"sandbox-id: {sandbox_id}")

    # Wait for gateway registry to sync
    time.sleep(3)

    # Verify sandbox has an access token configured
    access_token = get_sandbox_access_token(sandbox_id)
    assert access_token != "", "Sandbox should have a runtime-access-token annotation"

    # Request with wrong token should be rejected with 401
    resp = requests.get(
        f"{GATEWAY_URL}/",
        headers={
            "e2b-sandbox-id": sandbox_id,
            "e2b-sandbox-port": "49983",
            "X-Access-Token": "wrong-token-value",
        },
        timeout=10,
    )
    assert resp.status_code == 401, (
        f"Expected 401 with invalid token, got {resp.status_code} for sandbox {sandbox_id}"
    )


def test_gateway_auth_host_based_routing_with_token(sandbox_context):
    """Test that host-based routing also enforces access token authentication."""
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=120,
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    sandbox_id = sandbox.sandbox_id
    print(f"sandbox-id: {sandbox_id}")

    # Wait for gateway registry to sync
    time.sleep(3)

    # Retrieve the access token
    access_token = get_sandbox_access_token(sandbox_id)
    assert access_token != "", "Sandbox should have a runtime-access-token annotation"

    host = f"49983-{sandbox_id}.example.com"

    # Without token -> 401
    resp_no_token = requests.get(
        f"{GATEWAY_URL}/",
        headers={"Host": host},
        timeout=10,
    )
    assert resp_no_token.status_code == 401, (
        f"Expected 401 without token via host routing, got {resp_no_token.status_code}"
    )

    # With valid token -> success
    resp_valid = requests.get(
        f"{GATEWAY_URL}/",
        headers={
            "Host": host,
            "X-Access-Token": access_token,
        },
        timeout=10,
    )
    assert resp_valid.status_code != 401, (
        f"Gateway returned 401 with valid token via host routing for sandbox {sandbox_id}"
    )
    assert resp_valid.status_code != 502, (
        f"Gateway returned 502 via host routing: sandbox {sandbox_id} not found or not running"
    )
