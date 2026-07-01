"""Utility functions and fixtures for E2B tests."""
import json
import subprocess
import time
from typing import List, Optional

import pytest
from e2b.sandbox.sandbox_api import SandboxInfo, SandboxQuery
from e2b_code_interpreter import SandboxState
from e2b_code_interpreter import Sandbox

import logging

logger = logging.getLogger(__name__)



def connect_sandbox(sbx: Sandbox, timeout: Optional[int] = None) -> Sandbox | None:
    """Connect to a sandbox with retry logic for pausing state.

    If the sandbox is pausing, will retry up to 30 times with 2 second intervals.

    Args:
        sbx: The Sandbox instance to connect to.
        timeout: Optional timeout parameter to pass to connect().

    Returns:
        The connected Sandbox instance.

    Raises:
        Exception: If connection fails with error other than pausing state,
                   or if pausing state persists after 30 retries.
    """
    max_retries = 30
    retry_interval = 2  # seconds

    for attempt in range(max_retries):
        try:
            if timeout is not None:
                return sbx.connect(timeout=timeout)
            else:
                return sbx.connect()
        except Exception as e:
            error_msg = str(e)
            # Server-side pausing-state errors. The legacy phrasing predates
            # commit a83587ed (#358); the current phrasing comes from
            # IsSandboxResumable returning "SandboxIsPausing".
            is_pausing = (
                "sandbox is pausing, please wait a moment and try again" in error_msg
                or "sandbox is not resumable, reason: SandboxIsPausing" in error_msg
            )
            if is_pausing:
                if attempt < max_retries - 1:
                    logger.debug(
                        "Sandbox is pausing, waiting %ds before retry... (attempt %d/%d)",
                        retry_interval, attempt + 1, max_retries,
                    )
                    time.sleep(retry_interval)
                    continue
                else:
                    raise Exception(f"Sandbox still pausing after {max_retries} retries") from e
            else:
                # Other errors should be raised immediately
                raise
    return None


def run_code_sandbox(sbx: Sandbox, code: str, **kwargs):
    """Run code in a sandbox with retry logic.

    Args:
        sbx: The Sandbox instance to run code in.
        code: The code to execute.
        **kwargs: Additional arguments to pass to run_code (e.g., context, on_result).

    Returns:
        The execution result from run_code.

    Raises:
        Exception: If code execution fails after all retries.
    """
    max_retries = 5
    retry_interval = 5  # seconds

    for attempt in range(max_retries):
        try:
            return sbx.run_code(code, request_timeout=120., **kwargs)
        except Exception as e:
            if attempt < max_retries - 1:
                logger.warning(
                    "Failed to run code (attempt %d/%d): %s",
                    attempt + 1, max_retries, e,
                )
                logger.debug("Retrying in %ds...", retry_interval)
                time.sleep(retry_interval)
                continue
            else:
                logger.error("Failed to run code after %d attempts", max_retries)
                raise
    return None


def list_sandbox(query: SandboxQuery = None, namespace: str = "default") -> List[SandboxInfo]:
    """List sandboxes matching the given query with full pagination.

    By default, only returns RUNNING sandboxes belonging to the current test
    namespace. This avoids fixture race conditions caused by:
    - PAUSED sandboxes from pause_resume tests on other workers
    - Warm-pool sandboxes from other namespaces

    Args:
        query: SandboxQuery to filter sandboxes. If provided, overrides default filters.
        namespace: Namespace to filter by (prefix match on sandbox_id).
            Defaults to test_namespace. Pass None to skip namespace filtering.

    Returns:
        List of matching SandboxInfo objects.
    """
    # Default: only RUNNING state (exclude PAUSED sandboxes from pause_resume tests)
    if query is None:
        query = SandboxQuery(state=[SandboxState.RUNNING])

    paginator = Sandbox.list(query=query)
    sandboxes = paginator.next_items()

    while paginator.has_next:
        items = paginator.next_items()
        sandboxes.extend(items)

    # Post-filter by namespace prefix (sandbox_id format: namespace--pod-name)
    ns = namespace
    if ns:
        prefix = f"{ns}--"
        sandboxes = [sb for sb in sandboxes if sb.sandbox_id.startswith(prefix)]

    return sandboxes


def _force_kill_remaining_sandboxes(test_namespace: str):
    """Kill all remaining sandboxes in the test namespace to prevent cascade failures.

    Uses both SDK kill and kubectl delete to ensure cleanup — SDK kill may
    silently fail for sandboxes in non-running states (e.g. creating/dead).
    """
    try:
        remaining = list_sandbox(namespace=test_namespace)
        if not remaining:
            return
        logger.warning(
            "Force-killing %d remaining sandbox(es) to prevent cascade...",
            len(remaining),
        )
        for sbx_info in remaining:
            full_id = sbx_info.sandbox_id
            short_id = full_id.split("--")[1] if "--" in full_id else full_id
            try:
                Sandbox.kill(full_id)
                logger.info("  SDK kill %s", full_id)
            except Exception as e:
                logger.warning("  SDK kill %s failed: %s", full_id, e)
            try:
                kubectl("delete", "sbx", short_id, "-n", test_namespace,
                        "--ignore-not-found=true", "--timeout=10s")
                logger.info("  kubectl delete %s", short_id)
            except Exception:
                pass
    except Exception as e:
        logger.warning("Force-kill failed: %s", e)


@pytest.fixture(autouse=True)
def wait_for_sandbox(config):
    """BeforeEach: Wait for sandbox cleanup, then check warm pool Ready count.

    If sandboxes are not cleaned up within the initial timeout, force-kill them
    to prevent cascade failures across subsequent tests.

    When e2e_debug is set, skip the cleanup wait entirely so failed sandboxes
    are preserved for investigation.
    """
    if config.debug:
        logger.info("E2E_DEBUG: skipping sandbox cleanup wait (preserving resources)")
        yield
        return

    timeout = config.sandbox_cleanup_timeout
    interval = 2
    start_time = time.time()

    # Phase 1: Wait for sandboxes to be cleaned up naturally
    while time.time() - start_time < timeout:
        try:
            sandboxes = list_sandbox(namespace=config.test_namespace)
        except Exception as e:
            logger.error("list_sandbox() failed: %s", e)
            logger.info("=== Sandbox Manager Logs ===")
            try:
                kubectl("logs", "-n", "sandbox-system", "-l", "app.kubernetes.io/name=sandbox-manager", "--tail=100")
            except Exception:
                logger.warning("(failed to fetch sandbox-manager logs)")
            raise
        if len(sandboxes) == 0:
            logger.debug("No sandboxes running, proceeding to check Ready conditions")
            break
        logger.debug("Waiting for %d sandbox(es) to be cleaned up...", len(sandboxes))
        time.sleep(interval)

    else:
        # Phase 2: Timeout — force-kill remaining sandboxes instead of failing
        _force_kill_remaining_sandboxes(config.test_namespace)
        time.sleep(5)
        still_remaining = list_sandbox(namespace=config.test_namespace)
        if still_remaining:
            raise TimeoutError(
                f"{len(still_remaining)} sandbox(es) still running after force-kill"
            )

    # Wait for at least 2 Ready sandboxes
    ready_start_time = time.time()
    while time.time() - ready_start_time < timeout:
        try:
            result = subprocess.run(
                ["kubectl", "get", "sbx", "-o", "json", "-n", config.test_namespace],
                capture_output=True,
                text=True,
                check=True
            )
            sbx_list = json.loads(result.stdout)

            ready_count = 0
            for sbx in sbx_list.get("items", []):
                conditions = sbx.get("status", {}).get("conditions", [])
                for cond in conditions:
                    if cond.get("type") == "Ready" and cond.get("status") == "True":
                        ready_count += 1
                        break

            total_count = len(sbx_list.get("items", []))
            logger.debug(
                "Found %d Ready sandbox(es) out of %d total",
                ready_count, total_count,
            )

            if ready_count >= 2:
                logger.debug("Required Ready sandboxes available, proceeding with test")
                break

            logger.debug("Waiting for more Ready sandboxes (%d/2)...", ready_count)
            time.sleep(interval)

        except subprocess.CalledProcessError as e:
            raise RuntimeError(f"Failed to get sandboxes via kubectl: {e.stderr}")
        except json.JSONDecodeError as e:
            raise RuntimeError(f"Failed to parse kubectl output: {e}")
    else:
        # Timeout reached without enough Ready sandboxes
        # Get final state and describe not-ready sandboxes
        try:
            result = subprocess.run(
                ["kubectl", "get", "sbx", "-o", "json", "-n", config.test_namespace],
                capture_output=True,
                text=True,
                check=True
            )
            sbx_list = json.loads(result.stdout)

            ready_sandboxes = []
            not_ready_sandboxes = []

            for sbx in sbx_list.get("items", []):
                sbx_name = sbx.get("metadata", {}).get("name", "unknown")
                conditions = sbx.get("status", {}).get("conditions", [])
                is_ready = False
                for cond in conditions:
                    if cond.get("type") == "Ready" and cond.get("status") == "True":
                        is_ready = True
                        break

                if is_ready:
                    ready_sandboxes.append(sbx_name)
                else:
                    not_ready_sandboxes.append(sbx_name)

            logger.error(
                "Timeout: Only %d Ready sandbox(es), expected at least 2",
                len(ready_sandboxes),
            )
            logger.error("Ready sandboxes: %s", ready_sandboxes)
            logger.error("Not-ready sandboxes: %s", not_ready_sandboxes)

            for sbx_name in not_ready_sandboxes:
                logger.info("=== Describing not-ready sandbox: %s ===", sbx_name)
                try:
                    kubectl("describe", "pod", sbx_name, "-n", config.test_namespace)
                except Exception:
                    logger.warning("(failed to describe %s)", sbx_name)
                try:
                    kubectl("logs", sbx_name, "-n", config.test_namespace, "-c", "runtime")
                except Exception:
                    logger.warning("(failed to get runtime logs for %s)", sbx_name)
                try:
                    kubectl("logs", sbx_name, "-n", config.test_namespace, "-c", "sandbox")
                except Exception:
                    logger.warning("(failed to get sandbox logs for %s)", sbx_name)

            raise TimeoutError(
                f"Timeout waiting for Ready sandboxes: found {len(ready_sandboxes)}/2 after {timeout}s. "
                f"Not-ready sandboxes: {not_ready_sandboxes}"
            )

        except subprocess.CalledProcessError as e:
            raise RuntimeError(f"Failed to diagnose not-ready sandboxes: {e.stderr}")
        except json.JSONDecodeError as e:
            raise RuntimeError(f"Failed to parse kubectl output during diagnosis: {e}")

    yield  # Run the test


def kubectl(*args):
    """Execute kubectl command (no shell interpretation) and log output.

    Raises subprocess.CalledProcessError on non-zero exit.
    """
    result = subprocess.run(
        ["kubectl", *args],
        capture_output=True,
        text=True,
        check=True,
    )
    if result.stdout:
        logger.info("%s", result.stdout.rstrip("\n"))
    if result.stderr:
        logger.warning("stderr: %s", result.stderr.rstrip("\n"))


def kubectl_shell(cmd: str):
    """Execute a kubectl pipeline via the shell and log output.

    Use this only when shell features (pipes, redirects) are needed.
    Callers must build the full command string themselves.
    Does not raise on non-zero exit — callers inspect output as needed.
    """
    result = subprocess.run(
        cmd,
        capture_output=True,
        text=True,
        shell=True,
    )
    if result.stdout:
        logger.info("%s", result.stdout.rstrip("\n"))
    if result.stderr:
        logger.warning("stderr: %s", result.stderr.rstrip("\n"))
