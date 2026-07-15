"""E2E test: wake a paused sandbox by sending traffic through the gateway."""
import json
import subprocess
import time
from importlib.metadata import version as _pkg_version

import pytest
import requests
from e2b_code_interpreter import Sandbox, SandboxState

GATEWAY_URL = "http://localhost:80"
# Health-check path routed to manager_cluster by Envoy (prefix: /kruise/api).
# GET /health is the manager's dedicated health endpoint (returns 200 OK).
# A 200 here confirms the port-forward and Envoy are both alive.
_HEALTH_PATH = "/kruise/api/health"

# Annotation keys used by the wake-on-traffic feature.
_ANN_WAKE_ON_TRAFFIC = "agents.kruise.io/wake-on-traffic"
_ANN_WAKE_TIMEOUT_SECONDS = "agents.kruise.io/wake-timeout-seconds"

# e2b-code-interpreter 2.4.x predates the `lifecycle={"on_timeout": "pause"}`
# parameter, so auto-pause cannot be requested through that SDK.
_E2B_CODE_INTERPRETER_VERSION = _pkg_version("e2b-code-interpreter")
_SDK_LACKS_AUTO_PAUSE = _E2B_CODE_INTERPRETER_VERSION.startswith("2.4.")


def _gateway_health_check():
    """Return True if the gateway port-forward is alive."""
    try:
        r = requests.get(f"{GATEWAY_URL}{_HEALTH_PATH}", timeout=10)
        ok = r.status_code == 200
        print(f"gateway health-check: status={r.status_code} ok={ok}")
        return ok
    except Exception as e:
        print(f"gateway health-check failed: {e}")
        return False


def _get_sandbox_annotations(sandbox_id: str) -> dict:
    """Fetch the annotations of a Sandbox CR via kubectl."""
    # sandbox_id format: "<namespace>--<name>" or just "<name>"
    name = sandbox_id.split("--")[1] if "--" in sandbox_id else sandbox_id
    result = subprocess.run(
        ["kubectl", "get", "sandbox", name, "-o", "json"],
        capture_output=True,
        text=True,
        check=True,
    )
    cr = json.loads(result.stdout)
    return cr.get("metadata", {}).get("annotations", {})


def _set_wake_timeout_annotation(sandbox_id: str, timeout_seconds: int):
    """Set the wake-timeout-seconds annotation on the Sandbox CR via kubectl.

    The wake-on-traffic annotation is set automatically by the API when
    autoResume=true, so only the timeout needs to be set out-of-band.
    """
    name = sandbox_id.split("--")[1] if "--" in sandbox_id else sandbox_id
    subprocess.run(
        [
            "kubectl", "annotate", "sandbox", name,
            "--overwrite",
            f"{_ANN_WAKE_TIMEOUT_SECONDS}={timeout_seconds}",
        ],
        capture_output=True,
        text=True,
        check=True,
    )


def _request_gateway_until_forwarded(headers: dict, timeout_sec: int = 120) -> requests.Response:
    """Send requests to the gateway until we get a non-502/503 response.

    The wake-on-traffic filter may return 502/503 while the sandbox is still
    resuming. We poll with a short interval to avoid flaky tests.
    """
    deadline = time.time() + timeout_sec
    last_resp = None
    attempt = 0
    while time.time() < deadline:
        attempt += 1
        try:
            resp = requests.get(
                f"{GATEWAY_URL}/",
                headers=headers,
                timeout=30,
            )
            last_resp = resp
            print(f"gateway attempt {attempt}: status={resp.status_code}")
            if resp.status_code not in (502, 503):
                return resp
        except Exception as e:
            print(f"gateway attempt {attempt}: error={e}")
        time.sleep(2)
    return last_resp


@pytest.mark.skipif(_SDK_LACKS_AUTO_PAUSE, reason="SDK lacks lifecycle on_timeout pause")
def test_wake_on_traffic(sandbox_context):
    """Traffic to a paused sandbox with wake-on-traffic should resume it."""
    # Step 1: Create sandbox with auto-pause and auto-resume enabled.
    # auto_resume=True causes the API to set the wake-on-traffic annotation
    # automatically at sandbox creation time (no kubectl annotate needed).
    # Use a longer timeout (120s) to give enough time for the auto-pause wait
    # and the wake test to complete before ShutdownTime triggers deletion.
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=120,
        lifecycle={"on_timeout": "pause", "auto_resume": True},
        metadata={"test_case": "test_wake_on_traffic"},
        headers={"x-request-id": sandbox_context.request_id},
    ))
    sandbox_id = sbx.sandbox_id
    print(f"sandbox-id: {sandbox_id}")
    assert sbx.get_info().state == SandboxState.RUNNING

    # Step 2: Verify wake-on-traffic annotation was set by the API.
    annotations = _get_sandbox_annotations(sandbox_id)
    assert annotations.get(_ANN_WAKE_ON_TRAFFIC) == "true", (
        f"autoResume=true should set wake-on-traffic annotation, got: {annotations}"
    )
    print(f"wake-on-traffic annotation verified (set by API): {annotations.get(_ANN_WAKE_ON_TRAFFIC)}")

    # Step 3: Set the wake-timeout-seconds annotation via kubectl
    # (the API only sets wake-on-traffic, not the timeout).
    _set_wake_timeout_annotation(sandbox_id, timeout_seconds=120)
    annotations = _get_sandbox_annotations(sandbox_id)
    wake_timeout_ann = annotations.get(_ANN_WAKE_TIMEOUT_SECONDS)
    assert wake_timeout_ann is not None, (
        f"expected wake-timeout-seconds annotation, got: {annotations}"
    )
    print(f"wake annotations verified: wake-on-traffic=true, wake-timeout-seconds={wake_timeout_ann}")

    # Step 4: Wait for auto-pause.
    # The sandbox timeout is 120s, but the E2B SDK's auto-pause is triggered
    # by the server-side timeout handler. The server may use a shorter internal
    # pause deadline. Poll until the sandbox reports PAUSED state.
    pause_deadline = time.time() + 120 + 120
    paused = False
    while time.time() < pause_deadline:
        info = sbx.get_info()
        if info.state == SandboxState.PAUSED:
            paused = True
            print(f"sandbox auto-paused: {sandbox_id} state={info.state}")
            break
        time.sleep(2)
    assert paused, f"sandbox {sandbox_id} did not auto-pause within deadline"

    # Step 5: Verify gateway connectivity before sending wake traffic.
    assert _gateway_health_check(), (
        "Gateway port-forward is not alive before wake request. "
        "The port-forward may have dropped during the auto-pause wait."
    )

    # Step 6: Send traffic through the gateway (triggers wake).
    # The wake-on-traffic annotation was set by the API (autoResume=true),
    # so the gateway registry already has WakeOnTraffic=true.
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
    resp = _request_gateway_until_forwarded(headers, timeout_sec=120)
    assert resp is not None, "gateway did not respond within timeout"
    print(f"wake response: status={resp.status_code} body={resp.text[:200]!r}")
    print(f"wake response headers: {dict(resp.headers)}")

    # Step 7: Assert wake succeeded (not 502/503)
    assert resp.status_code != 502, (
        f"Gateway 502: sandbox {sandbox_id} not found or not running after wake"
    )
    assert resp.status_code != 503, (
        f"Gateway 503: sandbox {sandbox_id} wake failed or timed out"
    )

    # Step 8: Verify sandbox is Running (poll with retry for controller
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

    # Step 9: Verify wake-on-traffic annotation persists after wake.
    # The Resume operation should preserve the wake annotations (they are
    # part of the sandbox metadata, not stripped by Resume).
    post_wake_annotations = _get_sandbox_annotations(sandbox_id)
    assert post_wake_annotations.get(_ANN_WAKE_ON_TRAFFIC) == "true", (
        f"wake-on-traffic annotation should persist after wake, got: {post_wake_annotations}"
    )
    assert post_wake_annotations.get(_ANN_WAKE_TIMEOUT_SECONDS) == wake_timeout_ann, (
        f"wake-timeout-seconds annotation should persist after wake, got: {post_wake_annotations}"
    )
    print(f"post-wake annotations verified: {post_wake_annotations}")
