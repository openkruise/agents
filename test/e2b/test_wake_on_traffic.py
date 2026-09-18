"""E2E test: wake a paused sandbox by sending traffic through the gateway."""
import time
from importlib.metadata import version as _pkg_version

import pytest
import requests
from e2b_code_interpreter import Sandbox, SandboxState

from utils import get_sandbox_cr, resolve_sandbox_cr, wait_sandbox_fully_paused

GATEWAY_URL = "http://localhost:80"
# Health-check path routed to manager_cluster by Envoy (prefix: /kruise/api).
# GET /health is the manager's dedicated health endpoint (returns 200 OK).
# A 200 here confirms the port-forward and Envoy are both alive.
_HEALTH_PATH = "/kruise/api/health"

# Spec path of the wake-on-traffic rule.
_WAKE_RULE_PATH = ("spec", "autoPausePolicy", "resume", "onIngressTraffic")

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


def _get_sandbox_annotations(sbx: Sandbox) -> dict:
    """Fetch the annotations of a Sandbox CR via kubectl."""
    return get_sandbox_cr(sbx).get("metadata", {}).get("annotations", {})


def _get_wake_rule(sbx: Sandbox) -> dict | None:
    """Fetch spec.autoPausePolicy.resume.onIngressTraffic of the CR."""
    node = get_sandbox_cr(sbx)
    for key in _WAKE_RULE_PATH:
        if not isinstance(node, dict) or key not in node:
            return None
        node = node[key]
    return node


# Response bodies of every local reply the gateway filter can send instead of
# forwarding the request upstream. A response carrying any of these bodies was
# produced by the gateway itself, not by the sandbox, so it cannot prove the
# pending wake request was continued.
_GATEWAY_LOCAL_REPLY_BODIES = (
    "sandbox gateway is not ready",
    "sandbox not found:",
    "unauthorized: invalid or missing access token",
    "forbidden:",
    "service unavailable: traffic access token verifier is not ready",
    "healthy sandbox not found:",
    "wake failed",
    "sandbox wake failed",
)


def _send_single_wake_request(headers: dict, timeout_sec: int = 180) -> requests.Response:
    """Send exactly one request through the gateway and return its response.

    The wake-on-traffic filter holds this very request (api.Running) while
    the sandbox resumes and continues it once the sandbox is Ready, so the
    response proves both that the wake happened and that the original
    request was forwarded upstream. The client timeout must cover the full
    wake budget.

    Deliberately no retry: a retried request can succeed once the sandbox
    is Running even if the original pending request was never continued,
    which would hide a broken wake-completion path. Retries belong in the
    pre-wake synchronization steps only.
    """
    return requests.get(f"{GATEWAY_URL}/", headers=headers, timeout=timeout_sec)


@pytest.mark.skipif(_SDK_LACKS_AUTO_PAUSE, reason="SDK lacks lifecycle on_timeout pause")
def test_wake_on_traffic(sandbox_context):
    """Traffic to a paused sandbox with wake-on-traffic should resume it."""
    # Step 1: Create sandbox with auto-pause and auto-resume enabled.
    # auto_resume=True causes the API to set the wake-on-ingress-traffic
    # spec rule automatically at sandbox creation time (no kubectl patch
    # needed). Use a longer timeout (120s) to give enough time for the
    # auto-pause wait and the wake test to complete before ShutdownTime
    # triggers deletion.
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

    # Step 2: Verify the wake rule was written to the spec by the API.
    # Auto-pause sandboxes get the request timeout as the rule's
    # pauseTimeout, so a traffic wake re-arms auto-pause with it.
    wake_rule = _get_wake_rule(sbx)
    assert wake_rule is not None, (
        "autoResume=true should set spec.autoPausePolicy.resume.onIngressTraffic, "
        f"got CR spec: {get_sandbox_cr(sbx).get('spec', {})}"
    )
    # metav1.Duration marshals 120s as "2m0s"; accept both renderings.
    assert wake_rule.get("pauseTimeout") in ("120s", "2m0s"), (
        f"autoResume=true should set pauseTimeout from the request timeout, got: {wake_rule}"
    )
    print(f"wake rule verified: {wake_rule}")

    # Step 3: Wait for auto-pause.
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

    # Step 3.5: Wait for the pause transition to fully complete. get_info()
    # flips to PAUSED as soon as spec.paused is set (phase may still be
    # Running); a wake request in that window is rejected with
    # SandboxIsPausing. Since step 5 sends exactly one request, it must only
    # go out after status.phase is Paused and the SandboxPaused condition
    # is True.
    wait_sandbox_fully_paused(sbx)

    # Step 4: Verify gateway connectivity before sending wake traffic.
    assert _gateway_health_check(), (
        "Gateway port-forward is not alive before wake request. "
        "The port-forward may have dropped during the auto-pause wait."
    )

    # Step 5: Send a single request through the gateway (triggers wake).
    # The wake-on-ingress-traffic rule was set by the API (autoResume=true),
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
    resp = _send_single_wake_request(headers, timeout_sec=180)
    print(f"wake response: status={resp.status_code} body={resp.text[:200]!r}")
    print(f"wake response headers: {dict(resp.headers)}")

    # Step 6: Assert the single pending request was continued to the
    # sandbox's envd upstream. Any gateway local reply (wake failure,
    # auth rejection, missing route) means the original request was
    # answered by the filter instead of being forwarded, which fails the
    # test regardless of whether a later request would have succeeded.
    # (Gateway auth is disabled in this setup, so a 401 cannot come from
    # the gateway filter here.)
    body_start = resp.text.lstrip()[:120]
    assert not any(body_start.startswith(marker) for marker in _GATEWAY_LOCAL_REPLY_BODIES), (
        f"gateway answered the wake request locally instead of forwarding it "
        f"upstream: status={resp.status_code} body={resp.text[:200]!r}"
    )
    assert resp.status_code != 401, (
        f"envd rejected the access token: status=401 body={resp.text[:200]!r}"
    )

    # Step 7: Verify sandbox is Running (poll with retry for controller
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

    # Step 8: Verify the wake rule persists after wake.
    # The Resume operation re-arms the pause timeout but must not strip the
    # wake rule from the spec.
    post_wake_rule = _get_wake_rule(sbx)
    assert post_wake_rule is not None, (
        f"the wake rule should persist after wake, got CR spec: {get_sandbox_cr(sbx).get('spec', {})}"
    )
    print(f"post-wake wake rule verified: {post_wake_rule}")
