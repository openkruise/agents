"""Tests for sandbox-gateway routing methods (header-based and host-based)."""
import json
import subprocess
import time

import requests
from e2b_code_interpreter import Sandbox


GATEWAY_URL = "http://localhost:80"


def get_sandbox_access_token(sandbox_id: str) -> str:
    """Retrieve the runtime-access-token annotation from a Sandbox CR via kubectl."""
    name = sandbox_id.split("--")[1] if "--" in sandbox_id else sandbox_id
    result = subprocess.run(
        ["kubectl", "get", "sandbox", name, "-o", "json"],
        capture_output=True,
        text=True,
        check=True,
    )
    sbx = json.loads(result.stdout)
    annotations = sbx.get("metadata", {}).get("annotations", {})
    return annotations.get("agents.kruise.io/runtime-access-token", "")


def test_gateway_header_based_routing(sandbox_context):
    """Test routing via e2b-sandbox-id and e2b-sandbox-port headers."""
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=300,
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    sandbox_id = sandbox.sandbox_id
    print(f"sandbox-id: {sandbox_id}")

    # Wait for gateway registry to sync
    time.sleep(3)

    # Retrieve access token for authentication
    access_token = get_sandbox_access_token(sandbox_id)

    headers = {
        "e2b-sandbox-id": sandbox_id,
        "e2b-sandbox-port": "49983",
    }
    if access_token:
        headers["X-Access-Token"] = access_token

    resp = requests.get(
        f"{GATEWAY_URL}/",
        headers=headers,
        timeout=10,
    )
    assert resp.status_code != 502, f"Gateway 502: sandbox {sandbox_id} not found or not running"
    assert resp.status_code != 401, f"Gateway 401: access token mismatch for sandbox {sandbox_id}"


def test_gateway_host_based_routing(sandbox_context):
    """Test routing via native E2B host header format: {port}-{sandboxID}.{domain}."""
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=300,
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    sandbox_id = sandbox.sandbox_id
    print(f"sandbox-id: {sandbox_id}")

    # Wait for gateway registry to sync
    time.sleep(3)

    # Retrieve access token for authentication
    access_token = get_sandbox_access_token(sandbox_id)

    # Native E2B host format: {port}-{namespace}--{name}.example.com
    host = f"49983-{sandbox_id}.example.com"
    headers = {"Host": host}
    if access_token:
        headers["X-Access-Token"] = access_token

    resp = requests.get(
        f"{GATEWAY_URL}/",
        headers=headers,
        timeout=10,
    )
    assert resp.status_code != 502, f"Gateway 502: sandbox {sandbox_id} not found or not running"
    assert resp.status_code != 401, f"Gateway 401: access token mismatch for sandbox {sandbox_id}"
