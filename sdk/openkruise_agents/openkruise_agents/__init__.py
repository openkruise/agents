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

"""OpenKruise Agents resource-management SDK.

Manages Templates, SandboxSets, and PoolAutoscalers through the
sandbox-manager resource API at ``/agents.kruise.io/v1alpha1``; end users
never hold Kubernetes credentials. E2B Sandbox compatibility lives in the
separate ``sdk/customized_e2b`` package.
"""

from .client import KruiseClient, default_client
from .errors import (
    AuthenticationError,
    BadRequestError,
    ConflictError,
    KruiseApiError,
    KruiseError,
    NotFoundError,
    PermissionDeniedError,
    TemplateBuildFailedError,
    UnsupportedOperationError,
    WaitTimeoutError,
)
from .poolautoscaler import PoolAutoscaler
from .sandboxset import SandboxSet
from .template import Template, TemplateBuildHandle, default_build_logger

__all__ = [
    # resource APIs
    "Template",
    "TemplateBuildHandle",
    "SandboxSet",
    "PoolAutoscaler",
    "default_build_logger",
    # client
    "KruiseClient",
    "default_client",
    # errors
    "KruiseError",
    "KruiseApiError",
    "BadRequestError",
    "AuthenticationError",
    "PermissionDeniedError",
    "NotFoundError",
    "ConflictError",
    "UnsupportedOperationError",
    "TemplateBuildFailedError",
    "WaitTimeoutError",
]
