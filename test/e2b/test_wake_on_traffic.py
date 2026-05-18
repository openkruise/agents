"""Wake-on-traffic tests driven by the E2B SDK."""
import json
import re
import subprocess
import time
from datetime import datetime, timedelta, timezone
from typing import Any, Callable

import pytest
from e2b_code_interpreter import Sandbox, SandboxState

from utils import run_code_sandbox


WAKE_ON_TRAFFIC_ANNOTATION = "agents.kruise.io/wake-on-traffic"
KUBERNETES_TIME_RE = re.compile(
    r"^(?P<prefix>.+?)(?:\.(?P<fraction>\d+))?(?P<offset>Z|[+-]\d{2}:\d{2})$"
)


def kubectl_json(*args: str) -> dict[str, Any]:
    result = subprocess.run(
        ["kubectl", *args, "-o", "json"],
        capture_output=True,
        check=True,
        text=True,
    )
    return json.loads(result.stdout)


def sandbox_object_key(sandbox_id: str) -> tuple[str, str]:
    parts = sandbox_id.split("--", 1)
    assert len(parts) == 2, f"unexpected sandbox id format: {sandbox_id}"
    return parts[0], parts[1]


def get_sandbox_resource(sandbox_id: str) -> dict[str, Any]:
    namespace, name = sandbox_object_key(sandbox_id)
    return kubectl_json("get", "sbx", name, "-n", namespace)


def wait_for_resource(
    sandbox_id: str,
    predicate: Callable[[dict[str, Any]], bool],
    description: str,
    timeout_seconds: int = 180,
    interval_seconds: int = 2,
) -> dict[str, Any]:
    deadline = time.time() + timeout_seconds
    last_resource: dict[str, Any] | None = None

    while time.time() < deadline:
        last_resource = get_sandbox_resource(sandbox_id)
        if predicate(last_resource):
            return last_resource
        time.sleep(interval_seconds)

    raise AssertionError(
        f"timed out waiting for {description}; "
        f"last sandbox resource: {json.dumps(last_resource, indent=2, sort_keys=True)}"
    )


def wait_for_sdk_state(
    sandbox: Sandbox,
    expected_state: SandboxState,
    timeout_seconds: int = 180,
    interval_seconds: int = 2,
):
    deadline = time.time() + timeout_seconds
    last_state = None

    while time.time() < deadline:
        info = sandbox.get_info()
        last_state = info.state
        if last_state == expected_state:
            return info
        time.sleep(interval_seconds)

    raise AssertionError(
        f"timed out waiting for sandbox {sandbox.sandbox_id} to become "
        f"{expected_state}; last state: {last_state}"
    )


def parse_kubernetes_time(value: str) -> datetime:
    match = KUBERNETES_TIME_RE.match(value)
    assert match is not None, f"unexpected Kubernetes timestamp: {value}"

    offset = match.group("offset")
    if offset == "Z":
        offset = "+00:00"

    fraction = match.group("fraction")
    if fraction is None:
        return datetime.fromisoformat(f"{match.group('prefix')}{offset}")

    return datetime.fromisoformat(
        f"{match.group('prefix')}.{fraction[:6].ljust(6, '0')}{offset}"
    )


TEST_CASES = [
    {
        "name": "run_code_wakes_paused_auto_resume_sandbox",
        "timeout": 60,
        "expected_policy": "timeout:60s",
    },
]


@pytest.mark.parametrize("case", TEST_CASES, ids=[case["name"] for case in TEST_CASES])
def test_run_code_wakes_paused_auto_resume_sandbox(sandbox_context, case):
    sandbox: Sandbox = sandbox_context.add(Sandbox.create(
        template="code-interpreter",
        timeout=case["timeout"],
        lifecycle={
            "auto_resume": True,
        },
        metadata={
            "test_case": "test_run_code_wakes_paused_auto_resume_sandbox",
        },
        headers={
            "x-request-id": sandbox_context.request_id,
        },
    ))
    print(f"sandbox-id: {sandbox.sandbox_id}")

    wait_for_resource(
        sandbox.sandbox_id,
        lambda resource: resource.get("metadata", {})
        .get("annotations", {})
        .get(WAKE_ON_TRAFFIC_ANNOTATION) == case["expected_policy"],
        "wake-on-traffic annotation after create",
    )

    sandbox.beta_pause()
    wait_for_sdk_state(sandbox, SandboxState.PAUSED)
    wait_for_resource(
        sandbox.sandbox_id,
        lambda resource: resource.get("spec", {}).get("paused") is True
        and resource.get("metadata", {})
        .get("annotations", {})
        .get(WAKE_ON_TRAFFIC_ANNOTATION) == case["expected_policy"],
        "paused sandbox with wake-on-traffic annotation",
    )

    # Allow the gateway route cache to observe the paused sandbox before the SDK
    # sends traffic to the code-interpreter port.
    time.sleep(3)
    wake_start = datetime.now(timezone.utc)
    execution = run_code_sandbox(sandbox, "print('wake-on-traffic')")

    if execution.error:
        raise Exception(execution.error)
    assert execution.logs.stdout == ["wake-on-traffic\n"]

    wait_for_sdk_state(sandbox, SandboxState.RUNNING)
    resource = wait_for_resource(
        sandbox.sandbox_id,
        lambda resource: resource.get("spec", {}).get("paused") is False
        and bool(resource.get("spec", {}).get("shutdownTime"))
        and resource.get("metadata", {})
        .get("annotations", {})
        .get(WAKE_ON_TRAFFIC_ANNOTATION) == case["expected_policy"],
        "resumed sandbox with refreshed shutdown time",
    )

    shutdown_time = parse_kubernetes_time(resource["spec"]["shutdownTime"])
    assert shutdown_time >= wake_start + timedelta(minutes=4)
