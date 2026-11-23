# Copyright 2026 The OpenKruise Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Read existing quota evidence and bound the single E2B-style recovery wave."""

import math
import re
import subprocess
import time

import httpx
from opensandbox.api.lifecycle.api.sandboxes import delete_sandboxes_sandbox_id
from opensandbox.api.lifecycle.errors import UnexpectedStatus
import requests


MANAGER_NAMESPACE = "sandbox-system"
MANAGER_SELECTOR = "app.kubernetes.io/name=sandbox-manager"
METRICS = {
    "fail_open": ("sandbox_manager_quota_acquire_total", "result", "fail_open"),
    "allowed": ("sandbox_manager_quota_acquire_total", "result", "allowed"),
    "rejected": ("sandbox_manager_quota_acquire_total", "result", "rejected"),
    "backend_errors": ("sandbox_manager_quota_backend_errors_total", "operation", "acquire"),
    "breaker_open": ("sandbox_manager_quota_breaker_state_total", "state", "open"),
    "breaker_closed": ("sandbox_manager_quota_breaker_state_total", "state", "closed"),
}


def counter_value(metrics_text, metric, label, value):
    expected = f'{metric}{{{label}="{value}"}}'
    samples = [line.split() for line in metrics_text.splitlines()
               if line.startswith(metric) and not line.startswith(metric + "_")]
    result = None
    for sample in samples:
        assert len(sample) in (2, 3), f"malformed quota metric: {sample}"
        assert re.fullmatch(r'[a-zA-Z_:][a-zA-Z_0-9:]*\{[a-z_]+="[^"\\]*"\}', sample[0]), sample
        number = float(sample[1])
        assert math.isfinite(number) and number >= 0, f"invalid quota counter: {sample}"
        if sample[0] == expected:
            assert result is None, f"duplicate quota counter: {expected}"
            result = number
    # CounterVec families/labels are emitted lazily, just as in E2B's reader.
    return 0.0 if result is None else result


def remaining(deadline):
    value = deadline - time.monotonic()
    assert value > 0, "quota breaker recovery exceeded its 180-second deadline"
    return value


def metrics_snapshot(kubectl, kubectl_text, deadline=None):
    def bounded():
        return {"timeout": min(30, remaining(deadline))} if deadline is not None else {}

    pods = kubectl("get", "pods", "-n", MANAGER_NAMESPACE, "-l", MANAGER_SELECTOR,
                   **bounded())["items"]
    assert pods, "no sandbox-manager pod found for quota metrics"
    counters = {}
    for pod in sorted(pods, key=lambda item: item["metadata"]["name"]):
        meta = pod["metadata"]
        status = next((item for item in pod.get("status", {}).get("containerStatuses", [])
                       if item["name"] == "controller"), None)
        assert status is not None, f"controller status missing for {meta['name']}"
        identity = (meta["name"], meta["uid"], status["restartCount"])
        path = f"/api/v1/namespaces/{MANAGER_NAMESPACE}/pods/{meta['name']}:8080/proxy/metrics"
        text = kubectl_text("get", "--raw", path, **bounded())
        assert any(line.startswith("# TYPE ") for line in text.splitlines()), (
            f"manager {meta['name']} did not return Prometheus metrics")
        counters[identity] = {name: counter_value(text, *labels) for name, labels in METRICS.items()}
    return counters


def instance_delta(before, after):
    assert before.keys() == after.keys(), (
        f"sandbox-manager pods/restarts changed: before={list(before)}, after={list(after)}")
    result = {instance: {name: after[instance][name] - values[name] for name in METRICS}
              for instance, values in before.items()}
    assert all(value >= 0 for counters in result.values() for value in counters.values()), (
        f"quota counters decreased: {result}")
    return result


def metrics_delta(before, after):
    changes = instance_delta(before, after)
    return {name: sum(counters[name] for counters in changes.values()) for name in METRICS}


def requires_recovery(successes, misses, before, after, burst):
    delta = metrics_delta(before, after)
    diagnostics = f"successes={successes}, quota_misses={misses}, metric_delta={delta}"
    print("OpenSandbox concurrent quota:", diagnostics)
    if successes == 3 and misses == 7:
        return False
    assert successes > 3 and successes + misses == 10, diagnostics
    assert delta["fail_open"] >= successes - 3, f"oversell lacks fail-open evidence: {diagnostics}"
    assert burst == 0, f"second quota burst must be exactly 3/7: {diagnostics}"
    return True


def recover_admission(create, client, kubectl, kubectl_text, before, after, assert_rejected):
    targets = {instance for instance, counters in instance_delta(before, after).items()
               if counters["fail_open"] > 0}
    assert targets, "fail-open recovery requires affected manager pods"
    deadline = time.monotonic() + 180
    probes = []
    needs_cooldown = True
    previous = after

    def observe():
        nonlocal previous
        current = metrics_snapshot(kubectl, kubectl_text, deadline)
        instance_delta(previous, current)
        previous = current
        return current

    while True:
        current = observe()
        changes = instance_delta(after, current)
        recovered = {instance for instance in targets if changes[instance]["rejected"] > 0}
        if recovered == targets:
            print("OpenSandbox quota recovery:", {str(key): value for key, value in changes.items()})
            return probes
        if needs_cooldown:
            assert remaining(deadline) >= 35, f"quota recovery lacks cooldown time: {changes}"
            time.sleep(35)
            needs_cooldown = False
            continue
        probe_before = observe()
        # The generated SDK does not expose per-operation timeout arguments.
        # Bound all four HTTP phases by the remaining recovery budget instead.
        transport = client.get_httpx_client()
        original_timeout = transport.timeout
        transport.timeout = httpx.Timeout(min(30, remaining(deadline) / 4))
        try:
            response = create({"timeout": 600}, client=client, expected_status=(202, 403))
        finally:
            transport.timeout = original_timeout
        if response.status_code == 202:
            probes.append(response.parsed.to_dict())
        else:
            assert_rejected(response)
        probe_after = observe()
        delta = metrics_delta(probe_before, probe_after)
        if response.status_code == 202:
            assert delta["allowed"] + delta["fail_open"] >= 1, (
                f"quota probe succeeded without admission evidence: {delta}")
        needs_cooldown = delta["fail_open"] > 0
        if not needs_cooldown:
            time.sleep(min(1, remaining(deadline)))


def cleanup_wave(created, client, config, kubectl):
    errors = []
    with requests.Session() as http:
        for item in reversed(created):
            sandbox_id = item["id"]
            try:
                response = delete_sandboxes_sandbox_id.sync_detailed(sandbox_id, client=client)
                if response.status_code in (204, 404):
                    continue
                errors.append(f"OpenSandbox delete {sandbox_id}: HTTP {response.status_code}")
            except (httpx.HTTPError, UnexpectedStatus) as exc:
                errors.append(f"OpenSandbox delete {sandbox_id}: {type(exc).__name__}: {exc}")
            # Preserve the SDK failure even when native fallback prevents a leak.
            try:
                response = http.delete(f"{config['native_url']}/sandboxes/{sandbox_id}",
                                       headers={"X-API-Key": client.token}, timeout=(5, 30))
                if response.status_code not in (204, 404):
                    errors.append(f"native delete {sandbox_id}: HTTP {response.status_code}")
            except requests.RequestException as exc:
                errors.append(f"native delete {sandbox_id}: {type(exc).__name__}: {exc}")
    for item in created:
        namespace, name = item["metadata"]["opensandbox.kruise.io/sandbox-resource"].split("/", 1)
        deadline = time.monotonic() + 120
        try:
            while True:
                budget = deadline - time.monotonic()
                assert budget > 0, f"CR {namespace}/{name} remains after cleanup"
                boxes = kubectl("get", "sandboxes", "-n", namespace,
                                timeout=min(30, budget))["items"]
                if not any(box["metadata"]["name"] == name for box in boxes):
                    break
                assert time.monotonic() < deadline, f"CR {namespace}/{name} remains after cleanup"
                time.sleep(1)
        except (AssertionError, ValueError, OSError, subprocess.TimeoutExpired) as exc:
            errors.append(str(exc))
    assert not errors, "; ".join(errors)


def wait_for_zero_count(kubectl, kubectl_text, key_id):
    deadline = time.monotonic() + 120
    while True:
        budget = deadline - time.monotonic()
        assert budget > 0, "Redis count did not return to zero within 120 seconds"
        pods = kubectl("get", "pods", "-n", "sandbox-system", "-l",
                       "app.kubernetes.io/name=redis", timeout=min(30, budget))["items"]
        assert len(pods) == 1, f"expected one Redis pod, got {len(pods)}"
        value = kubectl_text("exec", "-n", "sandbox-system", pods[0]["metadata"]["name"],
                             "--", "redis-cli", "--raw", "HGET",
                             f"q:sum:{{{key_id}}}:sandbox.count", "all",
                             timeout=min(30, max(0, deadline - time.monotonic()))).strip()
        # Redis nil (missing hash/field) is an empty raw result, equivalent to 0.
        assert not value or re.fullmatch(r"[0-9]+", value), f"invalid Redis count: {value!r}"
        if not value or int(value) == 0:
            return
        assert time.monotonic() < deadline, f"Redis count[all]={value}, expected=0"
        time.sleep(1)
