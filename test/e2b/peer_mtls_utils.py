"""Helpers for the latest-pipeline peer mTLS E2E tests."""

from __future__ import annotations

import json
import os
import socket
import subprocess
import time
from contextlib import contextmanager
from pathlib import Path
from typing import Iterator, Optional

NS = "sandbox-system"
RUNTIME_NAME = "agentruntime.sandbox.agents.kruise.io"
FIXTURE_DIR = Path("/tmp/peer-mtls-e2e")


class PeerMTLSFailure(AssertionError):
    """Test failure with a diagnostic class prefix."""


def require_peer_mtls_env() -> None:
    if os.environ.get("PEER_MTLS_E2E") != "true":
        raise PeerMTLSFailure(
            "deploy failure: PEER_MTLS_E2E is not true; peer mTLS tests must fail, not skip"
        )
    for name in ("ca-a.crt", "untrusted.crt", "untrusted.key", "server.crt", "server.key"):
        if not (FIXTURE_DIR / name).is_file():
            raise PeerMTLSFailure(f"deploy failure: fixture file {name} is missing")


def ca_file() -> str:
    return str(FIXTURE_DIR / "ca-a.crt")


def client_files(identity: str) -> tuple[str, str]:
    if identity == "untrusted":
        return str(FIXTURE_DIR / "untrusted.crt"), str(FIXTURE_DIR / "untrusted.key")
    if identity == "server-as-client":
        return str(FIXTURE_DIR / "server.crt"), str(FIXTURE_DIR / "server.key")
    raise PeerMTLSFailure(f"unknown client identity {identity}")


def kubectl_jsonpath(args: list[str]) -> str:
    result = subprocess.run(
        ["kubectl", *args],
        capture_output=True,
        text=True,
        check=False,
        timeout=30,
    )
    if result.returncode != 0:
        raise PeerMTLSFailure(
            f"deploy failure: kubectl {' '.join(args)} failed: {result.stderr.strip()}"
        )
    return (result.stdout or "").strip()


def pod_name(label: str) -> str:
    name = kubectl_jsonpath(
        [
            "get",
            "pod",
            "-n",
            NS,
            "-l",
            label,
            "-o",
            "jsonpath={.items[0].metadata.name}",
        ]
    )
    if not name:
        raise PeerMTLSFailure(f"deploy failure: no pod matched {label}")
    return name


OUTBOUND_TLS_ERRORS_METRIC = "sandbox_peer_outbound_tls_errors_total"


def manager_outbound_tls_errors() -> float:
    name = pod_name("app.kubernetes.io/name=sandbox-manager")
    text = kubectl_jsonpath(
        [
            "get",
            "--raw",
            f"/api/v1/namespaces/{NS}/pods/{name}:8080/proxy/metrics",
        ]
    )
    for line in text.splitlines():
        if line.startswith("#"):
            continue
        fields = line.split()
        if len(fields) >= 2 and fields[0] == OUTBOUND_TLS_ERRORS_METRIC:
            return float(fields[1])
    raise PeerMTLSFailure(
        f"TLS identity failure: {OUTBOUND_TLS_ERRORS_METRIC} missing from manager /metrics"
    )


def wait_port(host: str, port: int, timeout: float = 15.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            with socket.create_connection((host, port), timeout=1):
                return
        except OSError:
            time.sleep(0.1)
    raise PeerMTLSFailure(f"deploy failure: port-forward {host}:{port} did not become ready")


class PortForward:
    def __init__(self, resource: str, remote_port: int, local_port: int = 0):
        if local_port == 0:
            sock = socket.socket()
            sock.bind(("127.0.0.1", 0))
            local_port = sock.getsockname()[1]
            sock.close()
        self.local_port = local_port
        self.proc = subprocess.Popen(
            [
                "kubectl",
                "port-forward",
                "-n",
                NS,
                resource,
                f"{local_port}:{remote_port}",
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
        )
        try:
            wait_port("127.0.0.1", local_port)
        except Exception:
            self.close()
            raise

    def close(self) -> None:
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.proc.kill()


@contextmanager
def port_forward(resource: str, remote_port: int) -> Iterator[int]:
    pf = PortForward(resource, remote_port)
    try:
        yield pf.local_port
    finally:
        pf.close()


def dummy_refresh_body() -> str:
    return json.dumps(
        {
            "ip": "127.0.0.1",
            "id": "peer-mtls-attack",
            "namespace": "peer-mtls",
            "name": "peer-mtls-attack",
            "uid": "00000000-0000-0000-0000-000000000000",
            "owner": "peer-mtls-e2e",
            "state": "running",
            "resourceVersion": "2",
            "requireTrafficAuth": False,
            "wakeOnTraffic": False,
        }
    )


def curl_https(
    local_port: int,
    path: str,
    method: str = "GET",
    data: Optional[str] = None,
    client_cert: Optional[str] = None,
    client_key: Optional[str] = None,
    timeout: int = 10,
) -> subprocess.CompletedProcess:
    cmd = [
        "curl",
        "-sS",
        "-o",
        "-",
        "-w",
        "\n%{http_code}",
        "--max-time",
        str(timeout),
        "--cacert",
        ca_file(),
        "--resolve",
        f"{RUNTIME_NAME}:{local_port}:127.0.0.1",
        "-X",
        method,
        f"https://{RUNTIME_NAME}:{local_port}{path}",
    ]
    if data is not None:
        cmd.extend(["-H", "Content-Type: application/json", "--data", data])
    if client_cert and client_key:
        cmd.extend(["--cert", client_cert, "--key", client_key])
    return subprocess.run(cmd, capture_output=True, text=True, timeout=timeout + 5)


def split_curl_output(proc: subprocess.CompletedProcess) -> tuple[str, str]:
    out = proc.stdout or ""
    if "\n" not in out:
        return out, ""
    body, code = out.rsplit("\n", 1)
    return body, code.strip()


def is_tls_error(message: str) -> bool:
    low = (message or "").lower()
    needles = (
        "x509",
        "tls:",
        "ssl",
        "certificate",
        "handshake",
        "unknown authority",
        "not authenticated",
        "bad certificate",
        "certificate required",
        "remote error",
    )
    return any(n in low for n in needles)
