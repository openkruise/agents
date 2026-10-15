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

"""SandboxTemplate resource API.

Two supported call styles, mirroring the interface proposal:

1. One-shot::

    build = Template.build(name="runtime", spec=..., options=...)
    build.wait_until_ready()

2. Chained ``run_cmd`` provisioning (Kubernetes backend only)::

    template = Template(spec=..., options=...).run_cmd(["pip install ..."])
    build = Template.build(template, "runtime", on_build_logs=logger)

``Template.get`` and ``Template.delete`` round out the lifecycle; delete is
refused with 409 while a SandboxSet references the Template.
"""

from __future__ import annotations

import time
from typing import Callable
from urllib.parse import quote

import httpx
from pydantic import PrivateAttr

from .client import KruiseClient, default_client
from .errors import KruiseApiError, TemplateBuildFailedError, WaitTimeoutError
from .models import (
    PersistentContent,
    RunCommand,
    SandboxTemplatePhase,
    SandboxTemplateSpec,
    SandboxTemplateStatus,
    TemplateBackend,
    TemplateBuild,
    TemplateBuildOptions,
)

LogCallback = Callable[[str], None]

# Transient polling failures (gateway errors / connection resets) are retried
# with exponential backoff before surfacing; Build polling runs for minutes.
_RETRYABLE_STATUS = frozenset({502, 503, 504})
_MAX_TRANSIENT_RETRIES = 5


def default_build_logger() -> LogCallback:
    """A build log callback that prints every received line to stdout."""

    def _log(line: str) -> None:
        print(line)

    return _log


class Template:
    """Client-side builder for a logical SandboxTemplate."""

    def __init__(
        self,
        spec: SandboxTemplateSpec,
        options: TemplateBuildOptions | None = None,
        backend: TemplateBackend | None = None,
        client: KruiseClient | None = None,
    ) -> None:
        if not isinstance(spec, SandboxTemplateSpec):
            raise TypeError("spec must be an openkruise_agents.models.SandboxTemplateSpec")
        self.spec = spec
        # Deep-copy so run_cmd() never mutates a caller-owned options object
        # shared across templates or repeated builds.
        self.options = (
            options.model_copy(deep=True) if options is not None else TemplateBuildOptions()
        )
        self.backend = backend
        self.client = client

    def run_cmd(self, command: str | list[str], user: str | None = None) -> "Template":
        """Queue a provisioning command executed by envd in the business
        container during a Kubernetes Build.

        Kubernetes backend only; a Substrate Build request carrying queued
        commands is rejected with Unsupported by the server. The server
        requires ``checkpoint=True`` with filesystem persistence; this is
        validated client-side so misconfiguration fails before any request.
        """

        if not self.options.checkpoint:
            raise ValueError("run_cmd requires options.checkpoint=True")
        contents = self.spec.persistent_contents or []
        if PersistentContent.FILESYSTEM not in contents:
            raise ValueError(
                "run_cmd requires spec.persistent_contents to include PersistentContent.FILESYSTEM"
            )
        self.options.run_commands.append(RunCommand(command=command, user=user))
        return self

    @classmethod
    def build(
        cls,
        *args: object,
        name: str | None = None,
        spec: SandboxTemplateSpec | None = None,
        options: TemplateBuildOptions | None = None,
        backend: TemplateBackend | None = None,
        client: KruiseClient | None = None,
        on_build_logs: LogCallback | None = None,
    ) -> "TemplateBuildHandle":
        """Create the logical SandboxTemplate and start an async Build.

        Accepts either ``Template.build(template, name, ...)`` with a
        :class:`Template` instance, or keyword-style
        ``Template.build(name=..., spec=..., options=..., backend=...)``.
        """

        template: Template | None = None
        if args:
            first = args[0]
            if isinstance(first, Template):
                template = first
                if len(args) > 1:
                    if name is not None:
                        raise TypeError("name passed both positionally and as keyword")
                    if not isinstance(args[1], str):
                        raise TypeError(
                            f"second positional argument must be the template name string, "
                            f"got {type(args[1]).__name__}"
                        )
                    name = args[1]
                if len(args) > 2:
                    raise TypeError("build() accepts at most (template, name) positionally")
            elif isinstance(first, str):
                if len(args) > 1:
                    raise TypeError("build() accepts at most (template, name) positionally")
                if name is not None:
                    raise TypeError("name passed both positionally and as keyword")
                name = first
            else:
                raise TypeError("first positional argument must be a Template or a name string")

        if template is not None:
            spec = spec if spec is not None else template.spec
            options = options if options is not None else template.options
            backend = backend if backend is not None else template.backend
            client = client if client is not None else template.client

        if not name:
            raise ValueError("template name is required")
        if spec is None:
            raise ValueError("spec is required")

        effective_client = client or default_client()
        body: dict[str, object] = {
            "name": name,
            "spec": spec.model_dump(mode="json", by_alias=True, exclude_none=True),
        }
        if options is not None:
            body["options"] = options.model_dump(mode="json", by_alias=True, exclude_none=True)
        if backend is not None:
            body["backend"] = backend.value

        response = effective_client.request("POST", "/templates", body=body)
        handle = TemplateBuildHandle._from_response(name, response, effective_client, on_build_logs)
        handle._emit_logs(response)
        return handle

    @classmethod
    def get(cls, name: str, client: KruiseClient | None = None) -> "TemplateBuildHandle":
        """Fetch a Template via ``GET /templates/{name}``."""

        effective_client = client or default_client()
        response = effective_client.request("GET", f"/templates/{quote(name, safe='')}")
        return TemplateBuildHandle._from_response(name, response, effective_client)

    @classmethod
    def delete(cls, name: str, client: KruiseClient | None = None) -> None:
        """Delete a Template via ``DELETE /templates/{name}``.

        Deleting a missing Template is an idempotent success (the server
        answers 204 for both). Raises :class:`ConflictError` while a
        SandboxSet still references the Template.
        """

        effective_client = client or default_client()
        effective_client.request("DELETE", f"/templates/{quote(name, safe='')}")


class TemplateBuildHandle(TemplateBuild):
    """``TemplateBuild`` plus the client context needed for polling."""

    _client: KruiseClient | None = PrivateAttr(default=None)
    _on_build_logs: LogCallback | None = PrivateAttr(default=None)

    @classmethod
    def _from_response(
        cls,
        name: str,
        response: dict,
        client: KruiseClient | None = None,
        on_build_logs: LogCallback | None = None,
    ) -> "TemplateBuildHandle":
        status = response.get("status")
        if isinstance(status, dict):
            parsed = SandboxTemplateStatus.model_validate(status)
            handle = cls(
                template_id=name,
                status=parsed.phase,
                artifact=parsed.artifact_ref,
                message=parsed.message,
            )
        else:
            handle = cls(template_id=name)
        handle._client = client
        handle._on_build_logs = on_build_logs
        return handle

    def _emit_logs(self, response: dict) -> None:
        if self._on_build_logs is not None:
            for line in response.get("buildLogs") or []:
                self._on_build_logs(str(line))

    def wait_until_ready(self, timeout: float = 600.0, interval: float = 2.0) -> "TemplateBuildHandle":
        """Poll ``GET /templates/{id}`` until phase is Ready.

        Transient polling failures (502/503/504 or connection errors) are
        retried with exponential backoff, up to ``_MAX_TRANSIENT_RETRIES``
        consecutive attempts, without exceeding the deadline.

        Raises :class:`TemplateBuildFailedError` when the Build reaches
        Failed, :class:`WaitTimeoutError` when ``timeout`` expires, and the
        last :class:`KruiseApiError` when retries are exhausted.
        """

        client = self._client or default_client()
        deadline = time.monotonic() + timeout
        transient_retries = 0
        backoff = interval
        while True:
            try:
                response = client.request("GET", f"/templates/{quote(self.template_id, safe='')}")
            except KruiseApiError as exc:
                if exc.status_code not in _RETRYABLE_STATUS:
                    raise
                transient_retries += 1
                if transient_retries > _MAX_TRANSIENT_RETRIES:
                    raise
            except httpx.TransportError as exc:
                transient_retries += 1
                if transient_retries > _MAX_TRANSIENT_RETRIES:
                    raise KruiseApiError(
                        503,
                        f"transport error while polling template build: "
                        f"{transient_retries} consecutive failures: {exc!r}",
                    ) from exc
            else:
                transient_retries = 0
                latest = TemplateBuildHandle._from_response(self.template_id, response)
                self.status = latest.status
                self.artifact = latest.artifact
                self.message = latest.message
                self._emit_logs(response)
                if self.status is SandboxTemplatePhase.READY:
                    return self
                if self.status is SandboxTemplatePhase.FAILED:
                    raise TemplateBuildFailedError(self.template_id, self.message)
            if time.monotonic() >= deadline:
                raise WaitTimeoutError(f"template {self.template_id}", "Ready", timeout)
            if transient_retries:
                time.sleep(min(backoff, 30.0))
                backoff = min(backoff * 2, 30.0)
            else:
                # A successful poll restarts the ramp from the base interval.
                time.sleep(interval)
                backoff = interval
