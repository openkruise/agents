"""Tests for sandbox-gateway routing methods (header-based and host-based)."""
import json
import logging
import subprocess
import time

import pytest
import requests
from e2b_code_interpreter import Sandbox

from gateway_utils import get_sandbox_access_token

logger = logging.getLogger(__name__)
pytestmark = pytest.mark.gateway_routing


def test_gateway_runtime_mtls_disabled_by_default():
    """Verify the default deployment keeps Runtime mTLS completely disabled."""
    deployment = subprocess.run(
        ["kubectl", "get", "deployment", "sandbox-gateway", "-n", "sandbox-system", "-o", "json"],
        capture_output=True,
        text=True,
        check=True,
    )
    deployment_spec = json.loads(deployment.stdout)["spec"]["template"]["spec"]
    init_names = {container["name"] for container in deployment_spec.get("initContainers", [])}
    assert "runtime-mtls-cert-init" not in init_names

    configmap = subprocess.run(
        ["kubectl", "get", "configmap", "envoy-config", "-n", "sandbox-system", "-o", "json"],
        capture_output=True,
        text=True,
        check=True,
    )
    envoy_config = json.loads(configmap.stdout)["data"]["envoy.yaml"]
    assert "enable-runtime-mtls: false" in envoy_config
    assert "original_dst_mtls_cluster" not in envoy_config


def test_gateway_header_and_host_based_routing(sandbox_context, config):
    """Test header-based and native E2B host-based routing."""
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

    header_routing = {
        "e2b-sandbox-id": sandbox_id,
        "e2b-sandbox-port": "49983",
    }
    if access_token:
        header_routing["X-Access-Token"] = access_token

    header_response = requests.get(
        f"{config.gateway_url}/",
        headers=header_routing,
        timeout=10,
    )
    assert header_response.status_code not in (401, 502, 503), (
        f"Header routing failed for sandbox {sandbox_id}: {header_response.status_code}"
    )

    host = f"49983-{sandbox_id}.{config.e2b_domain}"
    host_routing = {"Host": host}
    if access_token:
        host_routing["X-Access-Token"] = access_token

    host_response = requests.get(
        f"{config.gateway_url}/",
        headers=host_routing,
        timeout=10,
    )
    assert host_response.status_code not in (401, 502, 503), (
        f"Host routing failed for sandbox {sandbox_id}: {host_response.status_code}"
    )
