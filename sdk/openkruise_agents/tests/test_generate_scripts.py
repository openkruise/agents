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

import importlib.util
from pathlib import Path

import pytest


ROOT = Path(__file__).resolve().parents[1]


def load_script(name: str):
    path = ROOT / "scripts" / name
    spec = importlib.util.spec_from_file_location(name.removesuffix(".py"), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_verify_swagger_rejects_mismatched_bytes() -> None:
    generate = load_script("generate_k8s_models.py")

    with pytest.raises(RuntimeError, match="checksum mismatch"):
        generate.verify_swagger(b"tampered")


def test_extract_rejects_missing_roots() -> None:
    extract = load_script("extract_k8s_subset.py")

    with pytest.raises(ValueError, match="missing from swagger"):
        extract.extract({"definitions": {}})


def test_extract_rejects_external_references() -> None:
    extract = load_script("extract_k8s_subset.py")
    swagger = {
        "definitions": {
            root: {"type": "object"} for root in extract.ROOTS
        }
    }
    swagger["definitions"][extract.ROOTS[0]]["properties"] = {
        "linked": {"$ref": "#/x-definitions/External"}
    }

    with pytest.raises(ValueError, match="external reference"):
        extract.extract(swagger)


def test_extract_includes_transitive_references() -> None:
    extract = load_script("extract_k8s_subset.py")
    definitions = {root: {"type": "object"} for root in extract.ROOTS}
    definitions["io.k8s.api.core.v1.PodSpec"]["properties"] = {
        "volumes": {"$ref": "#/definitions/io.k8s.api.core.v1.Volume"}
    }
    definitions["io.k8s.api.core.v1.Volume"] = {"type": "object"}
    definitions["io.k8s.api.core.v1.Volume"]["properties"] = {
        "secret": {"$ref": "#/definitions/io.k8s.api.core.v1.SecretVolumeSource"}
    }
    definitions["io.k8s.api.core.v1.SecretVolumeSource"] = {"type": "object"}

    subset = extract.extract({"definitions": definitions})

    assert set(subset["definitions"]) == set(extract.ROOTS) | {
        "io.k8s.api.core.v1.Volume",
        "io.k8s.api.core.v1.SecretVolumeSource",
    }
