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

"""PoolAutoscaler business models (Kubernetes backend only in this phase)."""

from pydantic import BaseModel

from ._common import camel_config


class CapacityScalingRules(BaseModel):
    model_config = camel_config

    stabilization_window_seconds: int | None = None


class CapacityPolicy(BaseModel):
    model_config = camel_config

    target_available: int | None = None
    tolerance: str | None = None
    scale_up: CapacityScalingRules | None = None
    scale_down: CapacityScalingRules | None = None


class CronScalingPolicy(BaseModel):
    """A cron scaling rule (``spec.cronPolicies[]``).

    Cron policies take precedence over ``capacity_policy`` when both are set.
    """

    model_config = camel_config

    name: str
    schedule: str
    target_replicas: int
    time_zone: str | None = None


class PoolAutoscalerSpec(BaseModel):
    model_config = {**camel_config, "extra": "allow"}

    min_replicas: int | None = None
    max_replicas: int | None = None
    capacity_policy: CapacityPolicy | None = None
    cron_policies: list[CronScalingPolicy] | None = None
    suspend: bool | None = None


class PoolAutoscalerResource(BaseModel):
    """Resource representation returned by the sandbox-manager API.

    Accepts both a flat projection and the CR-shaped response where the
    autoscaler fields are nested under ``spec``.
    """

    model_config = {**camel_config, "extra": "allow"}

    name: str
    scale_target_ref: str | None = None
    active: bool | None = None
    suspend: bool | None = None
    observed_generation: int | None = None
    message: str | None = None

    @classmethod
    def from_api(cls, response: dict, fallback_name: str | None = None) -> "PoolAutoscalerResource":
        spec = response.get("spec") if isinstance(response.get("spec"), dict) else {}
        merged = {**spec, **{k: v for k, v in response.items() if k != "spec"}}
        merged["name"] = merged.get("name") or response.get("name") or fallback_name
        return cls.model_validate(merged)
