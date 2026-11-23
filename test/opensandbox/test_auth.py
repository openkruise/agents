"""Optional profile corresponding to the E2B gateway UUID auth cases."""
import os

import httpx
import pytest

pytestmark = [
    pytest.mark.gateway_uuid_auth,
    pytest.mark.skipif(os.environ.get("OPENSANDBOX_AUTH_E2E") != "true", reason="requires the existing UUID authentication profile"),
]


def test_execution_authentication(sandbox):
    endpoint = sandbox.get_endpoint(44772)
    protocol = sandbox.connection_config.protocol
    fields = ["X-EXECD-ACCESS-TOKEN"]
    if os.environ.get("OPENSANDBOX_ACCESS_MODE") == "ingress":
        fields.append("X-Access-Token")
    for field in fields:
        for credential in (None, "invalid-execution-token"):
            headers = dict(endpoint.headers)
            if credential is None:
                headers.pop(field, None)
            else:
                headers[field] = credential
            response = httpx.post(f"{protocol}://{endpoint.endpoint}/command", json={"command": "echo should-not-run"}, headers=headers, timeout=10)
            assert response.status_code == 401
    assert sandbox.commands.run("echo authenticated").error is None
