"""Peer mTLS E2E for inbound refresh rejection and a live Gateway route."""

from __future__ import annotations

import pytest
from e2b_code_interpreter import Sandbox

import peer_mtls_utils as u
from gateway_utils import get_sandbox_access_token, gateway_request_eventually

pytestmark = pytest.mark.peer_mtls


@pytest.fixture(scope="module")
def peer_mtls():
    u.require_peer_mtls_env()
    return {
        "manager_pod": u.pod_name("app.kubernetes.io/name=sandbox-manager"),
        "gateway_pod": u.pod_name("app.kubernetes.io/name=sandbox-gateway"),
    }


@pytest.fixture(params=["manager", "gateway"])
def refresh_target(request, peer_mtls):
    kind = request.param
    yield {"kind": kind, "pod": peer_mtls[f"{kind}_pod"]}


def test_manager_publish_reaches_gateway(peer_mtls, sandbox_context, config):
    """Create through Manager and confirm Gateway has the published route."""
    sbx = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=120,
            metadata={"test_case": "test_manager_publish_reaches_gateway"},
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    access_token = get_sandbox_access_token(sbx.sandbox_id) or getattr(
        sbx, "_envd_access_token", None
    )
    response = gateway_request_eventually(
        config, sbx.sandbox_id, runtime_access_token=access_token or None
    )
    assert response.status_code not in (401, 502, 503), (
        f"gateway missing manager-published route {sbx.sandbox_id}: {response.status_code}"
    )
    assert u.manager_outbound_tls_errors() == 0, (
        "manager recorded outbound peer TLS errors after a successful publish"
    )


def test_manager_refresh_without_client_cert_fails_tls(peer_mtls):
    """Manager /refresh without a client certificate fails at TLS."""
    with u.port_forward(f"pod/{peer_mtls['manager_pod']}", 7789) as port:
        proc = u.curl_https(port, "/refresh", method="POST", data=u.dummy_refresh_body())
        body, code = u.split_curl_output(proc)
        combined = f"{proc.stderr} {body} {code}"
        assert proc.returncode != 0 or code not in ("200", "204"), combined
        assert code != "204"
        assert u.is_tls_error(combined) or code in ("", "000")


def test_gateway_refresh_without_client_cert_http_403(peer_mtls):
    """Gateway /refresh without a client certificate is HTTP 403 after TLS."""
    with u.port_forward(f"pod/{peer_mtls['gateway_pod']}", 7789) as port:
        proc = u.curl_https(port, "/refresh", method="POST", data=u.dummy_refresh_body())
        body, code = u.split_curl_output(proc)
        combined = f"{proc.stderr} {body} {code}"
        assert proc.returncode == 0, combined
        assert code == "403", combined


def test_gateway_https_health_without_client_cert(peer_mtls):
    """Gateway HTTPS /healthz and /readyz succeed without a client certificate."""
    with u.port_forward(f"pod/{peer_mtls['gateway_pod']}", 7789) as port:
        health = u.curl_https(port, "/healthz")
        health_body, health_code = u.split_curl_output(health)
        assert health.returncode == 0, health.stderr
        assert health_code == "200", health_body
        ready = u.curl_https(port, "/readyz")
        ready_body, ready_code = u.split_curl_output(ready)
        assert ready.returncode == 0, ready.stderr
        assert ready_code == "200", ready_body


def test_untrusted_and_wrong_eku_client_rejected(refresh_target):
    """Untrusted and server-as-client certificates cannot POST /refresh."""
    with u.port_forward(f"pod/{refresh_target['pod']}", 7789) as port:
        for identity in ("untrusted", "server-as-client"):
            cert, key = u.client_files(identity)
            proc = u.curl_https(
                port,
                "/refresh",
                method="POST",
                data=u.dummy_refresh_body(),
                client_cert=cert,
                client_key=key,
            )
            body, code = u.split_curl_output(proc)
            combined = f"{proc.stderr} {body} {code}"
            assert proc.returncode != 0 or code not in ("200", "204"), (
                refresh_target["kind"],
                identity,
                combined,
            )
            assert code != "204", (refresh_target["kind"], identity, combined)
            assert u.is_tls_error(combined) or code in ("", "000"), (
                refresh_target["kind"],
                identity,
                combined,
            )
