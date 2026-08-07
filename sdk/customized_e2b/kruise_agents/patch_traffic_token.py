from __future__ import annotations

import inspect
from collections.abc import AsyncIterator, Iterator
from importlib.metadata import version
from urllib.parse import quote

from e2b import ConnectionConfig
from e2b.sandbox.main import SandboxBase
from e2b_code_interpreter.code_interpreter_sync import Sandbox as SandboxSync
from packaging.version import Version

_E2B_VERSION = Version(version("e2b"))
_TOKEN_REFRESH_SUPPORTED = Version("2.35.0") <= _E2B_VERSION < Version("2.38.0")

if _TOKEN_REFRESH_SUPPORTED:
    import httpx
    from e2b.api import make_async_logging_event_hooks, make_logging_event_hooks
    from e2b.api.client.types import Unset
    from e2b.envd import client_async as envd_client_async
    from e2b.envd import client_sync as envd_client_sync
    from e2b.sandbox_async.main import AsyncSandbox as E2BAsyncSandbox
    from e2b.sandbox_async.sandbox_api import SandboxApi as AsyncSandboxApi
    from e2b.sandbox_sync.commands.command import Commands
    from e2b.sandbox_sync.commands.pty import Pty
    from e2b.sandbox_sync.filesystem.filesystem import Filesystem
    from e2b.sandbox_sync.main import Sandbox as E2BSandbox
    from e2b.sandbox_sync.sandbox_api import SandboxApi as SyncSandboxApi
    from e2b_code_interpreter.code_interpreter_async import (
        AsyncSandbox as SandboxAsync,
    )

    from .traffic_token import (
        AsyncTrafficTokenManager,
        TrafficAccessToken,
        TrafficAccessTokenError,
        TrafficAccessTokenRefreshError,
        TrafficTokenManager,
        parse_expiration,
    )

_TRAFFIC_TOKEN_HEADER = "e2b-traffic-access-token"
_SYNC_MANAGER_ATTRIBUTE = "_kruise_sync_traffic_token_manager"
_ASYNC_MANAGER_ATTRIBUTE = "_kruise_async_traffic_token_manager"
_PATCHED = False

if _TOKEN_REFRESH_SUPPORTED:
    _original_sandbox_init = SandboxBase.__init__
    _original_traffic_access_token = SandboxBase.traffic_access_token
    _original_build_sync_interceptors = envd_client_sync.build_interceptors
    _original_build_async_interceptors = envd_client_async.build_interceptors
    _original_sync_client_factories = {
        cls: cls._create_envd_api for cls in (Filesystem, Commands, Pty)
    }
    _original_async_sandbox_init = E2BAsyncSandbox.__init__
    _original_sync_connect = E2BSandbox.__dict__["connect"]
    _original_async_connect = E2BAsyncSandbox.__dict__["connect"]
    _original_sync_jupyter_client = SandboxSync._client
    _original_async_jupyter_client = SandboxAsync._client


def _check_compatibility() -> None:
    e2b_version = Version(version("e2b"))
    code_interpreter_version = Version(version("e2b-code-interpreter"))
    if not Version("2.35.0") <= e2b_version < Version("2.38.0"):
        raise RuntimeError(
            f"traffic token refresh supports e2b>=2.35.0,<2.38.0, found {e2b_version}"
        )
    if not Version("2.9.0") <= code_interpreter_version < Version("2.10.0"):
        raise RuntimeError(
            "kruise-agents supports e2b-code-interpreter>=2.9.0,<2.10.0, "
            f"found {code_interpreter_version}"
        )
    parameters = inspect.signature(_original_sandbox_init).parameters
    if "traffic_access_token" not in parameters:
        raise RuntimeError(
            "the installed e2b SandboxBase does not expose traffic_access_token"
        )
    for cls in (Filesystem, Commands, Pty):
        if not hasattr(cls, "_create_envd_api"):
            raise RuntimeError(
                f"the installed e2b {cls.__name__} client is incompatible"
            )


def _refresh_url(config: ConnectionConfig, sandbox_id: str) -> str:
    return (
        f"{config.api_url.rstrip('/')}/sandboxes/"
        f"{quote(sandbox_id, safe='')}/traffic-access-token"
    )


def _refresh_headers(config: ConnectionConfig) -> dict[str, str]:
    headers = dict(config.headers)
    if config.api_key:
        headers["X-API-KEY"] = config.api_key
    return headers


def _parse_refresh_response(response: httpx.Response) -> TrafficAccessToken:
    if response.status_code >= 300:
        retry_after = response.headers.get("Retry-After")
        try:
            retry_after_seconds = float(retry_after) if retry_after else None
        except ValueError:
            retry_after_seconds = None
        raise TrafficAccessTokenRefreshError(
            f"traffic access token refresh failed with status {response.status_code}",
            retry_after=retry_after_seconds,
            status_code=response.status_code,
        )
    try:
        body = response.json()
        token = body["trafficAccessToken"]
        expiration = body["trafficAccessTokenExpiration"]
    except (KeyError, TypeError, ValueError) as exc:
        raise TrafficAccessTokenRefreshError(
            "traffic access token refresh returned an invalid response"
        ) from exc
    if not isinstance(token, str) or not token:
        raise TrafficAccessTokenRefreshError(
            "traffic access token refresh returned an invalid response"
        )
    return TrafficAccessToken(token=token, expires_at=parse_expiration(expiration))


def _sync_refresh(config: ConnectionConfig, sandbox_id: str) -> TrafficAccessToken:
    with httpx.Client(
        proxy=config.proxy,
        timeout=config.request_timeout,
        headers=_refresh_headers(config),
        event_hooks=make_logging_event_hooks(config.logger),
    ) as client:
        response = client.post(_refresh_url(config, sandbox_id))
    return _parse_refresh_response(response)


async def _async_refresh(
    config: ConnectionConfig, sandbox_id: str
) -> TrafficAccessToken:
    async with httpx.AsyncClient(
        proxy=config.proxy,
        timeout=config.request_timeout,
        headers=_refresh_headers(config),
        event_hooks=make_async_logging_event_hooks(config.logger),
    ) as client:
        response = await client.post(_refresh_url(config, sandbox_id))
    return _parse_refresh_response(response)


def _sandbox_init(self, *args, **kwargs) -> None:
    _original_sandbox_init(self, *args, **kwargs)
    token = self.traffic_access_token
    if not token:
        return
    config = self.connection_config
    sandbox_id = self.sandbox_id
    try:
        if isinstance(self, E2BAsyncSandbox):
            manager = AsyncTrafficTokenManager(
                token,
                lambda: _async_refresh(config, sandbox_id),
            )
            setattr(self, _ASYNC_MANAGER_ATTRIBUTE, manager)
            setattr(config, _ASYNC_MANAGER_ATTRIBUTE, manager)
        else:
            manager = TrafficTokenManager(
                token,
                lambda: _sync_refresh(config, sandbox_id),
            )
            setattr(self, _SYNC_MANAGER_ATTRIBUTE, manager)
            setattr(config, _SYNC_MANAGER_ATTRIBUTE, manager)
    except TrafficAccessTokenError:
        # Legacy providers may return opaque traffic tokens. They remain usable,
        # but cannot participate in expiration-based automatic refresh.
        return


def _traffic_access_token(self):
    manager = getattr(self, _SYNC_MANAGER_ATTRIBUTE, None)
    if manager is None:
        manager = getattr(self, _ASYNC_MANAGER_ATTRIBUTE, None)
    if manager is not None:
        return manager.token
    return _original_traffic_access_token.fget(self)


def _sync_request_hook(config: ConnectionConfig):
    def add_traffic_token(request: httpx.Request) -> None:
        manager = getattr(config, _SYNC_MANAGER_ATTRIBUTE, None)
        if manager is not None:
            request.headers[_TRAFFIC_TOKEN_HEADER] = manager.ensure_valid_token()

    return add_traffic_token


def _async_request_hook(config: ConnectionConfig):
    async def add_traffic_token(request: httpx.Request) -> None:
        manager = getattr(config, _ASYNC_MANAGER_ATTRIBUTE, None)
        if manager is not None:
            request.headers[_TRAFFIC_TOKEN_HEADER] = await manager.ensure_valid_token()

    return add_traffic_token


def _add_sync_request_hook(client: httpx.Client, config: ConnectionConfig) -> None:
    if getattr(config, _SYNC_MANAGER_ATTRIBUTE, None) is not None:
        client.event_hooks["request"].insert(0, _sync_request_hook(config))


def _add_async_request_hook(
    client: httpx.AsyncClient, config: ConnectionConfig
) -> None:
    if getattr(config, _ASYNC_MANAGER_ATTRIBUTE, None) is not None:
        client.event_hooks["request"].insert(0, _async_request_hook(config))


def _make_sync_client_factory(original):
    def create_envd_api(self):
        client = original(self)
        _add_sync_request_hook(client, self._connection_config)
        return client

    return create_envd_api


def _async_sandbox_init(self, *args, **kwargs) -> None:
    _original_async_sandbox_init(self, *args, **kwargs)
    manager = getattr(self, _ASYNC_MANAGER_ATTRIBUTE, None)
    if manager is not None:
        _add_async_request_hook(self._envd_api, self.connection_config)
        manager.start()


def _sync_jupyter_client(self) -> httpx.Client:
    client = _original_sync_jupyter_client.fget(self)
    _add_sync_request_hook(client, self.connection_config)
    return client


def _async_jupyter_client(self) -> httpx.AsyncClient:
    client = _original_async_jupyter_client.fget(self)
    _add_async_request_hook(client, self.connection_config)
    return client


class _TrafficTokenInterceptor:
    def __init__(self, config: ConnectionConfig):
        self._config = config

    @staticmethod
    def _apply(ctx, token: str) -> None:
        ctx.request_headers[_TRAFFIC_TOKEN_HEADER] = token

    def intercept_unary_sync(self, call_next, request, ctx):
        manager = getattr(self._config, _SYNC_MANAGER_ATTRIBUTE)
        self._apply(ctx, manager.ensure_valid_token())
        return call_next(request, ctx)

    async def intercept_unary(self, call_next, request, ctx):
        manager = getattr(self._config, _ASYNC_MANAGER_ATTRIBUTE)
        self._apply(ctx, await manager.ensure_valid_token())
        return await call_next(request, ctx)

    def intercept_server_stream_sync(self, call_next, request, ctx) -> Iterator:
        manager = getattr(self._config, _SYNC_MANAGER_ATTRIBUTE)
        self._apply(ctx, manager.ensure_valid_token())
        return call_next(request, ctx)

    def intercept_server_stream(self, call_next, request, ctx) -> AsyncIterator:
        async def stream():
            manager = getattr(self._config, _ASYNC_MANAGER_ATTRIBUTE)
            self._apply(ctx, await manager.ensure_valid_token())
            inner = call_next(request, ctx)
            try:
                async for item in inner:
                    yield item
            finally:
                close = getattr(inner, "aclose", None)
                if close is not None:
                    await close()

        return stream()


def _build_sync_interceptors(config: ConnectionConfig, base_url: str) -> list:
    interceptors = _original_build_sync_interceptors(config, base_url)
    if getattr(config, _SYNC_MANAGER_ATTRIBUTE, None) is not None:
        interceptors.insert(0, _TrafficTokenInterceptor(config))
    return interceptors


def _build_async_interceptors(config: ConnectionConfig, base_url: str) -> list:
    interceptors = _original_build_async_interceptors(config, base_url)
    if getattr(config, _ASYNC_MANAGER_ATTRIBUTE, None) is not None:
        interceptors.insert(0, _TrafficTokenInterceptor(config))
    return interceptors


def _refresh_sync(self, force: bool = False) -> str | None:
    manager = getattr(self, _SYNC_MANAGER_ATTRIBUTE, None)
    if manager is None:
        return self.traffic_access_token
    return manager.ensure_valid_token(force=force)


async def _refresh_async(self, force: bool = False) -> str | None:
    manager = getattr(self, _ASYNC_MANAGER_ATTRIBUTE, None)
    if manager is None:
        return self.traffic_access_token
    return await manager.ensure_valid_token(force=force)


async def _close_async_refresh(self) -> None:
    manager = getattr(self, _ASYNC_MANAGER_ATTRIBUTE, None)
    if manager is not None:
        await manager.stop()


def _connect_sync(self, timeout=None, **opts):
    if self.connection_config.debug:
        return self
    manager = getattr(self, _SYNC_MANAGER_ATTRIBUTE, None)

    def connect():
        response = SyncSandboxApi._cls_connect(
            sandbox_id=self.sandbox_id,
            timeout=timeout,
            **self.connection_config.get_api_params(**opts),
        )
        token = response.traffic_access_token
        if token is None or isinstance(token, Unset):
            token = None
        return response, token

    if manager is None:
        connect()
    else:
        manager.run_with_token_replacement(connect)
    return self


async def _connect_async(self, timeout=None, **opts):
    if self.connection_config.debug:
        return self
    manager = getattr(self, _ASYNC_MANAGER_ATTRIBUTE, None)

    async def connect():
        response = await AsyncSandboxApi._cls_connect(
            sandbox_id=self.sandbox_id,
            timeout=timeout,
            **self.connection_config.get_api_params(**opts),
        )
        token = response.traffic_access_token
        if token is None or isinstance(token, Unset):
            token = None
        return response, token

    if manager is None:
        await connect()
    else:
        await manager.run_with_token_replacement(connect)
    return self


def _patch_async_kill() -> None:
    descriptor = E2BAsyncSandbox.__dict__["kill"]
    original = descriptor.method

    async def kill(self, *args, **kwargs):
        try:
            return await original(self, *args, **kwargs)
        finally:
            manager = getattr(self, _ASYNC_MANAGER_ATTRIBUTE, None)
            if manager is not None:
                await manager.stop()

    descriptor.method = kill


def patch_traffic_access_token() -> None:
    """Patch supported E2B SDKs to refresh Traffic JWTs before expiration."""
    global _PATCHED
    if _PATCHED:
        return
    _check_compatibility()
    SandboxBase.__init__ = _sandbox_init
    SandboxBase.traffic_access_token = property(_traffic_access_token)
    E2BSandbox.refresh_traffic_access_token = _refresh_sync
    E2BAsyncSandbox.refresh_traffic_access_token = _refresh_async
    E2BAsyncSandbox.close_traffic_token_refresh = _close_async_refresh
    _original_sync_connect.method = _connect_sync
    _original_async_connect.method = _connect_async
    envd_client_sync.build_interceptors = _build_sync_interceptors
    envd_client_async.build_interceptors = _build_async_interceptors
    for cls, original in _original_sync_client_factories.items():
        cls._create_envd_api = _make_sync_client_factory(original)
    E2BAsyncSandbox.__init__ = _async_sandbox_init
    SandboxSync._client = property(_sync_jupyter_client)
    SandboxAsync._client = property(_async_jupyter_client)
    _patch_async_kill()
    _PATCHED = True
