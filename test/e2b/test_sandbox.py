import json
import subprocess
import time
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timedelta, timezone
from importlib.metadata import version as _pkg_version

import pytest
from dateutil.tz import tzutc
from e2b.exceptions import NotFoundException
from e2b_code_interpreter import Sandbox, SandboxQuery, SandboxState

import logging

from utils import list_sandbox, connect_sandbox, run_code_sandbox

logger = logging.getLogger(__name__)

# e2b-code-interpreter 2.4.x predates the `lifecycle={"on_timeout": "pause"}`
# parameter, so auto-pause cannot be requested through that SDK.
_E2B_CODE_INTERPRETER_VERSION = _pkg_version("e2b-code-interpreter")
_SDK_LACKS_AUTO_PAUSE = _E2B_CODE_INTERPRETER_VERSION.startswith("2.4.")
_SDK_LACKS_SANDBOX_PAUSE = not hasattr(Sandbox, "pause")


def _get_sandbox_json(name: str) -> dict:
    """Fetch the live Sandbox CR by name (post `--` portion of sandbox_id)."""
    result = subprocess.run(
        ["kubectl", "get", "sbx", name, "-o", "json"],
        capture_output=True,
        text=True,
        check=True,
    )
    return json.loads(result.stdout)


def _get_sandbox_spec(name: str) -> dict:
    """Fetch the live Spec of a Sandbox CR by name (post `--` portion of sandbox_id)."""
    return _get_sandbox_json(name).get("spec", {})


def _parse_rfc3339_utc(s: str) -> datetime:
    """Parse an RFC3339 timestamp (with or without trailing Z) into a UTC-aware datetime."""
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    return datetime.fromisoformat(s).astimezone(timezone.utc)


RETURN_POD_IP_METADATA_KEY = "e2b.agents.kruise.io/return-sandbox-ip"
POD_IP_METADATA_KEY = "e2b.agents.kruise.io/sandbox-ip"
RESERVE_PAUSED_SANDBOX_FOR_METADATA_KEY = "e2b.agents.kruise.io/reserve-paused-sandbox-duration"
RESERVE_PAUSED_SANDBOX_FOR_HEADER = "x-e2b-kruise-reserve-paused-sandbox-duration"
RESERVE_PAUSED_SANDBOX_FOR_ANNOTATION = "agents.kruise.io/reserve-paused-sandbox-duration"


def assert_pod_ip_metadata(info, expected_pod_ip: str | None = None) -> str:
    metadata = info.metadata or {}
    assert POD_IP_METADATA_KEY in metadata
    pod_ip = metadata[POD_IP_METADATA_KEY]
    assert isinstance(pod_ip, str)
    if expected_pod_ip is None:
        assert pod_ip != ""
    else:
        assert pod_ip == expected_pod_ip
    return pod_ip


def _wait_for_sandbox_state(sbx: Sandbox, expected_state: SandboxState, timeout_seconds: int):
    deadline = time.time() + timeout_seconds
    while time.time() < deadline:
        info = sbx.get_info()
        if info.state == expected_state:
            return info
        time.sleep(2)
    raise AssertionError(
        f"sandbox {sbx.sandbox_id} did not reach {expected_state} within {timeout_seconds}s"
    )


# Link: https://e2b.dev/docs/sandbox
def test_lifecycle(sandbox_context, config):
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=30,
        metadata={
            'userId': '123',
        },
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("sandbox-id: %s", sandbox.sandbox_id)
    info = sandbox.get_info()
    logger.info("info: %s", info)
    assert info.template_id == config.templates.code_interpreter
    assert info.state == SandboxState.RUNNING
    assert info.metadata["userId"] == "123"
    assert POD_IP_METADATA_KEY not in info.metadata


def test_sandbox_with_pod_ip(sandbox_context, config):
    test_case = f"test_sandbox_with_pod_ip-{uuid.uuid4()}"
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=6000,
        metadata={
            "test_case": test_case,
            RETURN_POD_IP_METADATA_KEY: "true",
        },
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("sandbox-id: %s", sandbox.sandbox_id)

    info = sandbox.get_info()
    pod_ip = assert_pod_ip_metadata(info)
    assert RETURN_POD_IP_METADATA_KEY not in info.metadata

    listed = list_sandbox(
        query=SandboxQuery(
            metadata={
                "test_case": test_case,
            }
        ),
        namespace=config.test_namespace,
    )
    matching = [item for item in listed if item.sandbox_id == sandbox.sandbox_id]
    assert len(matching) == 1
    assert_pod_ip_metadata(matching[0], pod_ip)

    connected = connect_sandbox(sandbox, timeout=6000)
    assert connected is not None
    connected_info = connected.get_info()
    assert_pod_ip_metadata(connected_info, pod_ip)

    sandbox.beta_pause()
    resumed = connect_sandbox(sandbox, timeout=6000)
    assert resumed is not None
    resumed_info = resumed.get_info()
    assert resumed_info.state == SandboxState.RUNNING
    assert_pod_ip_metadata(resumed_info)


def test_no_stock(sandbox_context, config):
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter_0,
        timeout=30,
        metadata={
            'userId': '123',
        },
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("sandbox-id: %s", sandbox.sandbox_id)
    info = sandbox.get_info()
    logger.info("info: %s", info)
    assert info.template_id == config.templates.code_interpreter_0
    assert info.state == SandboxState.RUNNING
    assert info.metadata["userId"] == "123"


def test_list_by_metadata(sandbox_context, config):
    random_user_id = str(uuid.uuid4())

    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=30,
        metadata={
            'userId': random_user_id,
        },
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("sandbox-id: %s", sandbox.sandbox_id)
    info = sandbox.get_info()
    logger.info("info: %s", info)
    # List sandboxes that are running or paused.
    sandboxes = list_sandbox(
        query=SandboxQuery(
            metadata={
                "userId": random_user_id,
            }
        ),
        namespace=config.test_namespace,
    )
    assert len(sandboxes) == 1
    assert sandboxes[0].sandbox_id == info.sandbox_id


def test_list_by_state(sandbox_context, config):
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=30,
        metadata={"test_case": "test_list_by_state"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("sandbox-id: %s", sbx.sandbox_id)
    info = sbx.get_info()
    logger.info("info: %s", info)
    # List sandboxes that are running or paused.
    sandboxes = list_sandbox(
        query=SandboxQuery(
            state=[SandboxState.RUNNING, SandboxState.PAUSED],
        ),
        namespace=config.test_namespace,
    )

    found = False
    for sandbox in sandboxes:
        if sandbox.sandbox_id == sbx.sandbox_id:
            found = True
            break
    if not found:
        raise AssertionError(
            f"Sandbox {sbx.sandbox_id} not found in running sandboxes list"
        )
    logger.info("sandbox found in list")


def test_timeout(sandbox_context, config):
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        metadata={"case": "timeout"},
        timeout=120,
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    sandbox2: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=1200,
        metadata={"test_case": "test_timeout"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    sandbox2.set_timeout(10)
    logger.info("wait 10s timeout and check sandbox2 %s deleted", sandbox2.sandbox_id)
    time.sleep(10)
    with pytest.raises(NotFoundException):
        connect_sandbox(sandbox2)
    sandbox.get_info()  # still exists — timeout=120 provides ample margin

    # Verify sandbox can also be killed via set_timeout
    sandbox.set_timeout(10)
    logger.info("wait 10s timeout and check sandbox %s deleted", sandbox.sandbox_id)
    time.sleep(10)
    with pytest.raises(NotFoundException):
        connect_sandbox(sandbox)

    logger.info("wait 20s again and check sandbox %s deleted", sandbox.sandbox_id)
    time.sleep(20)
    with pytest.raises(NotFoundException):
        connect_sandbox(sandbox)


def test_connect_shorter_timeout(sandbox_context, config):
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=3600,
        metadata={"test_case": "test_connect_shorter_timeout"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))

    info_before = sandbox.get_info()
    assert info_before.state == SandboxState.RUNNING

    connect_sandbox(sandbox, timeout=300)
    info_after = sandbox.get_info()
    assert info_after.state == SandboxState.RUNNING

    # For running sandboxes, connect(timeout=<shorter>) must not shorten endAt.
    assert info_after.end_at == info_before.end_at


@pytest.mark.skipif(
    _SDK_LACKS_AUTO_PAUSE,
    reason=(
        f"e2b-code-interpreter {_E2B_CODE_INTERPRETER_VERSION} does not support "
        "lifecycle={'on_timeout': 'pause'}; auto-pause cannot be exercised."
    ),
)
def test_auto_pause_resume_no_immediate_repause(sandbox_context, config):
    """Connect after auto-pause must leave the sandbox running through the
    stability window: the Connect path replaces the expired PauseTime with
    the requested running deadline, and the next reconcile must respect it.
    """
    auto_pause_timeout_seconds = 30
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=auto_pause_timeout_seconds,
        lifecycle={"on_timeout": "pause"},
        metadata={"test_case": "test_auto_pause_resume_no_immediate_repause"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("sandbox-id: %s", sbx.sandbox_id)
    sandbox_name = sbx.sandbox_id.split("--")[1]

    # Step 1: wait for the controller to auto-pause the sandbox.
    pause_deadline = time.time() + auto_pause_timeout_seconds + 60
    paused = False
    while time.time() < pause_deadline:
        info = sbx.get_info()
        if info.state == SandboxState.PAUSED:
            paused = True
            logger.info("sandbox auto-paused: %s state=%s", sandbox_name, info.state)
            break
        time.sleep(2)
    assert paused, f"sandbox {sandbox_name} did not auto-pause within deadline"

    # Step 2: assert auto-pause took effect.
    spec = _get_sandbox_spec(sandbox_name)
    assert spec.get("paused") is True, (
        f"spec.paused must be true after auto-pause; got spec={spec}"
    )
    pause_time_str = spec.get("pauseTime")
    assert pause_time_str, "spec.pauseTime must remain non-nil after auto-pause"

    # Step 3: resume via E2B Connect with an explicit timeout. Record the
    # wall-clock window that brackets the server's `now`, so we can bound the
    # post-Connect Spec.PauseTime (= serverNow + connect_timeout for autoPause).
    connect_timeout_seconds = 600
    connect_start = datetime.now(timezone.utc)
    connect_sandbox(sbx, timeout=connect_timeout_seconds)
    connect_end = datetime.now(timezone.utc)

    info = sbx.get_info()
    assert info.state == SandboxState.RUNNING, (
        f"sandbox should be RUNNING after resume; got {info.state}"
    )

    # Step 4: stability window — controller must NOT immediately re-pause.
    stability_seconds = 15
    poll_interval = 2
    deadline = time.time() + stability_seconds
    while time.time() < deadline:
        info = sbx.get_info()
        assert info.state == SandboxState.RUNNING, (
            f"sandbox got re-paused unexpectedly during stability window; "
            f"state={info.state}"
        )
        time.sleep(poll_interval)

    # Step 5: confirm final spec is consistent — paused=false and pauseTime
    # lands in the narrow [connect_start, connect_end] + connect_timeout window
    # (a stale 5-min-old pauseTime would also be "in the future" — the bound
    # below proves it's the freshly-written deadline, not a leftover).
    spec_after = _get_sandbox_spec(sandbox_name)
    assert not spec_after.get("paused"), (
        f"spec.paused should be false after resume; got spec={spec_after}"
    )
    pause_time_after_str = spec_after.get("pauseTime")
    assert pause_time_after_str, "spec.pauseTime must remain non-nil after resume"
    pause_time_after = _parse_rfc3339_utc(pause_time_after_str)

    skew_tolerance = timedelta(seconds=10)
    expected_min = connect_start + timedelta(seconds=connect_timeout_seconds) - skew_tolerance
    expected_max = connect_end + timedelta(seconds=connect_timeout_seconds) + skew_tolerance
    assert expected_min <= pause_time_after <= expected_max, (
        f"spec.pauseTime should be ~connect_start + {connect_timeout_seconds}s; "
        f"got {pause_time_after_str}, expected in [{expected_min.isoformat()}, "
        f"{expected_max.isoformat()}]"
    )


@pytest.mark.skipif(
    _SDK_LACKS_AUTO_PAUSE,
    reason=(
        f"e2b-code-interpreter {_E2B_CODE_INTERPRETER_VERSION} does not support "
        "lifecycle={'on_timeout': 'pause'}; auto-pause cannot be exercised."
    ),
)
def test_auto_pause_respects_custom_paused_retention(sandbox_context, config):
    auto_pause_timeout_seconds = 30
    paused_retention = timedelta(minutes=2)
    skew_tolerance = timedelta(seconds=10)
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=auto_pause_timeout_seconds,
        lifecycle={"on_timeout": "pause"},
        metadata={
            "test_case": "test_auto_pause_respects_custom_paused_retention",
            RESERVE_PAUSED_SANDBOX_FOR_METADATA_KEY: "2m",
        },
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("sandbox-id: %s", sbx.sandbox_id)
    sandbox_name = sbx.sandbox_id.split("--")[1]

    crd = _get_sandbox_json(sandbox_name)
    annotations = crd.get("metadata", {}).get("annotations", {})
    spec = crd.get("spec", {})
    assert annotations.get(RESERVE_PAUSED_SANDBOX_FOR_ANNOTATION) == "2m"
    pause_time = _parse_rfc3339_utc(spec["pauseTime"])
    shutdown_time = _parse_rfc3339_utc(spec["shutdownTime"])
    assert abs((shutdown_time - pause_time) - paused_retention) <= skew_tolerance, (
        f"shutdownTime - pauseTime should be ~2m; spec={spec}"
    )

    pause_wait_start = datetime.now(timezone.utc)
    _wait_for_sandbox_state(sbx, SandboxState.PAUSED, auto_pause_timeout_seconds + 60)
    pause_wait_end = datetime.now(timezone.utc)

    crd_after_pause = _get_sandbox_json(sandbox_name)
    annotations_after_pause = crd_after_pause.get("metadata", {}).get("annotations", {})
    spec_after_pause = crd_after_pause.get("spec", {})
    assert spec_after_pause.get("paused") is True, (
        f"spec.paused must be true after auto-pause; got spec={spec_after_pause}"
    )
    assert annotations_after_pause.get(RESERVE_PAUSED_SANDBOX_FOR_ANNOTATION) == "2m"
    shutdown_time_after = _parse_rfc3339_utc(spec_after_pause["shutdownTime"])
    expected_min = pause_wait_start + paused_retention - skew_tolerance
    expected_max = pause_wait_end + paused_retention + skew_tolerance
    assert expected_min <= shutdown_time_after <= expected_max, (
        f"shutdownTime should be ~auto-pause time + 2m; got {shutdown_time_after.isoformat()}, "
        f"expected in [{expected_min.isoformat()}, {expected_max.isoformat()}]"
    )


@pytest.mark.skipif(
    _SDK_LACKS_SANDBOX_PAUSE,
    reason=(
        f"e2b-code-interpreter {_E2B_CODE_INTERPRETER_VERSION} does not support "
        "Sandbox.pause(headers=...); manual pause header cannot be exercised."
    ),
)
def test_manual_pause_header_respects_custom_paused_retention(sandbox_context, config):
    paused_retention = timedelta(minutes=3)
    skew_tolerance = timedelta(seconds=10)
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=6000,
        metadata={"test_case": "test_manual_pause_header_respects_custom_paused_retention"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("sandbox-id: %s", sbx.sandbox_id)
    sandbox_name = sbx.sandbox_id.split("--")[1]

    pause_start = datetime.now(timezone.utc)
    sbx.pause(headers={
        RESERVE_PAUSED_SANDBOX_FOR_HEADER: "3m",
    })
    pause_end = datetime.now(timezone.utc)

    _wait_for_sandbox_state(sbx, SandboxState.PAUSED, 60)

    crd = _get_sandbox_json(sandbox_name)
    annotations = crd.get("metadata", {}).get("annotations", {})
    spec = crd.get("spec", {})
    assert spec.get("paused") is True, f"spec.paused must be true after manual pause; got spec={spec}"
    assert annotations.get(RESERVE_PAUSED_SANDBOX_FOR_ANNOTATION) == "3m"
    shutdown_time = _parse_rfc3339_utc(spec["shutdownTime"])
    expected_min = pause_start + paused_retention - skew_tolerance
    expected_max = pause_end + paused_retention + skew_tolerance
    assert expected_min <= shutdown_time <= expected_max, (
        f"shutdownTime should be ~pause request time + 3m; got {shutdown_time.isoformat()}, "
        f"expected in [{expected_min.isoformat()}, {expected_max.isoformat()}]"
    )


def test_pause_connect_kill(sandbox_context, config):
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=6000,
        metadata={"test_case": "test_pause_connect_kill"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    sandbox.beta_pause()
    logger.info("wait 30s and check sandbox %s paused", sandbox.sandbox_id)
    time.sleep(30)
    logger.info("trying to connect sandbox")
    connect_sandbox(sandbox)
    exec_result = run_code_sandbox(sandbox, "print('Hello, world')")
    assert exec_result.logs.stdout == ["Hello, world\n"], (
        f"Smoke test after resume failed: stdout={exec_result.logs.stdout!r}, "
        f"error={exec_result.error!r}"
    )
    logger.info("sandbox is working after resume")


def test_pause_kill(sandbox_context, config):
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=6000,
        metadata={"test_case": "test_pause_kill"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    sandbox.beta_pause()
    time.sleep(1)


def test_pause_state(sandbox_context, config):
    sbx = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=6000,
        metadata={"test_case": "test_pause_state"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("Sandbox created: %s", sbx.sandbox_id)

    sbx.beta_pause()
    logger.info("Sandbox paused: %s", sbx.sandbox_id)

    sandboxes = list_sandbox(SandboxQuery(state=[SandboxState.PAUSED]), namespace=config.test_namespace)

    found = False
    for sandbox in sandboxes:
        if sandbox.sandbox_id == sbx.sandbox_id:
            found = True
            break

    if not found:
        raise AssertionError(f"Sandbox {sbx.sandbox_id} not found in paused sandboxes list")
    logger.info("sandbox found in paused list")


def test_resume_state(sandbox_context, config):
    sbx = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=6000,
        metadata={"test_case": "test_resume_state"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("Sandbox created: %s", sbx.sandbox_id)

    sbx.beta_pause()
    logger.info("Sandbox paused: %s", sbx.sandbox_id)
    time.sleep(30)
    same_sbx = connect_sandbox(sbx)
    logger.info("Connected to the sandbox: %s", same_sbx.sandbox_id)

    sandboxes = list_sandbox(SandboxQuery(state=[SandboxState.RUNNING]), namespace=config.test_namespace)

    found = False
    for sandbox in sandboxes:
        if sandbox.sandbox_id == sbx.sandbox_id:
            found = True
            break
    if not found:
        raise AssertionError(f"Sandbox {sbx.sandbox_id} not found in running sandboxes list")
    logger.info("sandbox found in running list")


def test_is_running(sandbox_context, config):
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    assert sbx.is_running()  # Returns True

    sbx.kill()
    # kill() sends a DELETE request that returns immediately, but the
    # underlying pod deletion is asynchronous — the gateway's informer
    # cache may still serve the route for a few hundred milliseconds.
    # Poll until is_running() returns False.
    max_wait = 30
    interval = 1
    deadline = time.time() + max_wait
    while time.time() < deadline:
        if not sbx.is_running():
            break
        time.sleep(interval)
    assert not sbx.is_running(), \
        f"Sandbox was still running {max_wait}s after kill()"


zero_time = datetime(1, 1, 1, 0, 0, tzinfo=tzutc())


def test_never_timeout(sandbox_context, config):
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=60,
        headers={
            "x-request-id": sandbox_context.request_id
        },
        metadata={
            "e2b.agents.kruise.io/never-timeout": "true"
        }
    ))
    assert sbx.is_running()  # Returns True
    info = sbx.get_info()
    assert info.end_at == zero_time

    sbx.set_timeout(60)
    info = sbx.get_info()
    assert info.end_at == zero_time

    sbx.beta_pause()
    info = sbx.get_info()
    assert info.end_at == zero_time
    assert sbx.is_running() is False

    sbx.connect(timeout=60)
    info = sbx.get_info()
    assert info.end_at == zero_time
    assert sbx.is_running() is True
    assert info.state == "running"


def test_concurrent_pause_running_sandbox(sandbox_context, config):
    """3 concurrent pause requests on a running sandbox should all succeed (idempotent).
    Final state must be PAUSED."""
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=6000,
        metadata={"test_case": "test_concurrent_pause_running_sandbox"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("Sandbox created: %s", sbx.sandbox_id)
    assert sbx.get_info().state == SandboxState.RUNNING

    errors = []

    def do_pause():
        try:
            sbx.beta_pause()
        except Exception as e:
            # 409 Conflict is acceptable for concurrent pause on already-paused sandbox
            if "409" in str(e) or "conflict" in str(e).lower():
                logger.info("Concurrent pause got expected conflict: %s", e)
            else:
                return e
        return None

    with ThreadPoolExecutor(max_workers=3) as executor:
        futures = [executor.submit(do_pause) for _ in range(3)]
        for future in as_completed(futures):
            result = future.result()
            if result is not None:
                errors.append(result)

    assert len(errors) == 0, f"Unexpected errors during concurrent pause: {errors}"

    # Wait for state to stabilize
    time.sleep(5)
    info = sbx.get_info()
    logger.info("Final state: %s", info.state)
    assert info.state == SandboxState.PAUSED


def test_concurrent_resume_paused_sandbox(sandbox_context, config):
    """3 concurrent resume (connect) requests on a paused sandbox should all succeed (idempotent).
    Final state must be RUNNING."""
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=6000,
        metadata={"test_case": "test_concurrent_resume_paused_sandbox"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("Sandbox created: %s", sbx.sandbox_id)

    # Pause first
    sbx.beta_pause()
    time.sleep(5)
    info = sbx.get_info()
    assert info.state == SandboxState.PAUSED, f"Expected PAUSED but got {info.state}"
    logger.info("Sandbox paused: %s", sbx.sandbox_id)

    errors = []

    def do_resume():
        try:
            connect_sandbox(sbx)
        except Exception as e:
            # 409 Conflict is acceptable for concurrent resume on already-running sandbox
            if "409" in str(e) or "conflict" in str(e).lower():
                logger.info("Concurrent resume got expected conflict: %s", e)
            else:
                return e
        return None

    with ThreadPoolExecutor(max_workers=3) as executor:
        futures = [executor.submit(do_resume) for _ in range(3)]
        for future in as_completed(futures):
            result = future.result()
            if result is not None:
                errors.append(result)

    assert len(errors) == 0, f"Unexpected errors during concurrent resume: {errors}"

    # Wait for state to stabilize
    time.sleep(5)
    info = sbx.get_info()
    logger.info("Final state: %s", info.state)
    assert info.state == SandboxState.RUNNING


def test_concurrent_pause_resume_on_paused_sandbox(sandbox_context, config):
    """2 concurrent pause + 2 concurrent resume on a paused sandbox.
    Pause is no-op (already paused), resume wins. Final state must be RUNNING."""
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=6000,
        metadata={"test_case": "test_concurrent_pause_resume_on_paused_sandbox"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("Sandbox created: %s", sbx.sandbox_id)

    # Pause first
    sbx.beta_pause()
    time.sleep(5)
    info = sbx.get_info()
    assert info.state == SandboxState.PAUSED, f"Expected PAUSED but got {info.state}"
    logger.info("Sandbox paused: %s", sbx.sandbox_id)

    errors = []

    def do_pause():
        try:
            sbx.beta_pause()
        except Exception as e:
            if "409" in str(e) or "conflict" in str(e).lower():
                logger.info("Concurrent pause got expected conflict: %s", e)
            else:
                return e
        return None

    def do_resume():
        try:
            connect_sandbox(sbx)
        except Exception as e:
            if "409" in str(e) or "conflict" in str(e).lower():
                logger.info("Concurrent resume got expected conflict: %s", e)
            else:
                return e
        return None

    with ThreadPoolExecutor(max_workers=4) as executor:
        futures = []
        futures.extend([executor.submit(do_pause) for _ in range(2)])
        futures.extend([executor.submit(do_resume) for _ in range(2)])
        for future in as_completed(futures):
            result = future.result()
            if result is not None:
                errors.append(result)

    assert len(errors) == 0, f"Unexpected errors during concurrent pause/resume: {errors}"

    # Wait for state to stabilize
    time.sleep(10)
    info = sbx.get_info()
    logger.info("Final state: %s", info.state)
    assert info.state == SandboxState.RUNNING


def test_concurrent_pause_resume_on_running_sandbox(sandbox_context, config):
    """2 concurrent pause + 2 concurrent resume on a running sandbox.
    Resume is no-op (already running), pause wins. Final state must be PAUSED."""
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=6000,
        metadata={"test_case": "test_concurrent_pause_resume_on_running_sandbox"},
        headers={
            "x-request-id": sandbox_context.request_id
        }
    ))
    logger.info("Sandbox created: %s", sbx.sandbox_id)
    assert sbx.get_info().state == SandboxState.RUNNING

    errors = []

    def do_pause():
        try:
            sbx.beta_pause()
        except Exception as e:
            if "409" in str(e) or "conflict" in str(e).lower():
                logger.info("Concurrent pause got expected conflict: %s", e)
            else:
                return e
        return None

    def do_resume():
        try:
            sbx.connect()
        except Exception as e:
            if "409" in str(e) or "conflict" in str(e).lower():
                logger.info("Concurrent resume got expected conflict: %s", e)
            else:
                return e
        return None

    with ThreadPoolExecutor(max_workers=4) as executor:
        futures = []
        futures.extend([executor.submit(do_pause) for _ in range(2)])
        futures.extend([executor.submit(do_resume) for _ in range(2)])
        for future in as_completed(futures):
            result = future.result()
            if result is not None:
                errors.append(result)

    assert len(errors) == 0, f"Unexpected errors during concurrent pause/resume: {errors}"

    # Wait for state to stabilize
    time.sleep(10)
    info = sbx.get_info()
    logger.info("Final state: %s", info.state)
    assert info.state == SandboxState.PAUSED


def test_sandbox_with_labels_and_command(sandbox_context, config):
    """Create a sandbox with labels via label: prefix metadata, verify labels
    are present in the returned sandbox metadata, then run a command inside."""
    sbx: Sandbox = sandbox_context.add(Sandbox.create(
        template=config.templates.code_interpreter,
        timeout=30,
        metadata={
            "test_case": "test_sandbox_with_labels_and_command",
            "label:app": "my-test-app",
            "label:env": "e2e",
        },
        headers={
            "x-request-id": sandbox_context.request_id,
        },
    ))
    logger.info("sandbox-id: %s", sbx.sandbox_id)

    # Run a shell command immediately after create returns to confirm the
    # sandbox is operational before any further API calls.
    result = sbx.commands.run("echo 'hello from labeled sandbox'")
    assert not result.error, f"command failed: {result.error}"
    assert "hello from labeled sandbox" in result.stdout

    # Also exercise run_code to confirm the code interpreter is functional.
    run_code_sandbox(sbx, "print('labels work')")

    # Verify the labels are reflected back in the sandbox metadata.
    # The label: prefix is stripped before the key is stored as a K8s label,
    # and labels are returned as plain metadata (without the label: prefix).
    info = sbx.get_info()
    logger.info("sandbox info: %s", info)
    assert info.metadata.get("app") == "my-test-app", (
        f"expected label 'app=my-test-app' in metadata, got: {info.metadata}"
    )
    assert info.metadata.get("env") == "e2e", (
        f"expected label 'env=e2e' in metadata, got: {info.metadata}"
    )
    # The label: prefixed keys must not leak into metadata.
    assert "label:app" not in info.metadata
    assert "label:env" not in info.metadata
