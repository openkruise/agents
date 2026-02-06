"""Utility functions and fixtures for E2B tests."""
import json
import subprocess
import time
from typing import List, Optional

import pytest
from e2b.sandbox.sandbox_api import SandboxInfo, SandboxQuery
from e2b_code_interpreter import Sandbox


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
            if "sandbox is pausing, please wait a moment and try again" in error_msg:
                if attempt < max_retries - 1:
                    print(f"Sandbox is pausing, waiting {retry_interval}s before retry... (attempt {attempt + 1}/{max_retries})")
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
                print(f"Failed to run code (attempt {attempt + 1}/{max_retries}): {e}")
                print(f"Retrying in {retry_interval}s...")
                time.sleep(retry_interval)
                continue
            else:
                print(f"Failed to run code after {max_retries} attempts")
                raise
    return None


def list_sandbox(query: SandboxQuery = None) -> List[SandboxInfo]:
    """List all sandboxes matching the given query with full pagination.
    
    Args:
        query: SandboxQuery to filter sandboxes. If None, lists all sandboxes.
        
    Returns:
        List of all matching SandboxInfo objects.
    """
    paginator = Sandbox.list(query=query) if query else Sandbox.list()
    sandboxes = paginator.next_items()

    while paginator.has_next:
        items = paginator.next_items()
        sandboxes.extend(items)

    return sandboxes


@pytest.fixture(autouse=True)
def wait_for_sandbox():
    """BeforeEach: Wait up to 60 seconds until list_sandbox returns empty,
    then check sandbox Ready condition via kubectl."""
    timeout = 30
    interval = 2
    start_time = time.time()

    # Wait for sandboxes cleanup
    while time.time() - start_time < timeout:
        sandboxes = list_sandbox()
        if len(sandboxes) == 0:
            print("No sandboxes running, proceeding to check Ready conditions")
            break
        print(f"Waiting for {len(sandboxes)} sandbox(es) to be cleaned up...")
        time.sleep(interval)

    else:
        # Timeout reached, fail the test
        remaining = list_sandbox()
        raise TimeoutError(f"{len(remaining)} sandbox(es) still running after {timeout}s timeout")

    # Wait for at least 2 Ready sandboxes
    ready_start_time = time.time()
    while time.time() - ready_start_time < timeout:
        try:
            result = subprocess.run(
                ["kubectl", "get", "sbx", "-o", "json"],
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
            print(f"Found {ready_count} Ready sandbox(es) out of {total_count} total")

            if ready_count >= 2:
                print(f"Required Ready sandboxes available, proceeding with test")
                break

            print(f"Waiting for more Ready sandboxes ({ready_count}/2)...")
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
                ["kubectl", "get", "sbx", "-o", "json"],
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

            # Describe all not-ready sandboxes
            print(f"\n=== Timeout: Only {len(ready_sandboxes)} Ready sandbox(es), expected at least 2 ===")
            print(f"Ready sandboxes: {ready_sandboxes}")
            print(f"Not-ready sandboxes: {not_ready_sandboxes}\n")

            for sbx_name in not_ready_sandboxes:
                print(f"\n=== Describing not-ready sandbox: {sbx_name} ===")
                kubectl("describe", "pod", sbx_name)
                kubectl("logs", sbx_name, "-c", "runtime")
                kubectl("logs", sbx_name, "-c", "sandbox")

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
    """Execute kubectl command and print output."""
    result = subprocess.run(
        "kubectl " + " ".join(args),
        capture_output=True,
        text=True,
        shell=True
    )
    print(result.stdout)
    if result.stderr:
        print(f"Error: {result.stderr}")
