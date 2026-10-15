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

"""SandboxSet (WarmPool) resource API."""

from __future__ import annotations

import time
from urllib.parse import quote

from .client import KruiseClient, default_client
from .errors import WaitTimeoutError
from .models import SandboxSetResource, SandboxSetSpec


class SandboxSet:
    """Handle for a SandboxSet WarmPool resource."""

    def __init__(self, resource: SandboxSetResource, client: KruiseClient | None = None) -> None:
        self.resource = resource
        self._client = client

    @property
    def name(self) -> str:
        return self.resource.name

    @property
    def ready(self) -> bool | None:
        return self.resource.ready

    @classmethod
    def create(
        cls,
        name: str | None = None,
        template_ref: str | None = None,
        spec: SandboxSetSpec | None = None,
        client: KruiseClient | None = None,
    ) -> "SandboxSet":
        """Create a WarmPool via ``POST /sandboxsets``.

        The public request only carries the business template reference and
        pool fields; the server resolves the backend from the referenced
        Template Build and never receives an inline worker PodTemplate.
        """

        if not name:
            raise ValueError("sandboxset name is required")
        if not template_ref:
            raise ValueError("template_ref is required")

        # Deep-copy so the caller-owned spec is never mutated.
        effective_spec = (
            spec.model_copy(deep=True) if spec is not None else SandboxSetSpec()
        )
        effective_spec.template_ref = template_ref
        body = {
            "name": name,
            "spec": effective_spec.model_dump(mode="json", by_alias=True, exclude_none=True),
        }
        effective_client = client or default_client()
        response = effective_client.request("POST", "/sandboxsets", body=body)
        resource = SandboxSetResource.from_api(response, fallback_name=name)
        return cls(resource, effective_client)

    @classmethod
    def get(cls, name: str, client: KruiseClient | None = None) -> "SandboxSet":
        """Fetch a SandboxSet via ``GET /sandboxsets/{name}``."""

        effective_client = client or default_client()
        response = effective_client.request("GET", f"/sandboxsets/{quote(name, safe='')}")
        return cls(SandboxSetResource.from_api(response, fallback_name=name), effective_client)

    def _refresh(self) -> None:
        client = self._client or default_client()
        response = client.request("GET", f"/sandboxsets/{quote(self.name, safe='')}")
        self.resource = SandboxSetResource.from_api(response, fallback_name=self.name)

    def wait_until_ready(self, timeout: float = 300.0, interval: float = 2.0) -> "SandboxSet":
        """Poll until the pool reports ready.

        Raises :class:`WaitTimeoutError` when ``timeout`` expires.
        """

        deadline = time.monotonic() + timeout
        while True:
            self._refresh()
            if self.resource.ready:
                return self
            if time.monotonic() >= deadline:
                raise WaitTimeoutError(f"sandboxset {self.name}", "ready", timeout)
            time.sleep(interval)
