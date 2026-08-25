import asyncio
import base64
import json
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone

import pytest

from kruise_agents.traffic_token import (
    AsyncTrafficTokenManager,
    TrafficAccessToken,
    TrafficAccessTokenExpired,
    TrafficAccessTokenRefreshError,
    TrafficTokenManager,
    expiration_from_jwt,
    parse_expiration,
)


def jwt(exp: float, iat: float = 0) -> str:
    payload = base64.urlsafe_b64encode(
        json.dumps({"exp": exp, "iat": iat}).encode()
    ).rstrip(b"=")
    return f"header.{payload.decode()}.signature"


def result(token: str, expires_at: float) -> TrafficAccessToken:
    return TrafficAccessToken(
        token=token,
        expires_at=datetime.fromtimestamp(expires_at, timezone.utc),
    )


def test_parses_jwt_and_rfc3339_expirations():
    expires_at, issued_at = expiration_from_jwt(jwt(200, 100))

    assert expires_at == datetime.fromtimestamp(200, timezone.utc)
    assert issued_at == datetime.fromtimestamp(100, timezone.utc)
    assert parse_expiration("2026-08-06T12:00:00Z") == datetime(
        2026, 8, 6, 12, tzinfo=timezone.utc
    )


def test_sync_manager_refreshes_once_for_concurrent_requests():
    now = lambda: 950.0
    new_token = jwt(2000, 950)
    calls = 0
    calls_lock = threading.Lock()

    def refresh():
        nonlocal calls
        with calls_lock:
            calls += 1
        time.sleep(0.02)
        return result(new_token, 2000)

    manager = TrafficTokenManager(jwt(1000), refresh, now=now, random_value=lambda: 0)

    with ThreadPoolExecutor(max_workers=8) as executor:
        tokens = list(executor.map(lambda _: manager.ensure_valid_token(), range(8)))

    assert tokens == [new_token] * 8
    assert calls == 1


def test_sync_manager_bootstraps_missing_token_on_first_request():
    new_token = jwt(2000, 950)
    calls = 0

    def refresh():
        nonlocal calls
        calls += 1
        return result(new_token, 2000)

    manager = TrafficTokenManager(
        None, refresh, now=lambda: 950, random_value=lambda: 0
    )

    assert manager.token is None
    assert manager.ensure_valid_token() == new_token
    assert calls == 1


def test_sync_manager_uses_unexpired_token_during_refresh_backoff():
    current_time = [950.0]
    calls = 0

    def refresh():
        nonlocal calls
        calls += 1
        raise TrafficAccessTokenRefreshError("unavailable", retry_after=10)

    manager = TrafficTokenManager(
        jwt(1000),
        refresh,
        now=lambda: current_time[0],
        random_value=lambda: 0,
    )

    assert manager.ensure_valid_token() == manager.token
    current_time[0] = 955
    assert manager.ensure_valid_token() == manager.token
    assert calls == 1


def test_sync_manager_fails_closed_after_expiration():
    manager = TrafficTokenManager(
        jwt(1000),
        lambda: (_ for _ in ()).throw(TrafficAccessTokenRefreshError("unavailable")),
        now=lambda: 1001,
        random_value=lambda: 0,
    )

    with pytest.raises(TrafficAccessTokenExpired):
        manager.ensure_valid_token()


def test_force_refresh_bypasses_refresh_window():
    new_token = jwt(3000, 1100)
    manager = TrafficTokenManager(
        jwt(2000, 1000),
        lambda: result(new_token, 3000),
        now=lambda: 1100,
        random_value=lambda: 0,
    )

    assert manager.ensure_valid_token() != new_token
    assert manager.ensure_valid_token(force=True) == new_token


def test_concurrent_forced_sync_refreshes_are_coalesced():
    new_token = jwt(3000, 1100)
    calls = 0
    refresh_started = threading.Event()
    release_refresh = threading.Event()

    def refresh():
        nonlocal calls
        calls += 1
        refresh_started.set()
        release_refresh.wait()
        return result(new_token, 3000)

    manager = TrafficTokenManager(
        jwt(2000, 1000), refresh, now=lambda: 1100, random_value=lambda: 0
    )
    with ThreadPoolExecutor(max_workers=8) as executor:
        futures = [executor.submit(manager.ensure_valid_token, True) for _ in range(8)]
        refresh_started.wait()
        time.sleep(0.02)
        release_refresh.set()
        tokens = [future.result() for future in futures]

    assert tokens == [new_token] * 8
    assert calls == 1


def test_concurrent_failed_forced_sync_refreshes_are_coalesced():
    calls = 0
    refresh_started = threading.Event()
    release_refresh = threading.Event()
    old_token = jwt(2000, 1000)

    def refresh():
        nonlocal calls
        calls += 1
        refresh_started.set()
        release_refresh.wait()
        raise TrafficAccessTokenRefreshError("unavailable")

    manager = TrafficTokenManager(
        old_token, refresh, now=lambda: 1100, random_value=lambda: 0
    )
    with ThreadPoolExecutor(max_workers=8) as executor:
        futures = [executor.submit(manager.ensure_valid_token, True) for _ in range(8)]
        refresh_started.wait()
        time.sleep(0.02)
        release_refresh.set()
        tokens = [future.result() for future in futures]

    assert tokens == [old_token] * 8
    assert calls == 1


@pytest.mark.asyncio
async def test_async_manager_refreshes_once_for_concurrent_requests():
    calls = 0
    new_token = jwt(2000, 950)

    async def refresh():
        nonlocal calls
        calls += 1
        await asyncio.sleep(0.02)
        return result(new_token, 2000)

    manager = AsyncTrafficTokenManager(
        jwt(1000), refresh, now=lambda: 950, random_value=lambda: 0
    )

    tokens = await asyncio.gather(*(manager.ensure_valid_token() for _ in range(8)))

    assert tokens == [new_token] * 8
    assert calls == 1


@pytest.mark.asyncio
async def test_async_manager_bootstraps_missing_token_on_first_request():
    new_token = jwt(time.time() + 3600, time.time())
    calls = 0

    async def refresh():
        nonlocal calls
        calls += 1
        return result(new_token, time.time() + 3600)

    manager = AsyncTrafficTokenManager(None, refresh, random_value=lambda: 0)

    assert await manager.ensure_valid_token() == new_token
    assert calls == 1


@pytest.mark.asyncio
async def test_concurrent_forced_async_refreshes_are_coalesced():
    calls = 0
    new_token = jwt(3000, 1100)
    refresh_started = asyncio.Event()
    release_refresh = asyncio.Event()

    async def refresh():
        nonlocal calls
        calls += 1
        refresh_started.set()
        await release_refresh.wait()
        return result(new_token, 3000)

    manager = AsyncTrafficTokenManager(
        jwt(2000, 1000), refresh, now=lambda: 1100, random_value=lambda: 0
    )
    tasks = [asyncio.create_task(manager.ensure_valid_token(True)) for _ in range(8)]
    await refresh_started.wait()
    release_refresh.set()
    tokens = await asyncio.gather(*tasks)

    assert tokens == [new_token] * 8
    assert calls == 1


@pytest.mark.asyncio
async def test_concurrent_failed_forced_async_refreshes_are_coalesced():
    calls = 0
    refresh_started = asyncio.Event()
    release_refresh = asyncio.Event()
    old_token = jwt(2000, 1000)

    async def refresh():
        nonlocal calls
        calls += 1
        refresh_started.set()
        await release_refresh.wait()
        raise TrafficAccessTokenRefreshError("unavailable")

    manager = AsyncTrafficTokenManager(
        old_token, refresh, now=lambda: 1100, random_value=lambda: 0
    )
    tasks = [asyncio.create_task(manager.ensure_valid_token(True)) for _ in range(8)]
    await refresh_started.wait()
    release_refresh.set()
    tokens = await asyncio.gather(*tasks)

    assert tokens == [old_token] * 8
    assert calls == 1


@pytest.mark.parametrize(
    "refreshed_token,expiration",
    [
        ("not-a-jwt", 2000),
        (jwt(3000, 950), 2000),
    ],
)
def test_refresh_rejects_malformed_or_mismatched_jwt(refreshed_token, expiration):
    old_token = jwt(1000)
    manager = TrafficTokenManager(
        old_token,
        lambda: result(refreshed_token, expiration),
        now=lambda: 950,
        random_value=lambda: 0,
    )

    assert manager.ensure_valid_token() == old_token
