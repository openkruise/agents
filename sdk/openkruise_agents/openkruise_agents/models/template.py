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

"""SandboxTemplate business models.

Field types for the PodTemplate layer (Container, ResourceRequirements,
scheduling, security, ...) are imported from the generated Kubernetes
OpenAPI models in :mod:`openkruise_agents.models.k8s` so the SDK never
maintains a hand-written subset of Kubernetes container/volume/security
fields.

Wire format: every field serializes to camelCase (``model_dump(by_alias=
True)``), matching the Kubernetes JSON convention used by the
sandbox-manager resource API.
"""

from enum import Enum
from typing import Any

from pydantic import BaseModel, Field

from ..models.k8s import IoK8sApimachineryPkgApisMetaV1Condition as Condition
from ..models.k8s import IoK8sApimachineryPkgApisMetaV1ObjectMeta as ObjectMeta
from ..models.k8s import IoK8sApiCoreV1PodSpec as PodSpec
from ._common import camel_config


class TemplateBackend(str, Enum):
    """Backend selector for ``Template.build``.

    Kubernetes is the default backend and is omitted from the wire request;
    only Substrate is sent explicitly.
    """

    KUBERNETES = "kubernetes"
    SUBSTRATE = "substrate"


class PersistentContent(str, Enum):
    """Content classes captured by a Template Build checkpoint."""

    FILESYSTEM = "filesystem"
    MEMORY = "memory"


class RuntimeConfig(BaseModel):
    """Sandbox runtime injection configuration (``spec.runtimes[]``)."""

    model_config = camel_config

    name: str
    args: dict[str, Any] | None = None


class PauseStrategy(BaseModel):
    """Sandbox pause strategy (``spec.pauseStrategy``).

    The exact schema is owned by the Sandbox CRD; unknown fields are kept on
    round-trip so the SDK does not need to track every server-side addition.
    """

    model_config = {**camel_config, "extra": "allow"}

    enabled: bool | None = None


class BusinessStartPolicy(str, Enum):
    """When the business process command starts (Kubernetes Serving only)."""

    AFTER_CLAIM_INITIALIZATION = "after_claim_initialization"


class BusinessProcessOptions(BaseModel):
    """Business process startup configuration for a Build."""

    model_config = camel_config

    start_policy: BusinessStartPolicy | None = None


class RunCommand(BaseModel):
    """One queued ``run_cmd`` provisioning command (Kubernetes only).

    ``command`` accepts a string or a list of strings; the list is executed
    sequentially on the server and any non-zero exit fails the Build.
    """

    model_config = camel_config

    command: str | list[str]
    user: str | None = None


class TemplateBuildOptions(BaseModel):
    """Build behavior options; persisted server-side as ``spec.templateBuildOptions``."""

    model_config = camel_config

    checkpoint: bool = False
    image_cache: bool = False
    business_process: BusinessProcessOptions | None = None
    # run_cmd queue; populated by Template.run_cmd() chain calls. Kubernetes
    # backend only — a Substrate Build request containing it is rejected.
    run_commands: list[RunCommand] = Field(default_factory=list)


class SandboxTemplateSpec(PodSpec):
    """Public SandboxTemplate spec: full PodTemplate workload fields plus
    Kruise sandbox fields.

    Subclasses the generated Kubernetes ``PodSpec`` so container, volume,
    scheduling, security and networking fields stay identical to upstream
    Kubernetes; the template ``metadata`` (labels/annotations) and the Kruise
    sandbox fields are added on top. Field names inherited from Kubernetes
    keep their upstream camelCase spelling.
    """

    model_config = {**camel_config, "extra": "allow"}

    metadata: ObjectMeta | None = None
    persistent_contents: list[PersistentContent] | None = None
    runtimes: list[RuntimeConfig] | None = None
    pause_strategy: PauseStrategy | None = None


class SandboxTemplatePhase(str, Enum):
    """Lifecycle phase reported in ``SandboxTemplate.status``."""

    PENDING = "Pending"
    BUILDING = "Building"
    READY = "Ready"
    FAILED = "Failed"


class TemplateCheckpointRef(BaseModel):
    """Reference to the checkpoint object backing a Kubernetes artifact."""

    model_config = camel_config

    name: str
    namespace: str | None = None


class TemplateArtifactRef(BaseModel):
    """Current Build artifact; the single source of truth for Create.

    There is no public build identifier: 1 Template = 1 Build, the internal
    ActorTemplate carries the Template name itself.
    """

    model_config = camel_config

    backend: str
    checkpoint_ref: TemplateCheckpointRef | None = None
    actor_template_name: str | None = None


class SandboxTemplateStatus(BaseModel):
    """Public projection of ``SandboxTemplate.status``, written exclusively by
    the SandboxTemplate controller."""

    model_config = {**camel_config, "extra": "allow"}

    observed_generation: int | None = None
    phase: SandboxTemplatePhase | None = None
    artifact_ref: TemplateArtifactRef | None = None
    message: str | None = None
    start_time: str | None = None
    ready_time: str | None = None
    conditions: list[Condition] | None = None


class TemplateBuild(BaseModel):
    """Handle returned by ``Template.build()`` and refreshed while polling.

    The Template name is the Build identity: 1 Template = 1 Build.
    """

    model_config = {**camel_config, "extra": "allow"}

    template_id: str
    status: SandboxTemplatePhase | None = None
    artifact: TemplateArtifactRef | None = None
    message: str | None = None

    @classmethod
    def from_template_status(cls, name: str, status: SandboxTemplateStatus) -> "TemplateBuild":
        return cls(
            template_id=name,
            status=status.phase,
            artifact=status.artifact_ref,
            message=status.message,
        )
