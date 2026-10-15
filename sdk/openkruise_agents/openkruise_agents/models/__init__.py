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

"""Public SDK models.

Kubernetes field types are re-exported here from the generated OpenAPI
models (:mod:`openkruise_agents.models.k8s`) so user code can write::

    from openkruise_agents.models import Container, ResourceRequirements

without touching the generated module directly. The generated module is
built by ``scripts/generate_k8s_models.py`` at package build time.
"""

from .autoscaler import (
    CapacityPolicy,
    CapacityScalingRules,
    CronScalingPolicy,
    PoolAutoscalerResource,
    PoolAutoscalerSpec,
)
from .k8s import IoK8sApiCoreV1Container as Container
from .k8s import IoK8sApiCoreV1PodSpec as PodSpec
from .k8s import IoK8sApiCoreV1PodTemplateSpec as PodTemplateSpec
from .k8s import IoK8sApiCoreV1ResourceRequirements as ResourceRequirements
from .k8s import IoK8sApimachineryPkgApisMetaV1LabelSelector as LabelSelector
from .k8s import IoK8sApimachineryPkgApisMetaV1ObjectMeta as ObjectMeta
from .sandboxset import (
    SandboxSetResource,
    SandboxSetScaleStrategy,
    SandboxSetSpec,
)
from .template import (
    BusinessProcessOptions,
    BusinessStartPolicy,
    PauseStrategy,
    PersistentContent,
    RunCommand,
    RuntimeConfig,
    SandboxTemplatePhase,
    SandboxTemplateSpec,
    SandboxTemplateStatus,
    TemplateArtifactRef,
    TemplateBackend,
    TemplateBuild,
    TemplateBuildOptions,
    TemplateCheckpointRef,
)

__all__ = [
    # generated Kubernetes types (re-exported)
    "Container",
    "LabelSelector",
    "ObjectMeta",
    "PodSpec",
    "PodTemplateSpec",
    "ResourceRequirements",
    # template models
    "BusinessProcessOptions",
    "BusinessStartPolicy",
    "PauseStrategy",
    "PersistentContent",
    "RunCommand",
    "RuntimeConfig",
    "SandboxTemplatePhase",
    "SandboxTemplateSpec",
    "SandboxTemplateStatus",
    "TemplateArtifactRef",
    "TemplateBackend",
    "TemplateBuild",
    "TemplateBuildOptions",
    "TemplateCheckpointRef",
    # sandboxset models
    "SandboxSetResource",
    "SandboxSetScaleStrategy",
    "SandboxSetSpec",
    # autoscaler models
    "CapacityPolicy",
    "CapacityScalingRules",
    "CronScalingPolicy",
    "PoolAutoscalerResource",
    "PoolAutoscalerSpec",
]
