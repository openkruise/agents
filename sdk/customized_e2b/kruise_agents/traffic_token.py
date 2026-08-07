from __future__ import annotations

import asyncio
import base64
import binascii
import json
import random
import threading
import time
import weakref
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from datetime import datetime, timezone


class TrafficAccessTokenError(RuntimeError):
    """Base error for local traffic access token handling."""


class TrafficAccessTokenExpired(TrafficAccessTokenError):
    """Raised before a data-plane request would use an expired token."""


class TrafficAccessTokenRefreshError(TrafficAccessTokenError):
    def __init__(
        self,
        message: str,
        retry_after: float | None = None,
        status_code: int | None = None,
    ):
        super().__init__(message)
        self.retry_after = retry_after
        self.status_code = status_code


@dataclass(frozen=True)
class TrafficAccessToken:
    token: str
    expires_at: datetime


def parse_expiration(value: str) -> datetime:
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except (AttributeError, TypeError, ValueError) as exc:
        raise TrafficAccessTokenError(
            "invalid traffic access token expiration"
        ) from exc
    if parsed.tzinfo is None:
        raise TrafficAccessTokenError(
            "traffic access token expiration must include a timezone"
        )
    return parsed.astimezone(timezone.utc)


def expiration_from_jwt(token: str) -> tuple[datetime, datetime | None]:
    try:
        encoded_payload = token.split(".")[1]
        encoded_payload += "=" * (-len(encoded_payload) % 4)
        payload = json.loads(base64.urlsafe_b64decode(encoded_payload))
        expires_at = datetime.fromtimestamp(float(payload["exp"]), timezone.utc)
        issued_at = payload.get("iat")
        issued_at_time = (
            datetime.fromtimestamp(float(issued_at), timezone.utc)
            if issued_at is not None
            else None
        )
    except (
        binascii.Error,
        IndexError,
        KeyError,
        OSError,
        OverflowError,
        TypeError,
        UnicodeDecodeError,
        ValueError,
        json.JSONDecodeError,
    ) as exc:
        raise TrafficAccessTokenError(
            "traffic access token is not a JWT with a valid exp claim"
        ) from exc
    return expires_at, issued_at_time


class _TrafficTokenState:
    _INITIAL_BACKOFF_SECONDS = 1.0
    _MAX_BACKOFF_SECONDS = 30.0
    _EXPIRATION_TOLERANCE_SECONDS = 5.0

    def __init__(
        self,
        token: str,
        refresh: Callable[[], TrafficAccessToken],
        *,
        now: Callable[[], float] = time.time,
        random_value: Callable[[], float] = random.random,
    ):
        expires_at, issued_at = expiration_from_jwt(token)
        self._token = token
        self._expires_at = expires_at.timestamp()
        self._refresh = refresh
        self._now = now
        self._random_value = random_value
        self._refresh_at = self._calculate_refresh_at(
            self._expires_at,
            issued_at.timestamp() if issued_at is not None else now(),
        )
        self._next_retry_at = 0.0
        self._backoff_seconds = self._INITIAL_BACKOFF_SECONDS
        self._generation = 0
        self._refresh_attempt = 0
        self._refreshing = False
        self._terminal = False

    @property
    def token(self) -> str:
        return self._token

    @property
    def expires_at(self) -> datetime:
        return datetime.fromtimestamp(self._expires_at, timezone.utc)

    @property
    def refresh_at(self) -> datetime:
        return datetime.fromtimestamp(self._refresh_at, timezone.utc)

    def _calculate_refresh_at(self, expires_at: float, issued_at: float) -> float:
        validity = max(0.0, expires_at - issued_at)
        refresh_ahead = min(300.0, max(60.0, validity * 0.2))
        jitter = refresh_ahead * 0.1 * self._random_value()
        return expires_at - refresh_ahead - jitter

    def _needs_refresh(self, force: bool) -> bool:
        return force or self._now() >= self._refresh_at

    def _in_backoff(self, force: bool) -> bool:
        return not force and self._now() < self._next_retry_at

    def _expired_error(self) -> TrafficAccessTokenExpired:
        return TrafficAccessTokenExpired(
            "traffic access token expired before it could be refreshed"
        )

    def _use_current_or_raise(self) -> str:
        if self._now() >= self._expires_at:
            raise self._expired_error()
        return self._token

    def _record_failure(self, exc: Exception) -> None:
        if isinstance(exc, TrafficAccessTokenRefreshError) and exc.status_code in (
            404,
            409,
        ):
            self._terminal = True
        retry_after = (
            exc.retry_after if isinstance(exc, TrafficAccessTokenRefreshError) else None
        )
        delay = max(self._backoff_seconds, retry_after or 0.0)
        self._next_retry_at = self._now() + delay
        self._backoff_seconds = min(
            self._MAX_BACKOFF_SECONDS, self._backoff_seconds * 2
        )

    def _set_token(self, token: str, expires_at: float, issued_at: float) -> str:
        now = self._now()
        if expires_at <= now:
            raise TrafficAccessTokenError(
                "traffic access token refresh returned an expired token"
            )
        self._token = token
        self._expires_at = expires_at
        self._refresh_at = self._calculate_refresh_at(expires_at, issued_at)
        self._next_retry_at = 0.0
        self._backoff_seconds = self._INITIAL_BACKOFF_SECONDS
        self._generation += 1
        self._terminal = False
        return self._token

    def _record_success(self, result: TrafficAccessToken) -> str:
        if not isinstance(result.token, str) or not result.token:
            raise TrafficAccessTokenError(
                "traffic access token refresh returned an empty token"
            )
        if result.expires_at.tzinfo is None:
            raise TrafficAccessTokenError(
                "traffic access token expiration must include a timezone"
            )
        expires_at = result.expires_at.astimezone(timezone.utc).timestamp()
        jwt_expires_at, _ = expiration_from_jwt(result.token)
        if (
            abs(jwt_expires_at.timestamp() - expires_at)
            > self._EXPIRATION_TOLERANCE_SECONDS
        ):
            raise TrafficAccessTokenError(
                "traffic access token expiration does not match its exp claim"
            )
        return self._set_token(result.token, expires_at, self._now())

    def _record_replacement(self, token: str) -> str:
        expires_at, issued_at = expiration_from_jwt(token)
        return self._set_token(
            token,
            expires_at.timestamp(),
            issued_at.timestamp() if issued_at is not None else self._now(),
        )

    def next_wakeup_delay(self) -> float:
        target = max(self._refresh_at, self._next_retry_at)
        return max(0.0, target - self._now())

    @property
    def terminal(self) -> bool:
        return self._terminal


class TrafficTokenManager(_TrafficTokenState):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._lock = threading.Lock()

    def ensure_valid_token(self, force: bool = False) -> str:
        generation = self._generation
        refresh_attempt = self._refresh_attempt
        refresh_in_progress = self._refreshing
        if not self._needs_refresh(force):
            return self._token
        if self._in_backoff(force):
            return self._use_current_or_raise()

        with self._lock:
            if force and self._generation != generation:
                return self._token
            if force and (
                refresh_in_progress or self._refresh_attempt != refresh_attempt
            ):
                return self._use_current_or_raise()
            if not self._needs_refresh(force):
                return self._token
            if self._in_backoff(force):
                return self._use_current_or_raise()
            self._refresh_attempt += 1
            self._refreshing = True
            try:
                return self._record_success(self._refresh())
            except Exception as exc:
                self._record_failure(exc)
                if self._now() >= self._expires_at:
                    raise self._expired_error() from exc
                return self._token
            finally:
                self._refreshing = False

    def replace_token(self, token: str) -> str:
        with self._lock:
            return self._record_replacement(token)

    def run_with_token_replacement(self, operation):
        with self._lock:
            result, token = operation()
            if token is not None:
                self._record_replacement(token)
            return result


class AsyncTrafficTokenManager(_TrafficTokenState):
    def __init__(
        self,
        token: str,
        refresh: Callable[[], Awaitable[TrafficAccessToken]],
        **kwargs,
    ):
        super().__init__(token, refresh, **kwargs)
        self._lock = asyncio.Lock()
        self._refresh_task: asyncio.Task | None = None
        self._refresh_finalizer = None

    async def ensure_valid_token(self, force: bool = False) -> str:
        generation = self._generation
        refresh_attempt = self._refresh_attempt
        refresh_in_progress = self._refreshing
        if not self._needs_refresh(force):
            return self._token
        if self._in_backoff(force):
            return self._use_current_or_raise()

        async with self._lock:
            if force and self._generation != generation:
                return self._token
            if force and (
                refresh_in_progress or self._refresh_attempt != refresh_attempt
            ):
                return self._use_current_or_raise()
            if not self._needs_refresh(force):
                return self._token
            if self._in_backoff(force):
                return self._use_current_or_raise()
            self._refresh_attempt += 1
            self._refreshing = True
            try:
                result = await self._refresh()
                was_terminal = self._terminal
                token = self._record_success(result)
                if was_terminal:
                    self.start()
                return token
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                self._record_failure(exc)
                if self._now() >= self._expires_at:
                    raise self._expired_error() from exc
                return self._token
            finally:
                self._refreshing = False

    async def replace_token(self, token: str) -> str:
        async with self._lock:
            was_terminal = self._terminal
            result = self._record_replacement(token)
        if was_terminal:
            self.start()
        return result

    async def run_with_token_replacement(self, operation):
        async with self._lock:
            result, token = await operation()
            was_terminal = self._terminal
            if token is not None:
                self._record_replacement(token)
        if token is not None and was_terminal:
            self.start()
        return result

    def start(self) -> None:
        if self._refresh_task is not None and not self._refresh_task.done():
            return
        if self._refresh_finalizer is not None:
            self._refresh_finalizer.detach()
        manager_ref = weakref.ref(self)
        task = asyncio.create_task(self._refresh_loop(manager_ref))
        self._refresh_task = task
        self._refresh_finalizer = weakref.finalize(self, task.cancel)

    async def stop(self) -> None:
        task = self._refresh_task
        self._refresh_task = None
        finalizer = self._refresh_finalizer
        self._refresh_finalizer = None
        if finalizer is not None:
            finalizer.detach()
        if task is None:
            return
        task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            pass

    @staticmethod
    async def _refresh_loop(manager_ref) -> None:
        while True:
            manager = manager_ref()
            if manager is None:
                return
            delay = manager.next_wakeup_delay()
            del manager
            await asyncio.sleep(delay)
            manager = manager_ref()
            if manager is None:
                return
            try:
                await manager.ensure_valid_token()
            except TrafficAccessTokenExpired:
                pass
            if manager.terminal:
                return
            del manager
