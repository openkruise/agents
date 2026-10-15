# Copyright 2026.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""HTTP client for the sandbox-manager resource management API.

End users never talk to the Kubernetes API Server: every resource operation
(Template / SandboxSet / PoolAutoscaler) goes through sandbox-manager's
``/agents.kruise.io/v1alpha1`` HTTP API with ``X-API-Key`` authentication.
The server derives the namespace from the API key's team; callers cannot
select a namespace.
"""

from __future__ import annotations

import os
import threading
from typing import Any
from urllib.parse import urlsplit

import httpx

from .errors import api_error_from_response

API_PREFIX = "/agents.kruise.io/v1alpha1"

# The SDK deliberately reuses the E2B SDK environment variables instead of
# introducing its own: a deployment that already points the customized E2B
# SDK at sandbox-manager needs no extra configuration here.
API_KEY_ENV = "E2B_API_KEY"
API_URL_ENV = "E2B_API_URL"
DEFAULT_BASE_URL = "http://127.0.0.1:8997"


def _origin_from_url(url: str) -> str:
    """Extract the origin (scheme://host:port) from an API URL.

    ``E2B_API_URL`` usually carries the E2B API path (e.g.
    ``https://host/kruise/api``); the resource API lives on the same origin
    under ``/agents.kruise.io/v1alpha1``, so only the origin is kept. A bare
    origin (``http://host:8997``) passes through unchanged.
    """

    parts = urlsplit(url)
    return f"{parts.scheme}://{parts.netloc}"


class KruiseClient:
    """Synchronous client for the Kruise resource management API.

    Args:
        api_key: sandbox-manager API key. Falls back to ``E2B_API_KEY``.
        base_url: sandbox-manager origin (e.g. ``https://kruise.example.com``).
            Falls back to the origin extracted from ``E2B_API_URL``, then
            ``http://127.0.0.1:8997``.
        timeout: per-request timeout in seconds; applies only when
            ``http_client`` is not given (a preconfigured client owns its
            own timeout).
        http_client: optional preconfigured ``httpx.Client`` (e.g. with a
            custom transport for testing).
    """

    def __init__(
        self,
        api_key: str | None = None,
        base_url: str | None = None,
        timeout: float = 60.0,
        http_client: httpx.Client | None = None,
    ) -> None:
        self.api_key = api_key or os.environ.get(API_KEY_ENV) or ""
        if base_url:
            self.base_url = base_url.rstrip("/")
        elif env_url := os.environ.get(API_URL_ENV):
            self.base_url = _origin_from_url(env_url)
        else:
            self.base_url = DEFAULT_BASE_URL
        self._client = http_client or httpx.Client(timeout=timeout)

    def request(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        params: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Send a request under the resource API prefix and return the JSON body.

        Raises a :class:`openkruise_agents.errors.KruiseApiError` subclass
        for non-2xx responses.
        """

        url = f"{self.base_url}{API_PREFIX}{path}"
        headers = {}
        if self.api_key:
            headers["X-API-Key"] = self.api_key
        response = self._client.request(method, url, json=body, params=params, headers=headers)
        payload: dict[str, Any] | None = None
        raw_body: str | None = None
        try:
            payload = response.json()
        except ValueError:
            raw_body = response.text[:512] if response.text else None
        if response.status_code >= 300:
            raise api_error_from_response(response.status_code, payload, raw_body)
        return payload if isinstance(payload, dict) else {}

    def close(self) -> None:
        self._client.close()

    def __enter__(self) -> "KruiseClient":
        return self

    def __exit__(self, *_exc: object) -> None:
        self.close()


_default_client: KruiseClient | None = None
_default_client_lock = threading.Lock()


def default_client() -> KruiseClient:
    """Process-wide client built from the E2B environment variables (lazily created)."""

    global _default_client  # noqa: PLW0603
    if _default_client is None:
        with _default_client_lock:
            if _default_client is None:
                _default_client = KruiseClient()
    return _default_client
