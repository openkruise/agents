"""Pytest configuration for E2B tests."""
import sys
from pathlib import Path
from typing import List

import pytest
from e2b_code_interpreter import Sandbox

# Add sdk/customized_e2b to Python path
project_root = Path(__file__).parent.parent.parent
sdk_path = project_root / "sdk" / "customized_e2b"
if str(sdk_path) not in sys.path:
    sys.path.insert(0, str(sdk_path))

# Import and apply E2B patch for Kruise Agents
# noinspection PyUnresolvedReferences
from kruise_agents.patch_e2b import patch_e2b

patch_e2b(https=False)
print("e2b client patched")

# Import fixtures from utils directly to make them available to all tests
# This needs to be imported after patch_e2b is applied
# noinspection PyUnusedImports
from utils import wait_for_sandbox, kubectl, run_code_sandbox


class SandboxContext:
    """Context manager for sandboxes created during tests."""

    def __init__(self):
        self.sandboxes: List[Sandbox] = []

    def add(self, sandbox: Sandbox) -> Sandbox:
        """Add a sandbox to the context."""
        self.sandboxes.append(sandbox)
        return sandbox

    def cleanup(self, test_failed: bool = False):
        """Clean up all sandboxes in the context."""
        # If no sandboxes, print sandbox manager logs
        if test_failed and not self.sandboxes:
            print("\n=== No sandboxes to cleanup, printing logs ===")
            print("== sandbox-manager logs ==")
            try:
                kubectl("logs", "-n", "sandbox-system",
                        "-l", "component=sandbox-manager",
                        "--tail", "100")
            except Exception as e:
                print(f"Failed to get sandbox-manager logs: {e}")
            print("== end sandbox-manager logs ==")
            print("== sandbox-controller logs ==")
            try:
                kubectl("logs", "-n", "sandbox-system",
                        "-l", "control-plane=sandbox-controller-manager",
                        "--tail", "100")
            except Exception as e:
                print(f"Failed to get sandbox-controller logs: {e}")
            print("== end sandbox-controller logs ==")
            print("=== End sandbox-manager logs ===\n")
            return

        for sandbox in self.sandboxes:
            try:
                sandbox_id = sandbox.sandbox_id.split("--")[1]
                print(f"Cleaning up sandbox: {sandbox_id}")
                # Kill the sandbox
                sandbox.kill()
                print(f"Successfully cleaned up sandbox: {sandbox_id}")

                # If test failed, print sandbox manager logs
                if test_failed:
                    print(f"\n=== Logs for sandbox {sandbox_id} ===")
                    print("=== Sandbox Manager ===")
                    kubectl("logs", "-n", "sandbox-system",
                            "-l", "component=sandbox-manager",
                            "--tail", "10000", "|", "grep", sandbox_id)
                    print("=== Sandbox Controller ===")
                    kubectl("logs", "-n", "sandbox-system",
                            "-l", "control-plane=sandbox-controller-manager",
                            "--tail", "10000", "|", "grep", sandbox_id)
                    print(f"=== End logs for sandbox {sandbox_id} ===\n")

            except Exception as e:
                print(f"Failed to cleanup sandbox: {e}")

        self.sandboxes.clear()


@pytest.fixture
def sandbox_context(request):
    """Fixture that provides a sandbox context for test cases."""
    context = SandboxContext()

    yield context

    # AfterEach: cleanup all sandboxes
    test_failed = request.node.rep_call.failed if hasattr(request.node, 'rep_call') else False
    context.cleanup(test_failed=test_failed)


@pytest.hookimpl(tryfirst=True, hookwrapper=True)
def pytest_runtest_makereport(item, call):
    """Hook to capture test outcome for cleanup."""
    outcome = yield
    rep = outcome.get_result()
    setattr(item, f"rep_{rep.when}", rep)
