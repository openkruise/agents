"""E2E test: wake a paused sandbox by sending traffic through the gateway."""
import json
import subprocess
import time
from importlib.metadata import version as _pkg_version

import pytest
import requests
from e2b_code_interpreter import Sandbox, SandboxState

GATEWAY_URL = "http://localhost:80"

# e2b-code-interpreter 2.4.x predates the `lifecycle={"on_timeout": "pause"}`
# parameter, so auto-pause cannot be requested through that SDK.
_E2B_CODE_INTERPRETER_VERSION = _pkg_version("e2b-code-interpreter")
_SDK_LACKS_AUTO_PAUSE = _E2B_CODE_INTERPRETER_VERSION.startswith("2.4.")


def _get_sandbox_access_token(sandbox_id: str) -> str:
    """Retrieve the runtime-access-token annotation from a Sandbox CR."""
    name = sandbox_id.split("--")[1] if "--" in sandbox_id else sandbox_id
    result = subprocess.run(
        ["kubectl", "get", "sandbox", name, "-o", "json"],
        capture_output=True, text=True, check=True,
    )
    sbx = json.loads(result.stdout)
    return sbx.get("metadata", {}).get("annotations", {}).get(
        "agents.kruise.io/runtime-access-token", ""
    )


@pytest.mark.skipif(_SDK_LACKS_AUTO_PAUSE, reason="SDK lacks lifecycle on_timeout pause")
def test_wake_on_traffic(sandbox_context):
    """Traffic to a paused sandbox with wake-on-traffic should resume it."""
    # Step 1: Create sandbox with auto-pause and auto-resume (wake-on-traffic).
    # lifecycle.auto_resume=True makes the server set the wake-on-traffic
    # annotation at creation time, so no post-create kubectl annotate is needed.
    # Use a longer timeout (120s) to give enough time for the wake test to
    # complete before the next auto-pause triggers.
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=30,
        lifecycle={"on_timeout": "pause", "auto_resume": True},
        metadata={"test_case": "test_wake_on_traffic"},
        headers={"x-request-id": sandbox_context.request_id},
    ))
    sandbox_id = sbx.sandbox_id
    print(f"sandbox-id: {sandbox_id}")
    assert sbx.get_info().state == SandboxState.RUNNING

    # Step 2: Wait for auto-pause
    pause_deadline = time.time() + 30 + 120
    paused = False
    while time.time() < pause_deadline:
        info = sbx.get_info()
        if info.state == SandboxState.PAUSED:
            paused = True
            print(f"sandbox auto-paused: {sandbox_id} state={info.state}")
            break
        time.sleep(2)
    assert paused, f"sandbox {sandbox_id} did not auto-pause within deadline"

    # Step 3: Send traffic through the gateway (triggers wake).
    # The wake-on-traffic annotation was set at creation time, so the
    # gateway registry already has WakeOnTraffic=true.
    #
    # The access token is required by the agent-runtime sidecar inside
    # the pod (not the gateway filter). Even when gateway auth is disabled,
    # the pod's envd still validates the token.
    access_token = _get_sandbox_access_token(sandbox_id)
    headers = {
        "e2b-sandbox-id": sandbox_id,
        "e2b-sandbox-port": "49983",
    }
    if access_token:
        headers["X-Access-Token"] = access_token

    print(f"sending wake traffic to {GATEWAY_URL} for {sandbox_id}")
    resp = requests.get(
        f"{GATEWAY_URL}/",
        headers=headers,
        timeout=120,  # generous timeout for wake
    )
    print(f"wake response: status={resp.status_code} body={resp.text[:200]!r}")

    # Step 4: Assert wake succeeded (not 502/503)
    assert resp.status_code != 502, (
        f"Gateway 502: sandbox {sandbox_id} not found or not running after wake"
    )
    assert resp.status_code != 503, (
        f"Gateway 503: sandbox {sandbox_id} wake failed or timed out"
    )

    # Step 5: Verify sandbox is Running (poll with retry for controller
    # reconciliation — the gateway wake triggers an async controller
    # reconcile to update Status.Phase from Paused to Running).
    running_deadline = time.time() + 60
    running = False
    last_state = None
    while time.time() < running_deadline:
        info = sbx.get_info()
        last_state = info.state
        if info.state == SandboxState.RUNNING:
            running = True
            break
        print(f"waiting for running state, current: {info.state}")
        time.sleep(2)
    assert running, (
        f"sandbox should be RUNNING after wake; got {last_state}"
    )
    print(f"wake-on-traffic succeeded: {sandbox_id} is running")
