"""Tests for sandbox-gateway routing methods (header-based and host-based)."""
import json
import os
import subprocess
import time

import requests
from e2b_code_interpreter import Sandbox


GATEWAY_URL = "http://localhost:80"
WAKE_ON_TRAFFIC_ANNOTATION = "agents.kruise.io/wake-on-traffic"


def _get_api_url() -> str:
    api_url = os.environ.get("E2B_API_URL")
    if api_url:
        return api_url
    domain = os.environ.get("E2B_DOMAIN", "localhost")
    return f"http://{domain}/kruise/api"


def _get_headers(request_id: str) -> dict[str, str]:
    return {
        "X-API-KEY": os.environ.get("E2B_API_KEY", "some-api-key"),
        "X-Request-ID": request_id,
        "Content-Type": "application/json",
    }


def _sandbox_name(sandbox_id: str) -> str:
    return sandbox_id.split("--", 1)[1] if "--" in sandbox_id else sandbox_id


def _get_sandbox_cr(sandbox_id: str) -> dict:
    result = subprocess.run(
        ["kubectl", "get", "sbx", _sandbox_name(sandbox_id), "-o", "json"],
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0, (
        f"failed to get Sandbox CR for {sandbox_id}: stdout={result.stdout} stderr={result.stderr}"
    )
    return json.loads(result.stdout)


def _create_auto_resume_sandbox(api_url: str, headers: dict[str, str]) -> str:
    payload = {
        "templateID": "code-interpreter",
        "timeout": 60,
        "autoPause": True,
        "autoResume": {"enabled": True},
        "metadata": {
            "test_case": "test_wake_on_traffic_auto_resume",
        },
    }
    resp = requests.post(
        f"{api_url}/sandboxes",
        json=payload,
        headers=headers,
        timeout=90,
    )
    assert resp.status_code == 201, (
        f"failed to create auto-resume sandbox: {resp.status_code} {resp.text}"
    )
    body = resp.json()
    sandbox_id = body.get("sandboxID")
    assert sandbox_id, f"create response missing sandboxID: {resp.text}"
    return sandbox_id


def _delete_sandbox(api_url: str, headers: dict[str, str], sandbox_id: str | None):
    if not sandbox_id:
        return
    try:
        resp = requests.delete(
            f"{api_url}/sandboxes/{sandbox_id}",
            headers=headers,
            timeout=30,
        )
        if resp.status_code not in (204, 404):
            print(f"failed to delete sandbox {sandbox_id}: {resp.status_code} {resp.text}")
    except Exception as exc:
        print(f"failed to delete sandbox {sandbox_id}: {exc}")


def _pause_sandbox(api_url: str, headers: dict[str, str], sandbox_id: str):
    resp = requests.post(
        f"{api_url}/sandboxes/{sandbox_id}/pause",
        headers=headers,
        timeout=120,
    )
    assert resp.status_code == 204, (
        f"failed to pause sandbox {sandbox_id}: {resp.status_code} {resp.text}"
    )


def _get_sandbox_info(api_url: str, headers: dict[str, str], sandbox_id: str) -> dict:
    resp = requests.get(f"{api_url}/sandboxes/{sandbox_id}", headers=headers, timeout=30)
    assert resp.status_code == 200, (
        f"failed to get sandbox {sandbox_id}: {resp.status_code} {resp.text}"
    )
    return resp.json()


def _wait_for_sandbox_state(
    api_url: str,
    headers: dict[str, str],
    sandbox_id: str,
    expected_state: str,
    timeout_seconds: int,
) -> dict:
    deadline = time.time() + timeout_seconds
    last_info: dict | None = None
    while time.time() < deadline:
        last_info = _get_sandbox_info(api_url, headers, sandbox_id)
        if last_info.get("state") == expected_state:
            return last_info
        time.sleep(2)
    raise AssertionError(
        f"sandbox {sandbox_id} did not reach state {expected_state!r} within {timeout_seconds}s; "
        f"last info={last_info}"
    )


def _request_gateway_until_forwarded(sandbox_id: str, timeout_seconds: int) -> requests.Response:
    deadline = time.time() + timeout_seconds
    last_status = None
    last_body = ""
    while time.time() < deadline:
        resp = requests.get(
            f"{GATEWAY_URL}/",
            headers={
                "e2b-sandbox-id": sandbox_id,
                "e2b-sandbox-port": "49983",
            },
            timeout=180,
        )
        if resp.status_code != 502:
            return resp
        last_status = resp.status_code
        last_body = resp.text
        time.sleep(2)
    raise AssertionError(
        f"gateway did not forward traffic for sandbox {sandbox_id} within {timeout_seconds}s; "
        f"last_status={last_status} last_body={last_body}"
    )


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


def test_wake_on_traffic_auto_resume(sandbox_context):
    """Gateway traffic should auto-resume a paused sandbox created with autoResume enabled."""
    api_url = _get_api_url()
    headers = _get_headers(sandbox_context.request_id)
    sandbox_id = None

    try:
        sandbox_id = _create_auto_resume_sandbox(api_url, headers)
        print(f"sandbox-id: {sandbox_id}")

        cr = _get_sandbox_cr(sandbox_id)
        annotations = cr.get("metadata", {}).get("annotations", {})
        assert annotations.get(WAKE_ON_TRAFFIC_ANNOTATION) == "timeout:60", (
            f"wake-on-traffic annotation should be timeout:60; annotations={annotations}"
        )

        _pause_sandbox(api_url, headers, sandbox_id)
        _wait_for_sandbox_state(api_url, headers, sandbox_id, "paused", 120)

        # Let the gateway informer observe the paused route before sending the wake request.
        time.sleep(3)
        resp = _request_gateway_until_forwarded(sandbox_id, 180)
        assert resp.status_code != 502, (
            f"Gateway 502 after wake: sandbox {sandbox_id} not running"
        )

        info = _wait_for_sandbox_state(api_url, headers, sandbox_id, "running", 120)
        assert info.get("state") == "running", (
            f"sandbox should be running after wake; info={info}"
        )

        cr_after = _get_sandbox_cr(sandbox_id)
        spec_after = cr_after.get("spec", {})
        annotations_after = cr_after.get("metadata", {}).get("annotations", {})
        assert spec_after.get("paused") is not True, (
            f"spec.paused should be false after wake; spec={spec_after}"
        )
        assert annotations_after.get(WAKE_ON_TRAFFIC_ANNOTATION) == "timeout:60", (
            f"wake-on-traffic annotation should remain timeout:60; annotations={annotations_after}"
        )
    finally:
        _delete_sandbox(api_url, headers, sandbox_id)
