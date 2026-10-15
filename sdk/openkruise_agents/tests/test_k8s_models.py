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

from openkruise_agents.models.k8s import (
    IoK8sApiCoreV1Container,
    IoK8sApiCoreV1PodSpec,
    IoK8sApiCoreV1ResourceRequirements,
)


def test_generated_models_serialize_nested_pod_spec() -> None:
    pod_spec = IoK8sApiCoreV1PodSpec(
        containers=[
            IoK8sApiCoreV1Container(
                name="runtime",
                image="example/runtime@sha256:deadbeef",
                resources=IoK8sApiCoreV1ResourceRequirements(
                    limits={"cpu": "1", "memory": "1Gi"}
                ),
            )
        ]
    )

    assert pod_spec.model_dump(by_alias=True)["containers"][0]["resources"]["limits"] == {
        "cpu": "1",
        "memory": "1Gi",
    }
