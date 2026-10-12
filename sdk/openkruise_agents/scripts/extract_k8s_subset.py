#!/usr/bin/env python3
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

"""Extract the Kubernetes definitions reachable from the Kruise SDK roots."""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

ROOTS = (
    "io.k8s.api.core.v1.PodTemplateSpec",
    "io.k8s.api.core.v1.PodSpec",
    "io.k8s.api.core.v1.Container",
    "io.k8s.api.core.v1.ResourceRequirements",
    "io.k8s.api.core.v1.PersistentVolumeClaim",
    "io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta",
    "io.k8s.apimachinery.pkg.apis.meta.v1.LabelSelector",
    "io.k8s.apimachinery.pkg.apis.meta.v1.Condition",
)


def walk_refs(value: Any) -> list[str]:
    if isinstance(value, dict):
        refs: list[str] = []
        for key, child in value.items():
            if key == "$ref" and isinstance(child, str):
                prefix = "#/definitions/"
                if not child.startswith(prefix):
                    raise ValueError(f"unexpected external reference: {child}")
                refs.append(child.removeprefix(prefix))
            else:
                refs.extend(walk_refs(child))
        return refs
    if isinstance(value, list):
        return [ref for child in value for ref in walk_refs(child)]
    return []


def extract(swagger: dict[str, Any]) -> dict[str, Any]:
    definitions = swagger.get("definitions")
    if not isinstance(definitions, dict):
        raise ValueError("swagger document has no definitions")

    missing = sorted(set(ROOTS).difference(definitions))
    if missing:
        raise ValueError(f"root definitions missing from swagger: {missing}")

    seen: set[str] = set()
    pending = list(ROOTS)
    while pending:
        name = pending.pop()
        if name in seen:
            continue
        seen.add(name)
        pending.extend(walk_refs(definitions[name]))

    return {
        "$schema": "http://json-schema.org/draft-04/schema#",
        "title": "KruiseKubernetesModels",
        "definitions": {name: definitions[name] for name in sorted(seen)},
    }


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit(f"usage: {Path(sys.argv[0]).name} SWAGGER OUTPUT")

    source, destination = map(Path, sys.argv[1:])
    swagger = json.loads(source.read_text(encoding="utf-8"))
    destination.write_text(
        json.dumps(extract(swagger), indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
