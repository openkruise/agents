"""End-to-end coverage for optional sandbox-gateway Runtime mTLS."""

import json
import os
import subprocess
import time

import pytest
import requests


pytestmark = pytest.mark.runtime_mtls


def kubectl(*args: str, input_data: str | None = None) -> str:
    """Run kubectl and return stdout."""
    result = subprocess.run(
        ["kubectl", *args],
        input=input_data,
        capture_output=True,
        text=True,
        check=True,
    )
    return result.stdout


def wait_for_running_sandbox(name: str, timeout: int = 120) -> None:
    """Wait until the test Sandbox has a running Pod and gateway route."""
    deadline = time.time() + timeout
    phase = ""
    while time.time() < deadline:
        phase = kubectl(
            "get", "sandbox", name, "-n", "default", "-o", "jsonpath={.status.phase}",
        )
        if phase == "Running":
            time.sleep(3)
            return
        time.sleep(2)
    details = kubectl("get", "sandbox", name, "-n", "default", "-o", "yaml")
    raise AssertionError(f"Sandbox {name} did not become Running; phase={phase!r}\n{details}")


def wait_for_gateway_response(url: str, headers: dict[str, str], expected_body: str, timeout: int = 60) -> None:
    """Wait for the test listener and gateway registry to become ready."""
    deadline = time.time() + timeout
    last_result = "no request attempted"
    while time.time() < deadline:
        try:
            response = requests.get(url, headers=headers, timeout=10)
            last_result = f"status={response.status_code}, body={response.text!r}"
            if response.status_code == 200 and response.text == expected_body:
                return
        except requests.RequestException as error:
            last_result = repr(error)
        time.sleep(2)
    raise AssertionError(f"Gateway did not return {expected_body!r}: {last_result}")


def test_gateway_runtime_mtls_enabled(config):
    """Verify port 49983 reaches a backend requiring a valid gateway client certificate."""
    if os.environ.get("RUNTIME_MTLS_E2E") != "true":
        pytest.skip("Runtime mTLS E2E environment is not enabled")

    deployment = json.loads(kubectl("get", "deployment", "sandbox-gateway", "-n", "sandbox-system", "-o", "json"))
    init_names = {container["name"] for container in deployment["spec"]["template"]["spec"].get("initContainers", [])}
    assert "runtime-mtls-cert-init" in init_names

    envoy_config = kubectl("get", "configmap", "envoy-config", "-n", "sandbox-system", "-o", "jsonpath={.data.envoy\\.yaml}")
    assert "enable-runtime-mtls: true" in envoy_config
    assert "original_dst_mtls_cluster" in envoy_config

    sandbox_name = "gateway-runtime-mtls-e2e"
    manifest = {
        "apiVersion": "agents.kruise.io/v1alpha1",
        "kind": "Sandbox",
        "metadata": {"name": sandbox_name, "namespace": "default"},
        "spec": {
            "template": {
                "spec": {
                    "containers": [{
                        "name": "runtime-mtls-server",
                        "image": "runtime-mtls-server:latest",
                        "imagePullPolicy": "Never",
                        "volumeMounts": [{"name": "tls", "mountPath": "/tls", "readOnly": True}],
                        "readinessProbe": {
                            "tcpSocket": {"port": 49983},
                            "periodSeconds": 1,
                            "failureThreshold": 60,
                        },
                    }],
                    "volumes": [{"name": "tls", "secret": {"secretName": "runtime-mtls-server-tls"}}],
                },
            },
        },
    }

    kubectl("apply", "-f", "-", input_data=json.dumps(manifest))
    try:
        wait_for_running_sandbox(sandbox_name)
        wait_for_gateway_response(
            f"{config.gateway_url}/",
            {
                "e2b-sandbox-id": f"default--{sandbox_name}",
                "e2b-sandbox-port": "49983",
            },
            "runtime-mtls-ok",
        )
        wait_for_gateway_response(
            f"{config.gateway_url}/",
            {
                "e2b-sandbox-id": f"default--{sandbox_name}",
                "e2b-sandbox-port": "8080",
            },
            "runtime-plaintext-ok",
        )
    finally:
        kubectl("delete", "sandbox", sandbox_name, "-n", "default", "--ignore-not-found=true", "--wait=false")
