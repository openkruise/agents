"""Shared helpers for sandbox-gateway end-to-end tests."""

import json
import subprocess
import time

import requests
from utils import resolve_sandbox_cr

# The port agent-runtime listens on. agent-runtime authenticates x-access-token
# on its own, so a rejection observed here cannot be attributed to the gateway.
RUNTIME_PORT = 49983
# A port served by the sandbox workload itself. Nothing behind it inspects
# credentials, so a 200 carrying WORKLOAD_RESPONSE_BODY is positive proof that a
# request traversed the gateway, and any 4xx/5xx can only have come from the
# gateway. Kept distinct from the 8080 used by the WebSocket case so both
# servers can coexist inside one sandbox.
WORKLOAD_PORT = 8081
WORKLOAD_RESPONSE_BODY = "gateway-workload-ok"
TRAFFIC_ACCESS_TOKEN_HEADER = "E2B-Traffic-Access-Token"

_WORKLOAD_SERVER_PATH = "/tmp/gateway_workload_server.py"
_WORKLOAD_READY_PATH = "/tmp/gateway-workload-ready"
_WORKLOAD_SERVER = f'''
import http.server

BODY = {WORKLOAD_RESPONSE_BODY!r}.encode()


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(BODY)))
        self.end_headers()
        self.wfile.write(BODY)

    def log_message(self, *args):
        pass


server = http.server.HTTPServer(("0.0.0.0", {WORKLOAD_PORT}), Handler)
# Binding already succeeded here, so the readiness marker cannot race ahead of
# the listening socket.
with open({_WORKLOAD_READY_PATH!r}, "w"):
    pass
server.serve_forever()
'''


def get_sandbox_resource(sandbox_id: str) -> dict:
    """Return the Sandbox resource backing an opaque sandbox ID."""
    namespace, name = resolve_sandbox_cr(sandbox_id)
    if not namespace or not name:
        raise LookupError(f"cannot resolve Sandbox CR for sandbox ID {sandbox_id}")
    result = subprocess.run(
        ["kubectl", "get", "sandbox", name, "-n", namespace, "-o", "json"],
        capture_output=True,
        text=True,
        check=True,
    )
    return json.loads(result.stdout)


def get_sandbox_access_token(sandbox_id: str) -> str:
    """Return the runtime access token annotation, if present."""
    sandbox = get_sandbox_resource(sandbox_id)
    annotations = sandbox.get("metadata", {}).get("annotations", {})
    return annotations.get("agents.kruise.io/runtime-access-token", "")


def get_sandbox_uid(sandbox_id: str) -> str:
    """Return the immutable Sandbox UID."""
    return get_sandbox_resource(sandbox_id)["metadata"]["uid"]


def start_workload_server(sandbox) -> None:
    """Serve WORKLOAD_RESPONSE_BODY on WORKLOAD_PORT inside the sandbox.

    The caller must pass a client that can already reach the sandbox through the
    gateway; for a JWT opt-in Sandbox that means a client carrying a valid
    traffic token. Waiting on the readiness marker keeps the first gateway
    request from racing the listener startup.
    """
    sandbox.files.write(_WORKLOAD_SERVER_PATH, _WORKLOAD_SERVER)
    sandbox.commands.run(f"python3 {_WORKLOAD_SERVER_PATH}", background=True)
    sandbox.commands.run(
        f"for i in $(seq 1 100); do test -f {_WORKLOAD_READY_PATH} && exit 0; "
        "sleep 0.1; done; exit 1"
    )


def gateway_request(
    config,
    sandbox_id,
    runtime_access_token=None,
    traffic_access_token=None,
    port=RUNTIME_PORT,
):
    """Send a header-routed request through the gateway to one sandbox port."""
    headers = {
        "e2b-sandbox-id": sandbox_id,
        "e2b-sandbox-port": str(port),
    }
    if runtime_access_token is not None:
        headers["x-access-token"] = runtime_access_token
    if traffic_access_token is not None:
        headers[TRAFFIC_ACCESS_TOKEN_HEADER] = traffic_access_token
    return requests.get(f"{config.gateway_url}/", headers=headers, timeout=10)


def gateway_request_eventually(
    config,
    sandbox_id,
    runtime_access_token=None,
    traffic_access_token=None,
    port=RUNTIME_PORT,
    timeout=30,
):
    """Wait for a post-claim route before inspecting its final response.

    A stale warm-pool route has no token and can return 401 before its state is
    checked. Retry that response as well as registry/state unavailability (502
    and 503), bounded by the deadline. A permanent UUID failure still fails the
    wait. JWT rejection (403) remains terminal; tests expecting UUID rejection
    should use gateway_request directly rather than this readiness helper.
    """
    deadline = time.monotonic() + timeout
    response = None
    while time.monotonic() < deadline:
        response = gateway_request(
            config, sandbox_id, runtime_access_token, traffic_access_token, port
        )
        if response.status_code not in (401, 502, 503):
            return response
        time.sleep(0.5)
    raise AssertionError(
        f"gateway route was not ready for {sandbox_id} on port {port}: "
        f"{response.status_code if response is not None else 'no response'} "
        f"{response.text if response is not None else ''}"
    )


def assert_workload_reached(response) -> None:
    """Assert a response was produced by the workload, not by the gateway."""
    assert response.status_code == 200, (
        f"expected the workload's 200, got {response.status_code}: {response.text}"
    )
    assert response.text == WORKLOAD_RESPONSE_BODY, response.text
