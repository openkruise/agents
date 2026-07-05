"""Tests for sandbox-gateway routing methods (header-based and host-based)."""
import json
import subprocess
import time

import requests
from e2b_code_interpreter import Sandbox

import logging

logger = logging.getLogger(__name__)


def get_sandbox_access_token(sandbox_id: str) -> str:
    """Retrieve the runtime-access-token annotation from a Sandbox CR via kubectl."""
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


def test_gateway_header_based_routing(sandbox_context, config):
    """Test routing via e2b-sandbox-id and e2b-sandbox-port headers."""
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=120,
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    sandbox_id = sandbox.sandbox_id
    logger.info("sandbox-id: %s", sandbox_id)

    # Wait for gateway registry to sync
    time.sleep(3)

    access_token = get_sandbox_access_token(sandbox_id)

    headers = {
        "e2b-sandbox-id": sandbox_id,
        "e2b-sandbox-port": "49983",
    }
    if access_token:
        headers["X-Access-Token"] = access_token

    resp = requests.get(
        f"{config.gateway_url}/",
        headers=headers,
        timeout=10,
    )
    assert resp.status_code != 502, f"Gateway 502: sandbox {sandbox_id} not found or not running"
    assert resp.status_code != 401, f"Gateway 401: access token mismatch for sandbox {sandbox_id}"


def test_gateway_host_based_routing(sandbox_context, config):
    """Test routing via native E2B host header format: {port}-{sandboxID}.{domain}."""
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=120,
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    sandbox_id = sandbox.sandbox_id
    logger.info("sandbox-id: %s", sandbox_id)

    # Wait for gateway registry to sync
    time.sleep(3)

    access_token = get_sandbox_access_token(sandbox_id)

    host = f"49983-{sandbox_id}.{config.e2b_domain}"
    headers = {"Host": host}
    if access_token:
        headers["X-Access-Token"] = access_token

    resp = requests.get(
        f"{config.gateway_url}/",
        headers=headers,
        timeout=10,
    )
    assert resp.status_code != 502, f"Gateway 502: sandbox {sandbox_id} not found or not running"
    assert resp.status_code != 401, f"Gateway 401: access token mismatch for sandbox {sandbox_id}"
