import os
os.environ["E2B_DOMAIN"] = "localhost"
os.environ["E2B_API_KEY"] = "some-api-key"
# Import and patch the E2B SDK
import time
from e2b_code_interpreter import Sandbox
from kruise_agents.patch_e2b import patch_e2b
patch_e2b(False)


sandbox: Sandbox = Sandbox.create(
    template="code-interpreter",
    timeout=600,
    metadata={"test_case": "test_pause_connect_kill"},
)
sandbox.beta_pause()
print(f"wait 30s and check sandbox {sandbox.sandbox_id} paused")
time.sleep(30)
input(f"trying to connect sandbox")
sandbox.connect()
input(f"sandbox is working after resume")
sandbox.kill()