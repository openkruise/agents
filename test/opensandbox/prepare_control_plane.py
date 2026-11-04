# Copyright 2026 The OpenKruise Authors.
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

"""Render isolated control-plane E2E manifests with PyYAML; never contact a cluster."""

import argparse
from collections import Counter
import copy
import json
import os
from pathlib import Path
import re

import yaml


LABELS = {"opensandbox-control-plane-e2e": "true"}
ASSETS = Path(__file__).resolve().parents[1] / "e2b" / "assets"
VARIABLE = re.compile(r"\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))")


def load_pool(path, runtime_image):
    environment = {**os.environ, "RUNTIME_IMG": runtime_image}

    def expand(match):
        name = match.group(1) or match.group(2)
        if name not in environment:
            raise ValueError(f"{path.name}: environment variable {name} is unset")
        return environment[name]

    text = VARIABLE.sub(expand, path.read_text())
    if VARIABLE.search(text):
        raise ValueError(f"{path.name}: unresolved environment variable")
    pool = yaml.safe_load(text)
    if not isinstance(pool, dict) or pool.get("kind") != "SandboxSet":
        raise ValueError(f"{path.name}: expected one SandboxSet")
    pod = pool["spec"]["template"]["spec"]
    native = [container for container in pod.get("initContainers", []) if container["name"] == "runtime"]
    if len(native) != 1:
        raise ValueError(f"{path.name}: expected one native runtime init container")
    native[0]["image"] = runtime_image
    return pool


def validate_pod(pod):
    for category, items in (
            ("container", [*pod.get("initContainers", []), *pod.get("containers", [])]),
            ("volume", pod.get("volumes", []))):
        duplicates = sorted(name for name, count in Counter(item["name"] for item in items).items() if count > 1)
        if duplicates:
            raise ValueError(f"duplicate {category} names: {', '.join(duplicates)}")
    volumes = {volume["name"] for volume in pod.get("volumes", [])}
    for container in [*pod.get("initContainers", []), *pod.get("containers", [])]:
        for mount in container.get("volumeMounts", []):
            if mount["name"] not in volumes:
                raise ValueError(f"{container['name']}: missing volume {mount['name']}")


def render(runtime_image, workload_image=None):
    compatibility = {
        "containers": [{
            "name": "opensandbox-runtime", "image": runtime_image, "imagePullPolicy": "IfNotPresent",
            "command": ["/workspace/opensandbox-runtime"],
            "ports": [{"name": "execd", "containerPort": 44772}],
            "volumeMounts": [{"name": "opensandbox-state", "mountPath": "/var/run/opensandbox"}],
            "readinessProbe": {"tcpSocket": {"port": 44772}, "periodSeconds": 1},
        }],
        "volume": [{"name": "opensandbox-state", "emptyDir": {}}],
    }
    manifests = {}
    native = None
    for source, name, replicas in (
            ("code-interpreter", "opensandbox-control-plane", 4),
            ("code-interpreter-0", "opensandbox-control-plane-empty", 0)):
        pool = load_pool(ASSETS / f"sandboxset-{source}.yaml", runtime_image)
        pod = pool["spec"]["template"]["spec"]
        workload = next(container for container in pod["containers"] if container["name"] == "sandbox")
        if workload_image is not None:
            workload["image"] = workload_image
        if native is None:
            # Cold requests can override the workload command. Inherit envd
            # startup wiring, while Pool-specific application probes stay local.
            native = {
                "mainContainer": {field: copy.deepcopy(workload[field]) for field in (
                    "env", "volumeMounts", "lifecycle")},
                "csiSidecar": copy.deepcopy(pod["initContainers"]),
                "volume": copy.deepcopy(pod["volumes"]),
            }
        pool["metadata"]["name"] = name
        pool["metadata"]["labels"] = {**pool["metadata"].get("labels", {}), **LABELS}
        pool["spec"]["replicas"] = replicas
        metadata = pool["spec"]["template"].setdefault("metadata", {})
        metadata["annotations"] = metadata.get("annotations") or {}
        metadata["labels"] = {**(metadata.get("labels") or {}), **LABELS}
        pod["containers"].extend(copy.deepcopy(compatibility["containers"]))
        pod.setdefault("volumes", []).extend(copy.deepcopy(compatibility["volume"]))
        validate_pod(pod)
        manifests[f"sandboxset-{name}.json"] = pool

    # Use the supported user ConfigMap overlay; the repository's default native
    # injection configuration and the two source Pool assets remain unchanged.
    manifests["runtime-injection.json"] = {
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": "sandbox-injection-config-user", "namespace": "sandbox-system", "labels": LABELS},
        "data": {"agent-runtime": json.dumps(native), "opensandbox-runtime": json.dumps(compatibility)},
    }
    return manifests


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runtime-image", required=True, help="image built from the pinned compatibility runtime source")
    parser.add_argument("--workload-image", default=os.environ.get("CODE_INTERPRETER_IMG") or None,
                        help="Pool workload image; defaults to CODE_INTERPRETER_IMG or the existing asset image")
    parser.add_argument("--output-dir", required=True, type=Path)
    args = parser.parse_args()
    if not args.runtime_image.strip():
        parser.error("--runtime-image must not be empty")
    if args.workload_image is not None and not args.workload_image.strip():
        parser.error("--workload-image must not be empty")
    try:
        manifests = render(args.runtime_image, args.workload_image)
    except (KeyError, ValueError, yaml.YAMLError) as exc:
        parser.exit(2, f"cannot render control-plane manifests: {exc}\n")
    args.output_dir.mkdir(parents=True, exist_ok=True)
    for name, manifest in manifests.items():
        path = args.output_dir / name
        path.write_text(json.dumps(manifest, indent=2) + "\n")
        print(path)


if __name__ == "__main__":
    main()
