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

"""SandboxSet (WarmPool) business models."""

from pydantic import BaseModel

from ._common import camel_config


class SandboxSetScaleStrategy(BaseModel):
    """Scale-up concurrency cap (``spec.scaleStrategy``).

    ``max_unavailable`` accepts an absolute number (``5``) or a percentage
    string (``"10%"``), mirroring the CRD's IntOrString field.
    """

    model_config = {**camel_config, "extra": "allow"}

    max_unavailable: int | str | None = None


class SandboxSetSpec(BaseModel):
    """Public SandboxSet spec: business template reference plus pool fields.

    Per the interface proposal the SDK never exposes the inline
    ``spec.template`` worker PodTemplate; Substrate WorkerPool mapping is a
    server-side implementation detail resolved from ``template_ref``.
    """

    model_config = {**camel_config, "extra": "allow"}

    template_ref: str | None = None
    replicas: int | None = None
    scale_strategy: SandboxSetScaleStrategy | None = None


class SandboxSetResource(BaseModel):
    """Resource representation returned by the sandbox-manager API.

    Accepts both a flat projection and the CR-shaped response where the
    pool fields are nested under ``spec``.
    """

    model_config = {**camel_config, "extra": "allow"}

    name: str
    template_ref: str | None = None
    replicas: int | None = None
    observed_generation: int | None = None
    ready: bool | None = None
    message: str | None = None

    @classmethod
    def from_api(cls, response: dict, fallback_name: str | None = None) -> "SandboxSetResource":
        spec = response.get("spec") if isinstance(response.get("spec"), dict) else {}
        merged = {**spec, **{k: v for k, v in response.items() if k != "spec"}}
        merged["name"] = merged.get("name") or response.get("name") or fallback_name
        return cls.model_validate(merged)
