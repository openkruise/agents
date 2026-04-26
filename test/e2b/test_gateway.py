"""Tests for sandbox-gateway routing methods (header-based and host-based)."""
import time

import requests
from e2b_code_interpreter import Sandbox


GATEWAY_URL = "http://localhost:80"


def test_gateway_header_based_routing(sandbox_context):
    """Test routing via e2b-sandbox-id and e2b-sandbox-port headers."""
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
    resp = requests.get(
        f"{GATEWAY_URL}/",
        headers={
            "e2b-sandbox-id": sandbox_id,
            "e2b-sandbox-port": "49983",
        },
        timeout=10,
    )
    assert resp.status_code != 502, f"Gateway 502: sandbox {sandbox_id} not found or not running"


def test_gateway_host_based_routing(sandbox_context):
    """Test routing via native E2B host header format: {port}-{sandboxID}.{domain}."""
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
    # Native E2B host format: {port}-{namespace}--{name}.example.com
    host = f"49983-{sandbox_id}.example.com"
    resp = requests.get(
        f"{GATEWAY_URL}/",
        headers={"Host": host},
        timeout=10,
    )
    assert resp.status_code != 502, f"Gateway 502: sandbox {sandbox_id} not found or not running"
