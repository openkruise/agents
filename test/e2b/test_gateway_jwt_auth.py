"""E2E tests for sandbox-gateway TrafficAccessToken JWT authentication."""

import os
import shlex
import subprocess
import time

import pytest
import requests
from e2b import PtySize
from e2b_code_interpreter import Sandbox
from websocket import WebSocketBadStatusException, create_connection

from gateway_utils import get_sandbox_access_token, get_sandbox_uid


TOKEN_COMMAND = os.environ.get("JWT_E2E_TOKEN_COMMAND", "")
JWT_AUTH_METADATA_KEY = "security.agents.kruise.io/enable-jwt-auth"
TRAFFIC_ACCESS_TOKEN_HEADER = "E2B-Traffic-Access-Token"
WEBSOCKET_PORT = 8080
WEBSOCKET_SERVER = r'''
import base64
import hashlib
import socket
import time

guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
with socket.socket() as server:
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind(("0.0.0.0", 8080))
    server.listen(1)
    with open("/tmp/jwt-websocket-ready", "w"):
        pass

    connection, _ = server.accept()
    with connection:
        request = b""
        while b"\r\n\r\n" not in request:
            chunk = connection.recv(4096)
            if not chunk:
                raise RuntimeError("WebSocket client closed before handshake")
            request += chunk

        headers = {}
        for line in request.decode("latin1").split("\r\n")[1:]:
            if ":" in line:
                name, value = line.split(":", 1)
                headers[name.lower()] = value.strip()

        key = headers["sec-websocket-key"]
        accept = base64.b64encode(
            hashlib.sha1((key + guid).encode()).digest()
        ).decode()
        response = (
            "HTTP/1.1 101 Switching Protocols\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            f"Sec-WebSocket-Accept: {accept}\r\n"
            "\r\n"
        )
        connection.sendall(response.encode())
        time.sleep(1)
'''
pytestmark = [
    pytest.mark.jwt_auth,
    pytest.mark.skipif(
        os.environ.get("TRAFFIC_ACCESS_TOKEN_JWT_E2E", "").lower() != "true"
        or not TOKEN_COMMAND,
        reason="requires a JWT-enabled gateway and a token issuer command",
    ),
]


def issue_traffic_access_token(sandbox_id, sandbox_uid, expired=False):
    command = shlex.split(TOKEN_COMMAND) + [
        "--sandbox-id",
        sandbox_id,
        "--sandbox-uid",
        sandbox_uid,
    ]
    if expired:
        command.append("--expired")
    result = subprocess.run(
        command,
        capture_output=True,
        text=True,
        check=True,
    )
    token = result.stdout.strip()
    assert token, "token issuer command returned an empty token"
    return token


def sandbox_client_with_traffic_jwt(
    sandbox: Sandbox, traffic_jwt: str
) -> Sandbox:
    # The E2E issuer is external to sandbox-manager. Rebuild the client as if
    # CreateSandbox had returned the issued JWT, exercising patch_e2b at init.
    return Sandbox(
        sandbox_id=sandbox.sandbox_id,
        sandbox_domain=sandbox.sandbox_domain,
        envd_version=sandbox._envd_version,
        envd_access_token=sandbox._envd_access_token,
        traffic_access_token=traffic_jwt,
        connection_config=sandbox.connection_config,
    )


def gateway_websocket_url(config) -> str:
    if config.gateway_url.startswith("https://"):
        return config.gateway_url.replace("https://", "wss://", 1)
    return config.gateway_url.replace("http://", "ws://", 1)


def gateway_request(
    config, sandbox_id, runtime_access_token, traffic_access_token=None
):
    headers = {
        "e2b-sandbox-id": sandbox_id,
        "e2b-sandbox-port": "49983",
        "x-access-token": runtime_access_token,
    }
    if traffic_access_token is not None:
        headers[TRAFFIC_ACCESS_TOKEN_HEADER] = traffic_access_token
    return requests.get(f"{config.gateway_url}/", headers=headers, timeout=10)


def gateway_request_eventually(
    config, sandbox_id, runtime_access_token, traffic_access_token
):
    deadline = time.monotonic() + 30
    response = None
    while time.monotonic() < deadline:
        response = gateway_request(
            config, sandbox_id, runtime_access_token, traffic_access_token
        )
        if response.status_code not in (502, 503):
            return response
        time.sleep(0.5)
    raise AssertionError(
        f"gateway route was not ready for {sandbox_id}: "
        f"{response.status_code if response is not None else 'no response'} "
        f"{response.text if response is not None else ''}"
    )


def test_gateway_traffic_access_token_jwt(sandbox_context, config):
    """Verify route-selective JWT authentication and token validation."""
    first: Sandbox = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=120,
            metadata={JWT_AUTH_METADATA_KEY: "true"},
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    second: Sandbox = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=120,
            metadata={JWT_AUTH_METADATA_KEY: "true"},
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    public: Sandbox = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=120,
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    first_token = issue_traffic_access_token(
        first.sandbox_id, get_sandbox_uid(first.sandbox_id)
    )
    first_runtime_token = get_sandbox_access_token(first.sandbox_id)
    second_runtime_token = get_sandbox_access_token(second.sandbox_id)
    public_runtime_token = get_sandbox_access_token(public.sandbox_id)
    assert first_runtime_token, "first Sandbox is missing its runtime access token"
    assert second_runtime_token, "second Sandbox is missing its runtime access token"
    assert public_runtime_token, "public Sandbox is missing its runtime access token"

    public_response = gateway_request_eventually(
        config, public.sandbox_id, public_runtime_token, None
    )
    assert public_response.status_code in (200, 404), public_response.text

    valid = gateway_request_eventually(
        config, first.sandbox_id, first_runtime_token, first_token
    )
    assert valid.status_code in (200, 404), valid.text

    missing = gateway_request(config, first.sandbox_id, first_runtime_token)
    assert missing.status_code == 403, missing.text

    malformed = gateway_request(
        config, first.sandbox_id, first_runtime_token, "not-a-jwt"
    )
    assert malformed.status_code == 403, malformed.text

    expired_token = issue_traffic_access_token(
        first.sandbox_id, get_sandbox_uid(first.sandbox_id), expired=True
    )
    expired = gateway_request(
        config, first.sandbox_id, first_runtime_token, expired_token
    )
    assert expired.status_code == 403, expired.text

    second_ready = gateway_request_eventually(
        config, second.sandbox_id, second_runtime_token, "not-a-jwt"
    )
    assert second_ready.status_code == 403, second_ready.text

    replayed = gateway_request(
        config, second.sandbox_id, second_runtime_token, first_token
    )
    assert replayed.status_code == 403, replayed.text


def test_gateway_traffic_access_token_jwt_with_e2b_sdk(sandbox_context, config):
    """Verify JWT authentication across E2B SDK data-plane transports."""
    sandbox: Sandbox = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=120,
            metadata={JWT_AUTH_METADATA_KEY: "true"},
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    traffic_token = issue_traffic_access_token(
        sandbox.sandbox_id, get_sandbox_uid(sandbox.sandbox_id)
    )
    runtime_token = get_sandbox_access_token(sandbox.sandbox_id)
    assert runtime_token, "Sandbox is missing its runtime access token"

    ready = gateway_request_eventually(
        config, sandbox.sandbox_id, runtime_token, traffic_token
    )
    assert ready.status_code in (200, 404), ready.text

    missing = gateway_request(config, sandbox.sandbox_id, runtime_token)
    assert missing.status_code == 403, missing.text

    sandbox = sandbox_client_with_traffic_jwt(sandbox, traffic_token)
    assert sandbox.traffic_access_token == traffic_token

    assert sandbox.is_running()

    sandbox.files.write("/tmp/sdk-jwt-ok.txt", "files-jwt-ok")
    assert sandbox.files.read("/tmp/sdk-jwt-ok.txt") == "files-jwt-ok"

    result = sandbox.commands.run("echo commands-jwt-ok")
    assert result.exit_code == 0
    assert result.stdout.strip() == "commands-jwt-ok"

    pty = sandbox.pty.create(PtySize(rows=24, cols=80), timeout=10)
    assert pty.pid > 0
    sandbox.pty.resize(pty.pid, PtySize(rows=40, cols=120))
    assert sandbox.pty.kill(pty.pid)

    execution = sandbox.run_code(
        "print('code-interpreter-jwt-ok')",
        request_timeout=120,
    )
    assert execution.error is None
    assert execution.logs.stdout == ["code-interpreter-jwt-ok\n"]

    sandbox.files.write("/tmp/jwt_websocket_server.py", WEBSOCKET_SERVER)
    sandbox.commands.run(
        "python3 /tmp/jwt_websocket_server.py",
        background=True,
    )
    sandbox.commands.run(
        "for i in $(seq 1 100); do "
        "test -f /tmp/jwt-websocket-ready && exit 0; "
        "sleep 0.1; done; exit 1"
    )

    websocket_headers = [
        f"e2b-sandbox-id: {sandbox.sandbox_id}",
        f"e2b-sandbox-port: {WEBSOCKET_PORT}",
    ]
    with pytest.raises(WebSocketBadStatusException) as exc_info:
        create_connection(
            gateway_websocket_url(config),
            header=websocket_headers,
            timeout=10,
            http_proxy_host=None,
        )
    assert exc_info.value.status_code == 403

    websocket = create_connection(
        gateway_websocket_url(config),
        header=[
            *websocket_headers,
            f"{TRAFFIC_ACCESS_TOKEN_HEADER}: {traffic_token}",
        ],
        timeout=10,
        http_proxy_host=None,
    )
    try:
        assert websocket.connected
    finally:
        websocket.close()
