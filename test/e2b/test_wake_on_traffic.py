"""E2E test: wake a paused sandbox by sending traffic through the gateway."""
import time
from importlib.metadata import version as _pkg_version

import pytest
import requests
from e2b_code_interpreter import Sandbox, SandboxState

GATEWAY_URL = "http://localhost:80"
# Health-check path routed to manager_cluster by Envoy (prefix: /kruise/api).
# Any HTTP response (even 401) confirms the port-forward is alive.
# NOTE: /kruise/api/sandboxes is POST-only (returns 405 on GET), so use the
# v2 list endpoint which accepts GET.
_HEALTH_PATH = "/kruise/api/v2/sandboxes"

# e2b-code-interpreter 2.4.x predates the `lifecycle` parameter and
# `beta_pause()` method, so wake-on-traffic cannot be exercised through it.
_E2B_CODE_INTERPRETER_VERSION = _pkg_version("e2b-code-interpreter")
_SDK_LACKS_AUTO_PAUSE = _E2B_CODE_INTERPRETER_VERSION.startswith("2.4.")

# Long timeout so the sandbox stays alive throughout the test without
# risking ShutdownTime expiry.  We pause manually via beta_pause().
_SANDBOX_TIMEOUT = 300


def _gateway_health_check():
    """Return True if the gateway port-forward is alive.

    Any HTTP response (even 401 Unauthorized) confirms the port-forward and
    Envoy are both reachable.  Only connection errors / timeouts indicate a
    dead port-forward.
    """
    try:
        r = requests.get(f"{GATEWAY_URL}{_HEALTH_PATH}", timeout=10)
        ok = r.status_code < 500
        print(f"gateway health-check: status={r.status_code} ok={ok}")
        return ok
    except Exception as e:
        print(f"gateway health-check failed: {e}")
        return False


@pytest.mark.skipif(_SDK_LACKS_AUTO_PAUSE, reason="SDK lacks lifecycle / beta_pause()")
def test_wake_on_traffic(sandbox_context):
    """Traffic to a paused sandbox with wake-on-traffic should resume it."""
    # Step 1: Create sandbox with wake-on-traffic (auto_resume=True).
    # on_timeout="pause" is required by the server for auto_resume to be
    # accepted (autoResume requires autoPause).  Use a long timeout so the
    # sandbox won't auto-pause or expire during the test.  We pause it
    # manually via beta_pause() instead.
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=_SANDBOX_TIMEOUT,
        lifecycle={"on_timeout": "pause", "auto_resume": True},
        metadata={"test_case": "test_wake_on_traffic"},
        headers={"x-request-id": sandbox_context.request_id},
    ))
    sandbox_id = sbx.sandbox_id
    print(f"sandbox-id: {sandbox_id}")
    assert sbx.get_info().state == SandboxState.RUNNING

    # Step 2: Manually pause the sandbox via E2B API (POST /sandboxes/{id}/pause).
    print(f"pausing sandbox: {sandbox_id}")
    sbx.beta_pause()

    # Step 3: Wait until the sandbox reaches PAUSED state.
    paused = False
    pause_deadline = time.time() + 120
    while time.time() < pause_deadline:
        info = sbx.get_info()
        if info.state == SandboxState.PAUSED:
            paused = True
            print(f"sandbox paused: {sandbox_id}")
            break
        print(f"waiting for paused state, current: {info.state}")
        time.sleep(2)
    assert paused, f"sandbox {sandbox_id} did not pause within deadline"

    # Step 4: Verify gateway connectivity before sending wake traffic.
    assert _gateway_health_check(), (
        "Gateway port-forward is not alive before wake request."
    )

    # Step 5: Send traffic through the gateway (triggers wake).
    # The wake-on-traffic annotation was set at creation time via
    # lifecycle.auto_resume=True, so the gateway registry has WakeOnTraffic=true.
    #
    # The access token is required by the agent-runtime sidecar inside
    # the pod (not the gateway filter). Even when gateway auth is disabled,
    # the pod's envd still validates the token.
    access_token = sbx._envd_access_token or ""
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
    print(f"wake response headers: {dict(resp.headers)}")

    # Step 6: Assert wake succeeded (not 502/503)
    assert resp.status_code != 502, (
        f"Gateway 502: sandbox {sandbox_id} not found or not running after wake"
    )
    assert resp.status_code != 503, (
        f"Gateway 503: sandbox {sandbox_id} wake failed or timed out"
    )

    # Step 7: Verify sandbox is Running again.
    running = False
    last_state = None
    running_deadline = time.time() + 60
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
