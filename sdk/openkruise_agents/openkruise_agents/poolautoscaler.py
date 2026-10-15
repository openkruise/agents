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

"""PoolAutoscaler resource API (Kubernetes backend only in this phase).

Creating an autoscaler for a Substrate-backed SandboxSet is rejected by the
server with ``501 Not Implemented``.
"""

from __future__ import annotations

import time
from urllib.parse import quote

from .client import KruiseClient, default_client
from .errors import WaitTimeoutError
from .models import PoolAutoscalerResource, PoolAutoscalerSpec


class PoolAutoscaler:
    """Handle for a PoolAutoscaler resource."""

    def __init__(self, resource: PoolAutoscalerResource, client: KruiseClient | None = None) -> None:
        self.resource = resource
        self._client = client

    @property
    def name(self) -> str:
        return self.resource.name

    @property
    def active(self) -> bool | None:
        return self.resource.active

    @classmethod
    def create(
        cls,
        name: str | None = None,
        scale_target_ref: str | None = None,
        spec: PoolAutoscalerSpec | None = None,
        client: KruiseClient | None = None,
    ) -> "PoolAutoscaler":
        """Create an autoscaler via ``POST /poolautoscalers``."""

        if not name:
            raise ValueError("poolautoscaler name is required")
        if not scale_target_ref:
            raise ValueError("scale_target_ref is required")

        body = {
            "name": name,
            "scaleTargetRef": scale_target_ref,
            "spec": (spec if spec is not None else PoolAutoscalerSpec())
            .model_dump(mode="json", by_alias=True, exclude_none=True),
        }
        effective_client = client or default_client()
        response = effective_client.request("POST", "/poolautoscalers", body=body)
        resource = PoolAutoscalerResource.from_api(response, fallback_name=name)
        return cls(resource, effective_client)

    @classmethod
    def get(cls, name: str, client: KruiseClient | None = None) -> "PoolAutoscaler":
        """Fetch an autoscaler via ``GET /poolautoscalers/{name}``."""

        effective_client = client or default_client()
        response = effective_client.request("GET", f"/poolautoscalers/{quote(name, safe='')}")
        return cls(PoolAutoscalerResource.from_api(response, fallback_name=name), effective_client)

    def _refresh(self) -> None:
        client = self._client or default_client()
        response = client.request("GET", f"/poolautoscalers/{quote(self.name, safe='')}")
        self.resource = PoolAutoscalerResource.from_api(response, fallback_name=self.name)

    def wait_until_active(self, timeout: float = 300.0, interval: float = 2.0) -> "PoolAutoscaler":
        """Poll until the autoscaler reports active.

        Raises :class:`WaitTimeoutError` when ``timeout`` expires.
        """

        deadline = time.monotonic() + timeout
        while True:
            self._refresh()
            if self.resource.active:
                return self
            if time.monotonic() >= deadline:
                raise WaitTimeoutError(f"poolautoscaler {self.name}", "active", timeout)
            time.sleep(interval)
