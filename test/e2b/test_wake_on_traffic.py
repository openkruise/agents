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

# e2b-code-interpreter 2.4.x predates the `lifecycle={"on_timeout": "pause"}`
# parameter, so auto-pause cannot be requested through that SDK.
_E2B_CODE_INTERPRETER_VERSION = _pkg_version("e2b-code-interpreter")
_SDK_LACKS_AUTO_PAUSE = _E2B_CODE_INTERPRETER_VERSION.startswith("2.4.")

# Short auto-pause timeout so the sandbox pauses quickly (~30s).
# The controller's auto-pause does NOT extend ShutdownTime (only the
# manager API PauseSandbox handler does).  ShutdownTime stays at
# creation + maxTimeout, which can be as short as ~120s in some CI
# environments.  A short auto-pause timeout ensures we send the wake
# request well before ShutdownTime fires.
_AUTO_PAUSE_TIMEOUT = 30


def _gateway_health_check():
    """Return True if the gateway port-forward is alive.

    Any HTTP response (even 401 Unauthorized) confirms the port-forward and
    Envoy are both reachable.  Only connection errors / timeouts indicate a
    dead port-forward.
    """
    try:
        r = requests.get(f"{GATEWAY_URL}{_HEALTH_PATH}", timeout=10)
        # 401 is acceptable when gateway auth is enabled (the curl carries no
        # token).  Any status code means the gateway responded, so the
        # port-forward is alive.
        ok = r.status_code < 500
        print(f"gateway health-check: status={r.status_code} ok={ok}")
        return ok
    except Exception as e:
        print(f"gateway health-check failed: {e}")
        return False


@pytest.mark.skipif(_SDK_LACKS_AUTO_PAUSE, reason="SDK lacks lifecycle on_timeout pause")
def test_wake_on_traffic(sandbox_context):
    """Traffic to a paused sandbox with wake-on-traffic should resume it."""
    # Step 1: Create sandbox with auto-pause and auto-resume (wake-on-traffic).
    # lifecycle.auto_resume=True makes the server set the wake-on-traffic
    # annotation at creation time, so no post-create kubectl annotate is needed.
    # Use a short timeout (_AUTO_PAUSE_TIMEOUT) so auto-pause fires quickly
    # and the wake test completes before any ShutdownTime deadline.
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=_AUTO_PAUSE_TIMEOUT,
        lifecycle={"on_timeout": "pause", "auto_resume": True},
        metadata={"test_case": "test_wake_on_traffic"},
        headers={"x-request-id": sandbox_context.request_id},
    ))
    sandbox_id = sbx.sandbox_id
    print(f"sandbox-id: {sandbox_id}")
    assert sbx.get_info().state == SandboxState.RUNNING

    # Step 2: Wait for auto-pause.
    # The sandbox timeout is _AUTO_PAUSE_TIMEOUT, after which the controller
    # triggers auto-pause.  Poll until PAUSED with a generous buffer.
    pause_deadline = time.time() + _AUTO_PAUSE_TIMEOUT + 90
    paused = False
    while time.time() < pause_deadline:
        info = sbx.get_info()
        if info.state == SandboxState.PAUSED:
            paused = True
            print(f"sandbox auto-paused: {sandbox_id} state={info.state}")
            break
        time.sleep(2)
    assert paused, f"sandbox {sandbox_id} did not auto-pause within deadline"

    # Step 3: Verify gateway connectivity before sending wake traffic.
    assert _gateway_health_check(), (
        "Gateway port-forward is not alive before wake request. "
        "The port-forward may have dropped during the auto-pause wait."
    )

    # Step 4: Send traffic through the gateway (triggers wake).
    # The wake-on-traffic annotation was set at creation time, so the
    # gateway registry already has WakeOnTraffic=true.
    #
    # The access token is required by the agent-runtime sidecar inside
    # the pod (not the gateway filter). Even when gateway auth is disabled,
    # the pod's envd still validates the token. The SDK returns it directly
    # from the create response, so no kubectl lookup is needed.
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

    # Step 5: Assert wake succeeded (not 502/503)
    assert resp.status_code != 502, (
        f"Gateway 502: sandbox {sandbox_id} not found or not running after wake"
    )
    assert resp.status_code != 503, (
        f"Gateway 503: sandbox {sandbox_id} wake failed or timed out"
    )

    # Step 6: Verify sandbox is Running (poll with retry for controller
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
