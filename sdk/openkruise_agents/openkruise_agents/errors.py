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

"""SDK error types mirroring the resource API status codes."""

from __future__ import annotations


class KruiseError(Exception):
    """Base class for all openkruise_agents errors."""


class KruiseApiError(KruiseError):
    """An error response from the sandbox-manager resource API."""

    def __init__(self, status_code: int, message: str) -> None:
        super().__init__(f"[{status_code}] {message}")
        self.status_code = status_code
        self.message = message


class BadRequestError(KruiseApiError):
    """400: request validation failed (e.g. unsupported PodTemplate field)."""


class AuthenticationError(KruiseApiError):
    """401: missing or invalid API key."""


class PermissionDeniedError(KruiseApiError):
    """403: quota exhausted or operation not allowed for this API key."""


class NotFoundError(KruiseApiError):
    """404: resource not found in the caller's namespace."""


class ConflictError(KruiseApiError):
    """409: conflicting state (e.g. duplicate template name)."""


class UnsupportedOperationError(KruiseApiError):
    """501: the operation is not implemented for the selected backend
    (e.g. PoolAutoscaler with a Substrate target)."""


class TemplateBuildFailedError(KruiseError):
    """The Template Build reached the Failed phase while waiting."""

    def __init__(self, template_id: str, message: str | None = None) -> None:
        detail = f": {message}" if message else ""
        super().__init__(f"template {template_id} build failed{detail}")
        self.template_id = template_id
        self.message = message


class WaitTimeoutError(KruiseError):
    """A wait_until_* helper timed out before reaching the target state."""

    def __init__(self, resource: str, target: str, timeout: float) -> None:
        super().__init__(f"timeout after {timeout}s waiting for {resource} to become {target}")
        self.resource = resource
        self.target = target
        self.timeout = timeout


def api_error_from_response(
    status_code: int,
    payload: dict | None,
    raw_body: str | None = None,
) -> KruiseApiError:
    """Map an HTTP status code onto the SDK exception hierarchy.

    ``raw_body`` preserves diagnostic information when the error response is
    not JSON (e.g. HTML from a gateway or plain-text proxy error).
    """

    message = ""
    if isinstance(payload, dict):
        message = str(payload.get("message") or payload.get("error") or "")
    elif raw_body:
        message = raw_body
    if status_code == 400:
        return BadRequestError(status_code, message or "bad request")
    if status_code == 401:
        return AuthenticationError(status_code, message or "invalid api key")
    if status_code == 403:
        return PermissionDeniedError(status_code, message or "permission denied")
    if status_code == 404:
        return NotFoundError(status_code, message or "not found")
    if status_code == 409:
        return ConflictError(status_code, message or "conflict")
    if status_code == 501:
        return UnsupportedOperationError(status_code, message or "unsupported operation")
    return KruiseApiError(status_code, message or "api error")
