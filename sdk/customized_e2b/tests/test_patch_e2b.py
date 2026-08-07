import base64
import json
import time
from datetime import datetime, timezone
from types import SimpleNamespace

import httpx
import pytest
from e2b import ConnectionConfig
from e2b_code_interpreter.code_interpreter_async import AsyncSandbox
from e2b_code_interpreter.code_interpreter_sync import Sandbox
from packaging.version import Version

import kruise_agents.patch_traffic_token as patch_module
from kruise_agents.patch_e2b import patch_e2b
from kruise_agents.traffic_token import TrafficAccessToken


def jwt(exp: float, iat: float) -> str:
    payload = base64.urlsafe_b64encode(
        json.dumps({"exp": exp, "iat": iat}).encode()
    ).rstrip(b"=")
    return f"header.{payload.decode()}.signature"


def config() -> ConnectionConfig:
    return ConnectionConfig(
        api_key="e2b_" + "0" * 40,
        validate_api_key=False,
        api_url="https://example.test/kruise/api",
        debug=True,
        extra_sandbox_headers={"E2b-Sandbox-Id": "sandbox-1"},
    )


def token_result(token: str, expires_at: float) -> TrafficAccessToken:
    return TrafficAccessToken(
        token,
        datetime.fromtimestamp(expires_at, timezone.utc),
    )


def test_sync_patch_refreshes_http_rpc_and_jupyter_headers(monkeypatch):
    monkeypatch.setenv("E2B_DOMAIN", "example.test")
    patch_e2b(validate_key=False)
    patch_module.patch_traffic_access_token()
    now = time.time()
    refreshed_token = jwt(now + 7200, now)
    monkeypatch.setattr(
        patch_module,
        "_sync_refresh",
        lambda _config, _sandbox_id: token_result(refreshed_token, now + 7200),
    )
    sandbox = Sandbox(
        sandbox_id="sandbox-1",
        sandbox_domain="example.test",
        envd_version=Version("0.2.0"),
        envd_access_token=None,
        traffic_access_token=jwt(now + 3600, now),
        connection_config=config(),
    )

    assert sandbox.refresh_traffic_access_token(force=True) == refreshed_token
    assert sandbox.traffic_access_token == refreshed_token

    request = httpx.Request("POST", "https://example.test")
    patch_module._sync_request_hook(sandbox.connection_config)(request)
    assert request.headers[patch_module._TRAFFIC_TOKEN_HEADER] == refreshed_token

    envd_client = sandbox.files._envd_api
    assert envd_client.event_hooks["request"]
    envd_client.close()

    class Context:
        def __init__(self):
            self.request_headers = {}

    context = Context()
    interceptor = patch_module._TrafficTokenInterceptor(sandbox.connection_config)
    assert (
        interceptor.intercept_unary_sync(
            lambda request, _ctx: request, "rpc-result", context
        )
        == "rpc-result"
    )
    assert (
        context.request_headers[patch_module._TRAFFIC_TOKEN_HEADER] == refreshed_token
    )

    jupyter_client = sandbox._client
    assert jupyter_client.event_hooks["request"]
    jupyter_client.close()

    bootstrap_token = jwt(now + 7200, now)
    monkeypatch.setattr(
        patch_module.SyncSandboxApi,
        "_cls_connect",
        classmethod(
            lambda _cls, **_kwargs: SimpleNamespace(
                traffic_access_token=bootstrap_token
            )
        ),
    )
    sandbox.connection_config.debug = False
    assert sandbox.connect() is sandbox
    assert sandbox.traffic_access_token == bootstrap_token


def test_opaque_traffic_token_keeps_legacy_behavior(monkeypatch):
    monkeypatch.setenv("E2B_DOMAIN", "example.test")
    patch_e2b(validate_key=False)
    patch_module.patch_traffic_access_token()
    sandbox = Sandbox(
        sandbox_id="sandbox-1",
        sandbox_domain="example.test",
        envd_version=Version("0.2.0"),
        envd_access_token=None,
        traffic_access_token="opaque-token",
        connection_config=config(),
    )

    assert sandbox.traffic_access_token == "opaque-token"
    assert not hasattr(sandbox, patch_module._SYNC_MANAGER_ATTRIBUTE)


@pytest.mark.asyncio
async def test_async_patch_refreshes_and_stops_with_sandbox(monkeypatch):
    monkeypatch.setenv("E2B_DOMAIN", "example.test")
    patch_e2b(https=False, validate_key=False)
    patch_module.patch_traffic_access_token()

    async def refresh(_config, _sandbox_id):
        return token_result(refreshed_token, now + 7200)

    now = time.time()
    refreshed_token = jwt(now + 7200, now)
    monkeypatch.setattr(patch_module, "_async_refresh", refresh)
    sandbox = AsyncSandbox(
        sandbox_id="sandbox-1",
        sandbox_domain="example.test",
        envd_version=Version("0.2.0"),
        envd_access_token=None,
        traffic_access_token=jwt(now + 3600, now),
        connection_config=config(),
    )
    manager = getattr(sandbox, patch_module._ASYNC_MANAGER_ATTRIBUTE)

    assert await sandbox.refresh_traffic_access_token(force=True) == refreshed_token
    assert sandbox._envd_api.event_hooks["request"]
    request = httpx.Request("POST", "https://example.test")
    await patch_module._async_request_hook(sandbox.connection_config)(request)
    assert request.headers[patch_module._TRAFFIC_TOKEN_HEADER] == refreshed_token

    jupyter_client = sandbox._client
    assert jupyter_client.event_hooks["request"]
    assert sandbox._jupyter_url.startswith("http://")
    await jupyter_client.aclose()

    bootstrap_token = jwt(now + 7200, now)

    async def connect(_cls, **_kwargs):
        return SimpleNamespace(traffic_access_token=bootstrap_token)

    monkeypatch.setattr(
        patch_module.AsyncSandboxApi, "_cls_connect", classmethod(connect)
    )
    sandbox.connection_config.debug = False
    assert await sandbox.connect() is sandbox
    assert sandbox.traffic_access_token == bootstrap_token
    sandbox.connection_config.debug = True
    await sandbox.kill()
    await sandbox._envd_api.aclose()

    assert manager._refresh_task is None

    await sandbox.close_traffic_token_refresh()


def test_refresh_response_rejects_errors_without_exposing_body():
    response = httpx.Response(
        503,
        headers={"Retry-After": "7"},
        content=b'{"message":"issuer details"}',
    )

    with pytest.raises(Exception, match="status 503") as raised:
        patch_module._parse_refresh_response(response)

    assert "issuer details" not in str(raised.value)
    assert raised.value.retry_after == 7
    assert raised.value.status_code == 503


def test_compatibility_check_rejects_unsupported_e2b(monkeypatch):
    monkeypatch.setattr(
        patch_module,
        "version",
        lambda package: "2.34.0" if package == "e2b" else "2.9.0",
    )

    with pytest.raises(RuntimeError, match="supports e2b>=2.35.0,<2.38.0"):
        patch_module._check_compatibility()


def test_legacy_e2b_rejects_standalone_token_refresh_patch(monkeypatch):
    monkeypatch.setattr(patch_module, "_PATCHED", False)
    monkeypatch.setattr(
        patch_module,
        "version",
        lambda package: "2.34.0" if package == "e2b" else "2.9.0",
    )

    with pytest.raises(RuntimeError, match="supports e2b>=2.35.0,<2.38.0"):
        patch_module.patch_traffic_access_token()


def test_compatibility_check_accepts_latest_tested_e2b(monkeypatch):
    monkeypatch.setattr(
        patch_module,
        "version",
        lambda package: "2.37.0" if package == "e2b" else "2.9.0",
    )

    patch_module._check_compatibility()
