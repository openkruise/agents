"""SDK-specific fixtures; reuse the existing Kind, images and JUnit workflow."""
import json
import os
import ssl
import uuid
from datetime import timedelta
from pathlib import Path
from urllib.parse import urlsplit

import httpx
import pytest
from opensandbox.config import ConnectionConfigSync
from opensandbox.sync import SandboxSync


class RecordingTransport(httpx.BaseTransport):
    """Observe real SDK requests without changing their destination or headers."""

    def __init__(self):
        self.inner = httpx.HTTPTransport()
        self.created = []
        self.execution_requests = []

    def handle_request(self, request):
        response = self.inner.handle_request(request)
        if request.method == "POST" and request.url.path == "/v1/sandboxes" and response.status_code == 202:
            response.read()
            self.created.append(response.json()["id"])
        if request.url.path in ("/command", "/files/upload", "/files/download"):
            self.execution_requests.append((request.url.host, request.url.port, dict(request.headers)))
        return response

    def close(self):
        self.inner.close()


@pytest.fixture(scope="session")
def kube():
    account = Path("/var/run/secrets/kubernetes.io/serviceaccount")
    context = ssl.create_default_context(cafile=str(account / "ca.crt"))
    with httpx.Client(
        base_url="https://kubernetes.default.svc",
        verify=context,
        headers={"Authorization": f"Bearer {(account / 'token').read_text().strip()}"},
        timeout=20,
    ) as client:
        yield client


def sandbox_resource(kube, sandbox_id):
    namespace = os.environ.get("TEST_NAMESPACE", "default")
    response = kube.get(f"/apis/agents.kruise.io/v1alpha1/namespaces/{namespace}/sandboxes")
    response.raise_for_status()
    for item in response.json()["items"]:
        meta = item["metadata"]
        public_id = meta.get("labels", {}).get("agents.kruise.io/sandbox-id")
        if public_id == sandbox_id or f"{meta['namespace']}--{meta['name']}" == sandbox_id:
            return item
    raise AssertionError(f"Sandbox {sandbox_id} was not found")


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(item, call):
    outcome = yield
    setattr(item, f"rep_{call.when}", outcome.get_result())


@pytest.fixture
def sandbox(request, kube):
    transport = RecordingTransport()
    api = urlsplit(os.environ.get("OPENSANDBOX_API_URL", "http://sandbox-manager.sandbox-system.svc:8080"))
    config = ConnectionConfigSync(
        domain=api.netloc,
        protocol=api.scheme,
        api_key=os.environ.get("OPENSANDBOX_API_KEY", "some-api-key"),
        headers={"x-request-id": uuid.uuid4().hex},
        transport=transport,
        disable_metrics=True,
    )
    instance = None
    try:
        instance = SandboxSync.create(
            os.environ.get("CODE_INTERPRETER_IMG", "registry-ap-southeast-1.ack.aliyuncs.com/acs/code-interpreter:v1.6"),
            resource={"cpu": "100m", "memory": "256Mi"},
            timeout=timedelta(minutes=3),
            ready_timeout=timedelta(seconds=120),
            metadata={"test_case": request.node.name},
            connection_config=config,
        )
        # Discovery and health are setup, not additional business test matrices.
        endpoint = instance.get_endpoint(44772)
        egress = instance.get_endpoint(18080)
        assert endpoint.endpoint == egress.endpoint
        info = sandbox_resource(kube, instance.id)
        ip = info["status"]["podInfo"]["podIP"]
        target = urlsplit(f"{api.scheme}://{endpoint.endpoint}")
        mode = os.environ.get("OPENSANDBOX_ACCESS_MODE", "ingress")
        if mode == "direct":
            assert api.scheme == "http"
            assert target.hostname == ip and target.port == 44772
            assert "e2b-sandbox-id" not in endpoint.headers
        else:
            gateway = urlsplit(os.environ["OPENSANDBOX_GATEWAY_URL"])
            assert target.netloc == gateway.netloc
            assert endpoint.headers["e2b-sandbox-id"] == instance.id
            assert endpoint.headers["e2b-sandbox-port"] == "44772"
        yield instance
        assert transport.execution_requests, "SDK did not send an execution request"
        for host, port, headers in transport.execution_requests:
            assert host == target.hostname
            assert port == (target.port or (443 if api.scheme == "https" else 80))
            assert headers.get("x-execd-access-token"), "SDK lost endpoint credentials"
    finally:
        if instance is not None:
            instance.close()
        # Lifecycle delete is outside this data-plane delivery. Use the same
        # Kubernetes cleanup boundary as the existing create acceptance tests.
        failed = getattr(request.node, "rep_call", None)
        for sandbox_id in transport.created:
            info = sandbox_resource(kube, sandbox_id)
            namespace, name = info["metadata"]["namespace"], info["metadata"]["name"]
            if failed is None or failed.failed:
                print(f"Sandbox diagnostics: {namespace}/{name} {json.dumps(info.get('status', {}))}")
                logs = kube.get(f"/api/v1/namespaces/{namespace}/pods/{name}/log", params={"container": "opensandbox-runtime", "tailLines": "100"})
                print(logs.text)
            response = kube.delete(f"/apis/agents.kruise.io/v1alpha1/namespaces/{namespace}/sandboxes/{name}")
            assert response.status_code in (200, 202, 404), response.status_code
        transport.close()
