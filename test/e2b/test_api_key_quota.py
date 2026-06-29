import json
import os
import subprocess
import time
import uuid
from contextlib import contextmanager
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

import pytest
import requests
from e2b.exceptions import NotFoundException
from e2b_code_interpreter import Sandbox, SandboxState

from utils import connect_sandbox


PROJECT_ROOT = Path(__file__).resolve().parents[2]
ASSETS_DIR = PROJECT_ROOT / "test" / "e2b" / "assets"
QUOTA_SMALL_TEMPLATE = "quota-small"
QUOTA_NOSTOCK_TEMPLATE = "quota-nostock"
ABSENT_QUOTA = object()
QUOTA_E2E_PROFILE = os.environ.get("QUOTA_E2E_PROFILE", "none")
QUOTA_E2E_PROFILES = {"redis", "redis-clone", "redis-fault", "no-redis"}
QUOTA_NO_REDIS_TEST = "test_quota_is_accepted_and_unenforced_without_redis"
REDIS_NAMESPACE = "sandbox-system"
REDIS_SELECTOR = "app.kubernetes.io/name=redis"
REDIS_DEPLOYMENT = "redis"
SDK_REQUEST_TIMEOUT_SECONDS = 120
SNAPSHOT_WAIT_SUCCESS_SECONDS = 300

pytestmark = pytest.mark.skipif(
    QUOTA_E2E_PROFILE not in QUOTA_E2E_PROFILES,
    reason="API-key quota E2E is enabled only by QUOTA_E2E_PROFILE",
)


@pytest.fixture(autouse=True)
def skip_non_matching_quota_profile(request):
    if QUOTA_E2E_PROFILE == "no-redis" and request.node.name != QUOTA_NO_REDIS_TEST:
        pytest.skip("requires Redis quota profile")


def api_url():
    return os.environ.get("E2B_API_URL") or f"http://{os.environ.get('E2B_DOMAIN', 'localhost')}/kruise/api"


def with_request_id(headers, request_id):
    if not request_id:
        return headers
    return {**headers, "x-request-id": request_id}


def admin_headers(request_id=None):
    return with_request_id(
        {
            "X-API-KEY": os.environ.get("E2B_API_KEY", "e2b_00000000"),
            "Content-Type": "application/json",
        },
        request_id,
    )


def key_headers(api_key, request_id=None):
    return with_request_id({"X-API-KEY": api_key, "Content-Type": "application/json"}, request_id)


def quota_spec(*limits):
    return {"limits": [{"dimension": dimension, "scope": scope, "limit": limit} for dimension, scope, limit in limits]}


def assert_static_quota_response(payload, expected_quota):
    assert "usage" not in payload
    assert "quotaUsage" not in payload
    if expected_quota is None:
        assert "quota" not in payload or payload["quota"] in (None, {}), payload
        return
    assert payload.get("quota") == expected_quota, payload


def create_api_key(name, quota_marker, quota=ABSENT_QUOTA, headers=None, team_name=None):
    payload = {"name": f"{name}-{quota_marker}"}
    if team_name:
        payload["teamName"] = team_name
    if quota is not ABSENT_QUOTA:
        payload["quota"] = quota
    resp = requests.post(f"{api_url()}/api-keys", json=payload, headers=with_request_id(headers or admin_headers(), quota_marker))
    if resp.status_code == 201:
        key = resp.json().get("key")
        if key:
            wait_until(lambda: assert_api_key_usable(key, quota_marker), timeout=120)
    return resp


def assert_api_key_usable(api_key, marker=None):
    resp = requests.get(f"{api_url()}/api-keys/compatible", headers=key_headers(api_key, marker), timeout=10)
    assert resp.status_code == 200, resp.text


def assert_api_key_rejected(api_key, marker=None):
    resp = requests.get(f"{api_url()}/api-keys/compatible", headers=key_headers(api_key, marker), timeout=10)
    assert resp.status_code in (401, 403), resp.text


def delete_api_key(key_id, marker=None):
    if key_id:
        requests.delete(f"{api_url()}/api-keys/{key_id}", headers=admin_headers(marker))


def create_sandbox_with_key(api_key, template, marker, timeout=600):
    kwargs = {
        "template": template,
        "timeout": timeout,
        "metadata": {"quota_e2e": marker},
        "api_key": api_key,
        "headers": {"x-request-id": marker},
    }
    try:
        return Sandbox.create(**kwargs, request_timeout=SDK_REQUEST_TIMEOUT_SECONDS)
    except TypeError as exc:
        if "request_timeout" not in str(exc):
            raise
        return Sandbox.create(**kwargs)


def create_snapshot_with_key(api_key, sbx, marker):
    resp = requests.post(
        f"{api_url()}/sandboxes/{sbx.sandbox_id}/snapshots",
        json={},
        headers={
            **key_headers(api_key),
            "x-request-id": marker,
            "x-e2b-kruise-snapshot-wait-success-seconds": str(SNAPSHOT_WAIT_SUCCESS_SECONDS),
        },
        timeout=SNAPSHOT_WAIT_SUCCESS_SECONDS + 10,
    )
    assert resp.status_code in (200, 201), resp.text
    body = resp.json()
    checkpoint_id = body.get("snapshotID")
    assert checkpoint_id, body
    return checkpoint_id


def delete_template_or_snapshot(api_key, checkpoint_id, marker=None):
    try:
        requests.delete(f"{api_url()}/templates/{checkpoint_id}", headers=key_headers(api_key, marker), timeout=10)
    except Exception as exc:
        print(f"checkpoint cleanup ignored for {checkpoint_id}: {exc}")


def sandbox_name(sbx):
    return sbx.sandbox_id.split("--", 1)[1]


def kubectl_json(*args):
    result = subprocess.run(["kubectl", *args, "-o", "json"], capture_output=True, text=True, check=True)
    return json.loads(result.stdout)


def kubectl(*args):
    return subprocess.run(["kubectl", *args], capture_output=True, text=True, check=True).stdout


def dump_quota_template_state():
    commands = (
        ["get", "sandboxset", QUOTA_SMALL_TEMPLATE, QUOTA_NOSTOCK_TEMPLATE, "-o", "wide"],
        ["get", "sbx", "-l", f"agents.kruise.io/sandbox-template={QUOTA_SMALL_TEMPLATE}", "-o", "wide"],
        ["describe", "sbx", "-l", f"agents.kruise.io/sandbox-template={QUOTA_SMALL_TEMPLATE}"],
        ["get", "pod", "-o", "wide"],
    )
    for args in commands:
        print(f"$ kubectl {' '.join(args)}")
        subprocess.run(["kubectl", *args], check=False)


def redis_faults_enabled():
    return QUOTA_E2E_PROFILE == "redis-fault"


def redis_namespace():
    return REDIS_NAMESPACE


def redis_selector():
    return REDIS_SELECTOR


def redis_pod():
    pods = kubectl_json("get", "pod", "-n", redis_namespace(), "-l", redis_selector())
    names = [item["metadata"]["name"] for item in pods.get("items", [])]
    assert names, "no Redis pod found"
    return names[0]


def redis_cli(*args):
    return kubectl(
        "exec",
        "-n",
        redis_namespace(),
        redis_pod(),
        "--",
        "redis-cli",
        *args,
    ).strip()


def redis_hget(hash_key, field):
    value = redis_cli("HGET", hash_key, field)
    return int(value) if value else 0


def quota_live_key(key_id):
    return f"q:live:{{{key_id}}}"


def quota_sum_key(key_id, dimension):
    return f"q:sum:{{{key_id}}}:{dimension}"


def assert_no_ready_redis_pods():
    pods = kubectl_json("get", "pod", "-n", redis_namespace(), "-l", redis_selector())
    ready = []
    for item in pods.get("items", []):
        for cond in item.get("status", {}).get("conditions", []):
            if cond.get("type") == "Ready" and cond.get("status") == "True":
                ready.append(item["metadata"]["name"])
                break
    assert ready == [], f"Redis pods still ready: {ready}"


def assert_redis_ping():
    try:
        value = redis_cli("PING")
    except subprocess.CalledProcessError as exc:
        raise AssertionError(f"redis not reachable yet: {exc}")
    assert value == "PONG"


def wait_until(assertion, timeout=120, interval=2):
    deadline = time.time() + timeout
    last_error = None
    while time.time() < deadline:
        try:
            return assertion()
        except AssertionError as exc:
            last_error = exc
            time.sleep(interval)
    raise last_error or AssertionError("condition did not become true")


def is_quota_rejection(exc):
    text = str(exc).lower()
    return "403" in text and "quota" in text


def is_transient_lifecycle_error(exc):
    text = str(exc).lower()
    return any(
        fragment in text
        for fragment in (
            "500",
            "502",
            "503",
            "504",
            "5xx",
            "bad gateway",
            "gateway timeout",
            "service unavailable",
            "no stock",
            "dead sandbox",
            "context canceled",
            "deadline exceeded",
            "timeout",
            "timed out",
            "connection aborted",
            "connection reset",
        )
    )


def track_sandbox(owned, sbx):
    owned.append(sbx)
    return sbx


def assert_sandbox_gone(sbx):
    try:
        connect_sandbox(sbx)
    except NotFoundException:
        return
    raise AssertionError(f"sandbox still exists: {getattr(sbx, 'sandbox_id', '<unknown>')}")


def assert_sandbox_cr_gone(name):
    result = subprocess.run(["kubectl", "get", "sbx", name], capture_output=True, text=True)
    if result.returncode == 0:
        raise AssertionError(f"sandbox CR still exists: {name}")
    stderr = (result.stderr or "").lower()
    assert "not found" in stderr or "notfound" in stderr, result.stderr or result.stdout


def assert_sandbox_state(sbx, expected):
    info = sbx.get_info()
    assert info.state == expected, f"state={info.state}, expected={expected}"


def pause_sandbox(sbx):
    if hasattr(sbx, "pause"):
        return sbx.pause()
    return sbx.beta_pause()


def resume_sandbox(sbx):
    box = {}

    def attempt():
        try:
            if hasattr(sbx, "resume"):
                try:
                    box["sandbox"] = sbx.resume(request_timeout=SDK_REQUEST_TIMEOUT_SECONDS)
                except TypeError as exc:
                    if "request_timeout" not in str(exc):
                        raise
                    box["sandbox"] = sbx.resume()
                return
            try:
                box["sandbox"] = sbx.connect(timeout=6000, request_timeout=SDK_REQUEST_TIMEOUT_SECONDS)
            except TypeError as exc:
                if "request_timeout" not in str(exc):
                    raise
                box["sandbox"] = connect_sandbox(sbx, timeout=6000)
        except Exception as exc:
            if is_transient_lifecycle_error(exc):
                raise AssertionError(str(exc))
            raise

    wait_until(attempt, timeout=180)
    return box.get("sandbox")


def force_delete_sandbox(sbx):
    # Last-resort cleanup when the SDK kill/wait path fails. Quota-owned sandboxes
    # are not admin-owned, so the autouse wait_for_sandbox cannot catch a leak; a
    # leftover sandbox would consume the quota-small pool until its lifecycle
    # timeout. Delete the CR directly by name (default namespace) and move on.
    try:
        name = sbx if isinstance(sbx, str) else sandbox_name(sbx)
    except Exception:
        return
    subprocess.run(
        ["kubectl", "delete", "sbx", name, "--ignore-not-found=true", "--wait=false"],
        check=False,
    )
    try:
        wait_until(lambda: assert_sandbox_cr_gone(name), timeout=120)
    except Exception as exc:
        print(f"quota cleanup CR delete did not finish for {name}: {exc}")


def cleanup_quota_case(key_id, owned, marker=None):
    for sbx in reversed(owned):
        try:
            sbx.kill()
        except Exception as exc:
            print(f"quota cleanup kill fell back to kubectl for {getattr(sbx, 'sandbox_id', '<unknown>')}: {exc}")
            force_delete_sandbox(sbx)
    for sbx in owned:
        try:
            wait_until(lambda sbx=sbx: assert_sandbox_gone(sbx), timeout=120)
        except Exception as exc:
            print(f"quota cleanup wait fell back to kubectl for {getattr(sbx, 'sandbox_id', '<unknown>')}: {exc}")
            force_delete_sandbox(sbx)
    delete_api_key(key_id, marker)
    wait_until(lambda: assert_available_pool_sandbox_count(QUOTA_SMALL_TEMPLATE, minimum=2), timeout=180)


def assert_quota_http_create_rejected(api_key, template, marker):
    resp = requests.post(
        f"{api_url()}/sandboxes",
        json={
            "templateID": template,
            "timeout": 600,
            "metadata": {"quota_e2e": marker},
        },
        headers={**key_headers(api_key), "x-request-id": marker},
        timeout=30,
    )
    assert resp.status_code == 403, resp.text
    body = resp.json()
    assert body.get("code") == 403, body
    assert "quota" in str(body.get("message", "")).lower(), body
    assert body.get("message"), body


def assert_quota_create_rejected(api_key, template, marker, owned=None):
    try:
        sbx = create_sandbox_with_key(api_key, template, marker)
    except Exception as exc:
        text = str(exc).lower()
        assert "403" in text and "quota" in text, text
        return

    if owned is not None:
        track_sandbox(owned, sbx)
    else:
        try:
            sbx.kill()
            wait_until(lambda: assert_sandbox_gone(sbx), timeout=120)
        except Exception as exc:
            print(f"unexpected quota success cleanup ignored for {sbx.sandbox_id}: {exc}")
    raise AssertionError("quota create unexpectedly succeeded")


def create_sandbox_eventually_allowed(api_key, template, marker, timeout=120):
    box = {}

    def attempt():
        try:
            box["sandbox"] = create_sandbox_with_key(api_key, template, marker)
        except Exception as exc:
            if is_quota_rejection(exc):
                raise
            if is_transient_lifecycle_error(exc):
                raise AssertionError(str(exc))
            raise

    wait_until(attempt, timeout=timeout)
    return box["sandbox"]


def assert_quota_eventually_rejected(api_key, template, marker, owned, timeout=120):
    # Like assert_quota_create_rejected, but tolerant of a transient fail-open
    # window. After a Redis outage the admission circuit breaker stays open for
    # its cooldown (default 30s) and the first create within that window is
    # admitted fail-open even though Redis is reachable again. Retry until a
    # create is actually rejected. Any create that slips through is killed
    # immediately so it neither leaks nor drains the warm pool across retries;
    # if the kill fails it is tracked for the case-level cleanup instead.
    def attempt():
        try:
            sbx = create_sandbox_with_key(api_key, template, marker)
        except Exception as exc:
            text = str(exc).lower()
            assert "403" in text or "quota" in text, text
            return
        try:
            sbx.kill()
            wait_until(lambda: assert_sandbox_gone(sbx), timeout=120)
        except Exception as exc:
            track_sandbox(owned, sbx)
            print(f"fail-open create cleanup deferred for {getattr(sbx, 'sandbox_id', '<unknown>')}: {exc}")
        raise AssertionError("create still admitted; fail-open window not closed yet")

    wait_until(attempt, timeout=timeout)


def assert_redis_count(key_id, scope, expected):
    # Convert "Redis not reachable yet" into a retriable AssertionError so the
    # surrounding wait_until tolerates the window after a scale-to-0/scale-to-1
    # cycle while the fresh Redis pod is still coming up. redis_pod() already
    # raises AssertionError when no pod exists; this covers the exec-against-an-
    # unready-pod case (subprocess check=True -> CalledProcessError).
    try:
        actual = redis_hget(quota_sum_key(key_id, "sandbox.count"), scope)
    except subprocess.CalledProcessError as exc:
        raise AssertionError(f"redis not reachable yet: {exc}")
    assert actual == expected, f"count[{scope}]={actual}, expected={expected}"


def assert_redis_deleted(key_id):
    try:
        live_exists = redis_cli("EXISTS", quota_live_key(key_id))
        count_exists = redis_cli("EXISTS", quota_sum_key(key_id, "sandbox.count"))
        cpu_exists = redis_cli("EXISTS", quota_sum_key(key_id, "limits.cpu"))
        memory_exists = redis_cli("EXISTS", quota_sum_key(key_id, "limits.memory"))
    except subprocess.CalledProcessError as exc:
        raise AssertionError(f"redis not reachable yet: {exc}")
    assert (live_exists, count_exists, cpu_exists, memory_exists) == ("0", "0", "0", "0")


def assert_deleted_key_create_rejected(api_key, template, marker, owned):
    try:
        sbx = create_sandbox_with_key(api_key, template, marker)
    except Exception as exc:
        text = str(exc)
        assert "401" in text or "403" in text, text
        return
    track_sandbox(owned, sbx)
    raise AssertionError("deleted API key still accepted")


@contextmanager
def redis_stopped_temporarily():
    subprocess.run(["kubectl", "scale", f"deployment/{REDIS_DEPLOYMENT}", "-n", REDIS_NAMESPACE, "--replicas=0"], check=True)
    wait_until(assert_no_ready_redis_pods, timeout=120)
    try:
        yield
    finally:
        subprocess.run(["kubectl", "scale", f"deployment/{REDIS_DEPLOYMENT}", "-n", REDIS_NAMESPACE, "--replicas=1"], check=True)
        subprocess.run(
            ["kubectl", "wait", "--for=condition=available", f"deployment/{REDIS_DEPLOYMENT}", "-n", REDIS_NAMESPACE, "--timeout=120s"],
            check=False,
        )
        wait_until(assert_redis_ping, timeout=120)


@pytest.fixture(scope="session", autouse=True)
def quota_templates():
    limitranges = kubectl_json("get", "limitrange", "-n", "default")
    assert limitranges.get("items", []) == [], "quota E2E requires no LimitRange in default namespace"
    for asset in ("sandboxset-quota-small.yaml", "sandboxset-quota-nostock.yaml"):
        subprocess.run(["kubectl", "apply", "-f", str(ASSETS_DIR / asset)], check=True)
    assert_sandboxset_limits(QUOTA_SMALL_TEMPLATE, "500m", "512Mi")
    assert_sandboxset_limits(QUOTA_NOSTOCK_TEMPLATE, "500m", "512Mi")
    try:
        wait_until(lambda: assert_available_pool_sandbox_count(QUOTA_SMALL_TEMPLATE, minimum=2), timeout=180)
    except AssertionError:
        dump_quota_template_state()
        raise


def assert_sandboxset_limits(name, cpu, memory):
    data = kubectl_json("get", "sandboxset", name)
    limits = data["spec"]["template"]["spec"]["containers"][0]["resources"]["limits"]
    assert limits == {"cpu": cpu, "memory": memory}, f"{name} limits {limits} != {{'cpu': {cpu}, 'memory': {memory}}}"


def is_sandbox_ready(item):
    for cond in item.get("status", {}).get("conditions", []):
        if cond.get("type") == "Ready" and cond.get("status") == "True":
            return True
    return False


def assert_available_pool_sandbox_count(sandboxset_name, minimum):
    data = kubectl_json("get", "sbx")
    available = 0
    for item in data.get("items", []):
        metadata = item.get("metadata", {})
        labels = metadata.get("labels", {})
        if labels.get("agents.kruise.io/sandbox-template") != sandboxset_name:
            continue
        annotations = metadata.get("annotations", {})
        if metadata.get("deletionTimestamp"):
            continue
        if annotations.get("agents.kruise.io/owner") or annotations.get("agents.kruise.io/lock"):
            continue
        if item.get("status", {}).get("phase") != "Running":
            continue
        if not is_sandbox_ready(item):
            continue
        available += 1
    assert available >= minimum, f"{sandboxset_name} available pool count {available} < {minimum}"


@pytest.mark.parametrize(
    "case_name,quota,expected_quota",
    [
        ("absent", ABSENT_QUOTA, None),
        ("null", None, None),
        ("empty", {}, None),
        (
            "full",
            quota_spec(
                ("sandbox.count", "all", 5),
                ("limits.cpu", "all", 500),
                ("limits.memory", "all", 512),
                ("sandbox.count", "running", 2),
                ("limits.cpu", "running", 200),
                ("limits.memory", "running", 256),
            ),
            quota_spec(
                ("sandbox.count", "all", 5),
                ("limits.cpu", "all", 500),
                ("limits.memory", "all", 512),
                ("sandbox.count", "running", 2),
                ("limits.cpu", "running", 200),
                ("limits.memory", "running", 256),
            ),
        ),
    ],
)
def test_api_key_quota_create_list_static_response(sandbox_context, case_name, quota, expected_quota):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    try:
        resp = create_api_key(f"quota-static-{case_name}", marker, quota=quota)
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        assert_static_quota_response(body, expected_quota)

        listed = requests.get(f"{api_url()}/api-keys", headers=admin_headers(marker))
        assert listed.status_code == 200, listed.text
        match = next(item for item in listed.json() if item["id"] == created_id)
        assert_static_quota_response(match, expected_quota)

        if expected_quota is None:
            sbx = track_sandbox(owned, sandbox_context.add(create_sandbox_eventually_allowed(body["key"], QUOTA_SMALL_TEMPLATE, marker)))
            assert sbx.get_info().state == SandboxState.RUNNING
    finally:
        cleanup_quota_case(created_id, owned, marker)


@pytest.mark.parametrize(
    "case_name,raw_body,expected_fragment",
    [
        ("negative_count", {"name": "x", "quota": quota_spec(("sandbox.count", "all", -1))}, "limit"),
        ("negative_cpu", {"name": "x", "quota": quota_spec(("limits.cpu", "running", -1))}, "limit"),
        ("negative_memory", {"name": "x", "quota": quota_spec(("limits.memory", "all", -1))}, "limit"),
        ("unsupported_scope", {"name": "x", "quota": quota_spec(("sandbox.count", "template:code-interpreter", 1))}, "scope"),
        ("unsupported_dimension", {"name": "x", "quota": quota_spec(("gpu", "all", 1))}, "dimension"),
        ("nested_wire_all", {"name": "x", "quota": {"all": {"sandbox.count": 1}}}, "unknown"),
        ("nested_wire_running", {"name": "x", "quota": {"running": {}}}, "unknown"),
    ],
)
def test_api_key_quota_validation_rejects_invalid_values(case_name, raw_body, expected_fragment):
    marker = str(uuid.uuid4())
    raw_body["name"] = f"quota-invalid-{case_name}-{marker}"
    resp = requests.post(f"{api_url()}/api-keys", json=raw_body, headers=admin_headers(marker))
    assert resp.status_code == 400, resp.text
    assert expected_fragment in resp.text.lower()


def test_api_key_quota_zero_limit_is_valid_and_blocks_create(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    try:
        resp = create_api_key("quota-zero", marker, quota=quota_spec(("sandbox.count", "all", 0)))
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        limited_key = body["key"]

        try:
            sbx = sandbox_context.add(create_sandbox_with_key(limited_key, QUOTA_SMALL_TEMPLATE, marker))
        except Exception as exc:
            text = str(exc).lower()
            assert "403" in text and "quota" in text, text
        else:
            track_sandbox(owned, sbx)
            raise AssertionError("hard-zero quota unexpectedly allowed a sandbox create")
    finally:
        cleanup_quota_case(created_id, owned, marker)


def test_count_all_limit_blocks_then_delete_releases(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    try:
        resp = create_api_key("quota-count-all", marker, quota=quota_spec(("sandbox.count", "all", 1)))
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        first = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        assert first.get_info().state == SandboxState.RUNNING

        assert_quota_http_create_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker)
        assert_quota_create_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker)

        first.kill()
        wait_until(lambda: assert_sandbox_gone(first), timeout=120)

        wait_until(lambda: assert_available_pool_sandbox_count(QUOTA_SMALL_TEMPLATE, minimum=1), timeout=180)
        second = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        assert second.get_info().state == SandboxState.RUNNING
    finally:
        cleanup_quota_case(created_id, owned, marker)


def test_count_all_limit_still_counts_paused_sandbox(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    try:
        resp = create_api_key("quota-count-all-paused", marker, quota=quota_spec(("sandbox.count", "all", 1)))
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        sbx = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        pause_sandbox(sbx)
        wait_until(lambda: assert_sandbox_state(sbx, SandboxState.PAUSED), timeout=120)

        assert_quota_create_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker)
    finally:
        cleanup_quota_case(created_id, owned, marker)


def test_count_running_limit_pause_frees_running_scope(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    try:
        resp = create_api_key("quota-count-running", marker, quota=quota_spec(("sandbox.count", "running", 1)))
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        first = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        assert_quota_create_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker)

        pause_sandbox(first)
        wait_until(lambda: assert_sandbox_state(first, SandboxState.PAUSED), timeout=120)

        second = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        assert second.get_info().state == SandboxState.RUNNING
    finally:
        cleanup_quota_case(created_id, owned, marker)


def test_running_resume_can_exceed_limit_and_blocks_new_creates(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    try:
        resp = create_api_key("quota-running-resume", marker, quota=quota_spec(("sandbox.count", "running", 1)))
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        first = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        pause_sandbox(first)
        wait_until(lambda: assert_sandbox_state(first, SandboxState.PAUSED), timeout=120)

        second = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        resume_sandbox(first)
        wait_until(lambda: assert_sandbox_state(first, SandboxState.RUNNING), timeout=120)

        assert second.get_info().state == SandboxState.RUNNING
        assert first.get_info().state == SandboxState.RUNNING
        assert_quota_create_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker)
    finally:
        cleanup_quota_case(created_id, owned, marker)


def test_cpu_all_limit_uses_millicore_footprint(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    try:
        resp = create_api_key("quota-cpu-all", marker, quota=quota_spec(("limits.cpu", "all", 750)))
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        sbx = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        assert sbx.get_info().state == SandboxState.RUNNING
        assert_quota_create_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker)
    finally:
        cleanup_quota_case(created_id, owned, marker)


def test_memory_all_limit_uses_mib_footprint(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    try:
        resp = create_api_key("quota-memory-all", marker, quota=quota_spec(("limits.memory", "all", 768)))
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        sbx = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        assert sbx.get_info().state == SandboxState.RUNNING
        assert_quota_create_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker)
    finally:
        cleanup_quota_case(created_id, owned, marker)


def test_running_resource_limits_pause_and_resume(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    try:
        resp = create_api_key(
            "quota-running-resources",
            marker,
            quota=quota_spec(("limits.cpu", "running", 750), ("limits.memory", "running", 768)),
        )
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        first = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        assert_quota_create_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker)

        pause_sandbox(first)
        wait_until(lambda: assert_sandbox_state(first, SandboxState.PAUSED), timeout=120)

        second = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        resume_sandbox(first)
        wait_until(lambda: assert_sandbox_state(first, SandboxState.RUNNING), timeout=120)
        assert second.get_info().state == SandboxState.RUNNING
        assert_quota_create_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker)
    finally:
        cleanup_quota_case(created_id, owned, marker)


def test_no_stock_path_stamps_owner_lockstring_and_counts_quota(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    try:
        resp = create_api_key(
            "quota-no-stock-lock",
            marker,
            quota=quota_spec(("sandbox.count", "all", 1), ("limits.cpu", "all", 2000), ("limits.memory", "all", 2048)),
        )
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        sbx = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_NOSTOCK_TEMPLATE, marker))
        cr = kubectl_json("get", "sbx", sandbox_name(sbx))
        annotations = cr["metadata"].get("annotations", {})
        assert annotations.get("agents.kruise.io/owner") == created_id
        assert annotations.get("agents.kruise.io/lock")
        assert_quota_create_rejected(api_key, QUOTA_NOSTOCK_TEMPLATE, marker)
    finally:
        cleanup_quota_case(created_id, owned, marker)


@pytest.mark.skipif(QUOTA_E2E_PROFILE != "redis-clone", reason="requires checkpoint clone quota profile")
def test_checkpoint_clone_path_stamps_owner_and_lockstring(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    api_key = None
    checkpoint_id = None
    owned = []
    try:
        resp = create_api_key(
            "quota-checkpoint-clone-lock",
            marker,
            quota=quota_spec(("sandbox.count", "all", 2), ("limits.cpu", "all", 1000), ("limits.memory", "all", 1024)),
        )
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        source = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        checkpoint_id = create_snapshot_with_key(api_key, source, marker)
        clone = track_sandbox(
            owned,
            create_sandbox_eventually_allowed(api_key, checkpoint_id, f"{marker}-clone", timeout=240),
        )

        cr = kubectl_json("get", "sbx", sandbox_name(clone))
        annotations = cr["metadata"].get("annotations", {})
        assert annotations.get("agents.kruise.io/owner") == created_id
        assert annotations.get("agents.kruise.io/lock")
        assert_quota_create_rejected(api_key, checkpoint_id, f"{marker}-clone-over")
    finally:
        if checkpoint_id and api_key:
            delete_template_or_snapshot(api_key, checkpoint_id, marker)
        cleanup_quota_case(created_id, owned, marker)


def test_concurrent_creates_do_not_exceed_count_limit(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    successes = []
    owned = []
    try:
        resp = create_api_key("quota-concurrent", marker, quota=quota_spec(("sandbox.count", "all", 3)))
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        assert_api_key_usable(api_key, marker)
        # Pre-warm the pool to at least the count limit so every quota-admitted
        # create is a warm claim. Otherwise a create-on-no-stock cold start could
        # exceed the SDK request timeout and land in the unexpected-error bucket,
        # making the strict `successes == 3, quota_misses == 7` assertion flaky.
        wait_until(lambda: assert_available_pool_sandbox_count(QUOTA_SMALL_TEMPLATE, minimum=3), timeout=180)

        def attempt(i):
            try:
                return ("ok", create_sandbox_with_key(api_key, QUOTA_SMALL_TEMPLATE, f"{marker}-{i}"))
            except Exception as exc:
                text = str(exc).lower()
                if "403" in text or "quota" in text:
                    return ("quota", text)
                return ("error", text)

        with ThreadPoolExecutor(max_workers=10) as pool:
            results = [future.result() for future in as_completed([pool.submit(attempt, i) for i in range(10)])]

        successes = [value for status, value in results if status == "ok"]
        quota_misses = [value for status, value in results if status == "quota"]
        unexpected = [value for status, value in results if status == "error"]

        for sbx in successes:
            track_sandbox(owned, sbx)
            sandbox_context.add(sbx)
        assert len(successes) == 3, results
        assert len(quota_misses) == 7, results
        assert not unexpected, unexpected
    finally:
        cleanup_quota_case(created_id, owned, marker)


def test_quota_miss_returns_e2b_error_body_and_does_not_lock_pooled_sandbox():
    marker = str(uuid.uuid4())
    created_id = None
    owned = []
    try:
        resp = create_api_key("quota-pool-return", marker, quota=quota_spec(("sandbox.count", "all", 0)))
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        assert_quota_http_create_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker)

        data = kubectl_json("get", "sbx")
        for item in data.get("items", []):
            annotations = item.get("metadata", {}).get("annotations", {})
            if annotations.get("agents.kruise.io/owner") == created_id:
                owned.append(item["metadata"]["name"])
        assert owned == []
    finally:
        for name in owned:
            subprocess.run(
                ["kubectl", "delete", "sbx", name, "--ignore-not-found=true", "--wait=false"],
                check=False,
            )
        delete_api_key(created_id, marker)


def test_api_key_quota_create_requires_admin_permission():
    marker = str(uuid.uuid4())
    team_name = f"quota-team-{marker[:8]}"
    source_key_id = None
    allowed_key_id = None
    target_key_id = None
    try:
        subprocess.run(["kubectl", "create", "namespace", team_name], check=True)
        source = create_api_key("quota-non-admin-source", marker, team_name=team_name)
        assert source.status_code == 201, source.text
        source_body = source.json()
        source_key_id = source_body["id"]

        allowed = create_api_key(
            "quota-non-admin-allowed",
            marker,
            headers=key_headers(source_body["key"], marker),
        )
        assert allowed.status_code == 201, allowed.text
        allowed_key_id = allowed.json()["id"]

        resp = create_api_key(
            "quota-non-admin-target",
            marker,
            quota=quota_spec(("sandbox.count", "all", 1)),
            headers=key_headers(source_body["key"], marker),
        )
        if resp.status_code == 201:
            target_key_id = resp.json().get("id")
        assert resp.status_code == 403, resp.text
    finally:
        delete_api_key(target_key_id, marker)
        delete_api_key(allowed_key_id, marker)
        delete_api_key(source_key_id, marker)
        subprocess.run(["kubectl", "delete", "namespace", team_name, "--ignore-not-found=true", "--wait=false"], check=False)


def test_api_key_quota_patch_is_not_supported():
    marker = str(uuid.uuid4())
    created_id = None
    expected_quota = quota_spec(("sandbox.count", "all", 2))
    try:
        resp = create_api_key("quota-no-patch", marker, quota=expected_quota)
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        assert_static_quota_response(body, expected_quota)

        patch = requests.patch(
            f"{api_url()}/api-keys/{created_id}",
            json={"quota": quota_spec(("sandbox.count", "all", 99))},
            headers=admin_headers(marker),
        )
        assert 400 <= patch.status_code < 500, patch.text

        listed = requests.get(f"{api_url()}/api-keys", headers=admin_headers(marker))
        assert listed.status_code == 200, listed.text
        match = next(item for item in listed.json() if item["id"] == created_id)
        assert_static_quota_response(match, expected_quota)
    finally:
        delete_api_key(created_id, marker)


@pytest.mark.skipif(not redis_faults_enabled(), reason="requires Redis inspection")
def test_api_key_delete_cleans_quota_state_but_keeps_sandbox(sandbox_context):
    marker = sandbox_context.request_id
    sbx_name = None
    owned = []
    resp = create_api_key("quota-delete-cleanup", marker, quota=quota_spec(("sandbox.count", "all", 2)))
    assert resp.status_code == 201, resp.text
    body = resp.json()
    key_id = body["id"]
    api_key = body["key"]

    try:
        sbx = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        sbx_name = sandbox_name(sbx)
        wait_until(lambda: assert_redis_count(key_id, "all", 1), timeout=120)

        # This case deletes the key first on purpose to verify quota cleanup does not reap an existing sandbox CR.
        delete_resp = requests.delete(f"{api_url()}/api-keys/{key_id}", headers=admin_headers(marker))
        assert delete_resp.status_code == 204, delete_resp.text
        cr = kubectl_json("get", "sbx", sbx_name)
        assert cr["metadata"]["name"] == sbx_name

        wait_until(lambda: assert_redis_deleted(key_id), timeout=120)
        wait_until(lambda: assert_api_key_rejected(api_key, marker), timeout=120)
        wait_until(
            lambda: assert_deleted_key_create_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker, owned),
            timeout=120,
        )
    finally:
        if sbx_name:
            force_delete_sandbox(sbx_name)
        for sbx in list(owned):
            force_delete_sandbox(sbx)
        owned.clear()


@pytest.mark.skipif(not redis_faults_enabled(), reason="requires Redis inspection")
def test_antidrift_releases_non_manager_deleted_sandbox(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    try:
        resp = create_api_key("quota-antidrift-release", marker, quota=quota_spec(("sandbox.count", "all", 1)))
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        sbx = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        wait_until(lambda: assert_redis_count(created_id, "all", 1), timeout=120)

        subprocess.run(["kubectl", "delete", "sbx", sandbox_name(sbx), "--wait=false"], check=True)
        try:
            wait_until(lambda: assert_sandbox_gone(sbx), timeout=120)
            owned.remove(sbx)
        except Exception:
            pass
        wait_until(lambda: assert_redis_count(created_id, "all", 0), timeout=240)

        replacement = track_sandbox(
            owned,
            create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, f"{marker}-replacement"),
        )
        assert replacement.get_info().state == SandboxState.RUNNING
        wait_until(lambda: assert_redis_count(created_id, "all", 1), timeout=120)
    finally:
        cleanup_quota_case(created_id, owned, marker)


@pytest.mark.skipif(not redis_faults_enabled(), reason="requires Redis inspection")
def test_redis_data_loss_rebuilds_from_live_crs_and_reenforces(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    try:
        resp = create_api_key("quota-redis-loss", marker, quota=quota_spec(("sandbox.count", "all", 2)))
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        first = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        second = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        assert first.get_info().state == SandboxState.RUNNING
        assert second.get_info().state == SandboxState.RUNNING
        wait_until(lambda: assert_redis_count(created_id, "all", 2), timeout=120)

        redis_cli("DEL", quota_live_key(created_id))
        redis_cli("DEL", quota_sum_key(created_id, "sandbox.count"))
        redis_cli("DEL", quota_sum_key(created_id, "limits.cpu"))
        redis_cli("DEL", quota_sum_key(created_id, "limits.memory"))

        wait_until(lambda: assert_redis_count(created_id, "all", 2), timeout=240)
        assert_quota_http_create_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker)
        assert_quota_create_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker)
    finally:
        cleanup_quota_case(created_id, owned, marker)


@pytest.mark.skipif(QUOTA_E2E_PROFILE != "no-redis", reason="requires no-Redis quota profile")
def test_quota_is_accepted_and_unenforced_without_redis(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    expected_quota = quota_spec(("sandbox.count", "all", 0))
    try:
        resp = create_api_key("quota-no-redis", marker, quota=expected_quota)
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]
        assert_static_quota_response(body, expected_quota)

        listed = requests.get(f"{api_url()}/api-keys", headers=admin_headers(marker))
        assert listed.status_code == 200, listed.text
        match = next(item for item in listed.json() if item["id"] == created_id)
        assert_static_quota_response(match, expected_quota)

        sbx = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        assert sbx.get_info().state == SandboxState.RUNNING
    finally:
        cleanup_quota_case(created_id, owned, marker)


@pytest.mark.skipif(not redis_faults_enabled(), reason="requires Redis fault profile")
def test_redis_outage_fails_open_then_reenforces_after_rebuild(sandbox_context):
    marker = sandbox_context.request_id
    created_id = None
    owned = []
    try:
        resp = create_api_key("quota-redis-outage", marker, quota=quota_spec(("sandbox.count", "all", 1)))
        assert resp.status_code == 201, resp.text
        body = resp.json()
        created_id = body["id"]
        api_key = body["key"]

        first = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, marker))
        assert first.get_info().state == SandboxState.RUNNING
        wait_until(lambda: assert_redis_count(created_id, "all", 1), timeout=120)
        wait_until(lambda: assert_available_pool_sandbox_count(QUOTA_SMALL_TEMPLATE, minimum=5), timeout=240)

        with redis_stopped_temporarily():
            started = time.time()
            extra = []
            for i in range(4):
                extra.append(
                    track_sandbox(owned, create_sandbox_with_key(api_key, QUOTA_SMALL_TEMPLATE, f"{marker}-outage-{i}"))
                )
            assert time.time() - started < 60
            assert len(extra) == 4

        wait_until(lambda: assert_redis_count(created_id, "all", 5), timeout=240)
        # Right after the outage the admission breaker may still be open (default
        # 30s cooldown), admitting the first create fail-open even though Redis is
        # back and the sums are rebuilt. Retry until enforcement re-engages.
        assert_quota_eventually_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker, owned)

        extras = owned[1:]
        for sbx in extras:
            sbx.kill()
        for sbx in extras:
            try:
                wait_until(lambda sbx=sbx: assert_sandbox_gone(sbx), timeout=120)
            except Exception:
                force_delete_sandbox(sbx)
        owned = owned[:1]
        wait_until(lambda: assert_redis_count(created_id, "all", 1), timeout=180)
        assert_quota_eventually_rejected(api_key, QUOTA_SMALL_TEMPLATE, marker, owned)

        first.kill()
        try:
            wait_until(lambda: assert_sandbox_gone(first), timeout=120)
            owned.remove(first)
        except Exception:
            pass
        wait_until(lambda: assert_redis_count(created_id, "all", 0), timeout=120)
        replacement = track_sandbox(owned, create_sandbox_eventually_allowed(api_key, QUOTA_SMALL_TEMPLATE, f"{marker}-replacement"))
        assert replacement.get_info().state == SandboxState.RUNNING
    finally:
        cleanup_quota_case(created_id, owned, marker)
