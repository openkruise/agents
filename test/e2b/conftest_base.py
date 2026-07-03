"""Community E2B test plugin — shared fixtures, config, and hooks.

Loaded via: pytest -p conftest_base
"""
import dataclasses
import logging
import os
import subprocess
import sys
import uuid
from pathlib import Path
from typing import FrozenSet, List

import pytest
from e2b_code_interpreter import Sandbox

logger = logging.getLogger(__name__)

# Suppress verbose HTTP/2 header logs from hpack (used by urllib3/httpx under the hood)
logging.getLogger("hpack").setLevel(logging.WARNING)

# Add sdk/customized_e2b to Python path
project_root = Path(__file__).parent.parent.parent
sdk_path = project_root / "sdk" / "customized_e2b"
if str(sdk_path) not in sys.path:
    sys.path.insert(0, str(sdk_path))

# Import and apply E2B patch for Kruise Agents
if os.environ.get("PATCH_E2B", "true") == "true":
    # noinspection PyUnresolvedReferences
    from kruise_agents.patch_e2b import patch_e2b
    use_https = os.environ.get("PATCH_E2B_USE_HTTPS", "false") == "true"
    patch_e2b(https=use_https)
    logger.info("e2b client patched (https=%s)", use_https)
else:
    logger.info("e2b client NOT patched — using standard SDK")

# Import fixtures from utils directly to make them available to all tests
# This needs to be imported after patch_e2b is applied
# noinspection PyUnusedImports
from utils import wait_for_sandbox, kubectl, kubectl_shell, run_code_sandbox  # noqa: E402, F401



@dataclasses.dataclass(frozen=True)
class Templates:
    code_interpreter: str = "code-interpreter"
    code_interpreter_0: str = "code-interpreter-0"


@dataclasses.dataclass(frozen=True)
class Images:
    runtime: str = ""
    inplace_update: str = ""


@dataclasses.dataclass(frozen=True)
class TestConfig:
    templates: Templates = dataclasses.field(default_factory=Templates)
    images: Images = dataclasses.field(default_factory=Images)
    e2b_domain: str = "localhost"
    api_url: str = ""
    api_key: str = ""
    gateway_url: str = ""
    test_namespace: str = "default"
    debug: bool = False
    sandbox_cleanup_timeout: int = 60
    # Used by envsubst in SandboxSet YAML templates, and by pause/resume and
    # checkpoint/restore tests to decide whether code context should persist.
    persistent_contents: FrozenSet[str] = dataclasses.field(default_factory=frozenset)


@pytest.fixture(scope="session")
def config() -> TestConfig:
    domain = os.environ.get("E2B_DOMAIN", "localhost")
    use_https = os.environ.get("PATCH_E2B_USE_HTTPS", "false") == "true"
    scheme = "https" if use_https else "http"

    return TestConfig(
        e2b_domain=domain,
        api_url=os.environ.get("E2B_API_URL", f"{scheme}://{domain}/kruise/api"),
        api_key=os.environ.get("E2B_API_KEY", "e2b_00000000"),
        gateway_url=os.environ.get("GATEWAY_URL", f"http://{domain}"),
        test_namespace=os.environ.get("TEST_NAMESPACE", "default"),
        images=Images(
            inplace_update=os.environ.get(
                "INPLACE_UPDATE_IMAGE",
                "registry-ap-southeast-1.ack.aliyuncs.com/acs/code-interpreter:v1.6-update",
            ),
        ),
        debug=os.environ.get("E2E_DEBUG", "").lower() == "true",
        sandbox_cleanup_timeout=int(os.environ.get("SANDBOX_CLEANUP_TIMEOUT", "60")),
    )


class SandboxContext:
    """Context manager for sandboxes created during tests."""

    def __init__(self):
        self.sandboxes: List[Sandbox] = []
        self.request_id: str = str(uuid.uuid4())

    def add(self, sandbox: Sandbox) -> Sandbox:
        """Add a sandbox to the context."""
        self.sandboxes.append(sandbox)
        return sandbox

    @staticmethod
    def _run_kubectl(args, timeout=30):
        """Run kubectl and return stdout. For diagnostics only — uses print()."""
        try:
            r = subprocess.run(
                ["kubectl", *args],
                capture_output=True, text=True, timeout=timeout,
            )
            return r.stdout or r.stderr or "(no output)"
        except Exception as e:
            return f"(kubectl error: {e})"

    @staticmethod
    def _run_kubectl_shell(cmd, timeout=30):
        """Run kubectl shell command and return stdout. For diagnostics only."""
        try:
            r = subprocess.run(
                cmd, capture_output=True, text=True, shell=True, timeout=timeout,
            )
            return r.stdout or r.stderr or "(no output)"
        except Exception as e:
            return f"(kubectl error: {e})"

    def _collect_diagnostics(self):
        """Collect kubectl describe and component logs for all tracked sandboxes.

        Always called on test failure, regardless of E2E_DEBUG setting.
        Uses print() (not logger) so output appears in pytest captured stdout
        and is included in failure reports and JUnit XML.
        """
        if not self.sandboxes:
            print("\n--- no sandboxes tracked ---")
            print("sandbox-manager logs:")
            print(self._run_kubectl_shell(
                f"kubectl logs -n sandbox-system -l component=sandbox-manager "
                f"--tail 5000 | grep {self.request_id}"
            ))
            print("sandbox-controller logs:")
            print(self._run_kubectl(
                ["logs", "-n", "sandbox-system",
                 "-l", "control-plane=sandbox-controller-manager",
                 "--tail", "100"]
            ))
            return

        for sandbox in self.sandboxes:
            sandbox_id = sandbox.sandbox_id.split("--")[1]
            ns = sandbox.sandbox_id.split("--")[0]
            print(f"\n--- sandbox: {sandbox_id} (ns={ns}) ---")
            print("Describe sandbox CR:")
            print(self._run_kubectl(["describe", "sandbox", sandbox_id, "-n", ns]))
            print("Describe pod:")
            print(self._run_kubectl(["describe", "pod", sandbox_id, "-n", ns]))
            print("Sandbox container logs:")
            print(self._run_kubectl_shell(
                f"kubectl logs {sandbox_id} -n {ns} --tail 5000 "
                f"| grep -v 169.254.169.254"
            ))
            print("Sandbox Manager logs:")
            print(self._run_kubectl_shell(
                f"kubectl logs -n sandbox-system -l component=sandbox-manager "
                f"--tail 5000 | grep {sandbox_id}"
            ))
            print("Sandbox Controller logs:")
            print(self._run_kubectl_shell(
                f"kubectl logs -n sandbox-system "
                f"-l control-plane=sandbox-controller-manager "
                f"--tail 5000 | grep {sandbox_id}"
            ))

    def cleanup(self, test_failed: bool = False, cfg: TestConfig = None):
        """Clean up all sandboxes in the context."""
        debug = cfg.debug if cfg else False

        if test_failed and debug:
            logger.warning(
                "E2E_DEBUG: preserving %d sandbox(es) for investigation",
                len(self.sandboxes),
            )
            self.sandboxes.clear()
            return

        for sandbox in self.sandboxes:
            try:
                sandbox_id = sandbox.sandbox_id.split("--")[1]
                logger.info("Cleaning up sandbox: %s", sandbox_id)
                sandbox.kill()
                logger.info("Successfully cleaned up sandbox: %s", sandbox_id)
            except Exception as e:
                logger.warning("Failed to cleanup sandbox: %s", e)

        self.sandboxes.clear()


@pytest.fixture
def sandbox_context(request, config):
    """Fixture that provides a sandbox context for test cases."""
    context = SandboxContext()

    yield context

    # Cleanup (kill) all sandboxes
    test_failed = request.node.rep_call.failed if hasattr(request.node, 'rep_call') else False
    context.cleanup(test_failed=test_failed, cfg=config)


@pytest.hookimpl(tryfirst=True, hookwrapper=True)
def pytest_runtest_makereport(item, call):
    """Collect and print sandbox diagnostics on failure, before instafail.

    Execution order within this hookwrapper:
    1. yield → test runs (call phase)
    2. After yield → fixture teardown has NOT run yet, sandbox still alive
    3. We collect & print diagnostics directly to stdout
    4. hookwrapper exits → makereport generates the report
    5. pytest_runtest_logreport fires → instafail prints (our output is already there)
    """
    outcome = yield
    rep = outcome.get_result()
    setattr(item, f"rep_{rep.when}", rep)

    if rep.when == "call" and rep.failed:
        # Sandbox is still alive at this point — fixture teardown hasn't run.
        # Collect and print diagnostics directly so they appear before instafail.
        ctx = item.funcargs.get("sandbox_context")
        if ctx and ctx.sandboxes:
            print(f"\n{'-' * 27} Sandbox diagnostics {'-' * 27}")
            try:
                ctx._collect_diagnostics()
            except Exception as e:
                print(f"(diagnostics error: {e})")
            print(f"{'-' * 75}\n")
