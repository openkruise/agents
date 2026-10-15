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

"""Generate the Kruise SDK Kubernetes Pydantic models from the upstream swagger."""

from __future__ import annotations

import argparse
import hashlib
import os
import ssl
import subprocess
import sys
import tempfile
import urllib.request
from pathlib import Path


KUBERNETES_VERSION = "v1.35.0"
SWAGGER_URL = (
    "https://raw.githubusercontent.com/kubernetes/kubernetes/"
    f"{KUBERNETES_VERSION}/api/openapi-spec/swagger.json"
)
SWAGGER_SHA256 = "483500149ee52ce5753d75f5639101d985bb4f5e902cc05b1ba7627465d62446"


def verify_swagger(data: bytes) -> None:
    digest = hashlib.sha256(data).hexdigest()
    if digest != SWAGGER_SHA256:
        raise RuntimeError(
            f"Kubernetes {KUBERNETES_VERSION} swagger checksum mismatch: "
            f"expected {SWAGGER_SHA256}, got {digest}"
        )


def download_swagger(destination: Path) -> None:
    # Honor explicit CA overrides first; Homebrew and minimal Python installs
    # often lack a usable system CA bundle, so certifi is the fallback.
    cafile = os.environ.get("SSL_CERT_FILE")
    if cafile is None:
        try:
            import certifi
        except ImportError:
            pass
        else:
            cafile = certifi.where()
    context = ssl.create_default_context(cafile=cafile)

    with urllib.request.urlopen(SWAGGER_URL, timeout=120, context=context) as response:
        data = response.read()
    verify_swagger(data)
    destination.write_bytes(data)


def generate(output: Path) -> None:
    output.parent.mkdir(parents=True, exist_ok=True)
    scripts = Path(__file__).resolve().parent

    # Keep intermediates next to the output so publishing is an atomic,
    # same-filesystem rename.
    with tempfile.TemporaryDirectory(dir=output.parent) as work_dir:
        work = Path(work_dir)
        swagger = work / "swagger.json"
        subset = work / "subset.json"
        generated = work / "k8s_models.py"

        download_swagger(swagger)
        subprocess.run(
            [sys.executable, str(scripts / "extract_k8s_subset.py"), str(swagger), str(subset)],
            check=True,
        )
        subprocess.run(
            [
                sys.executable,
                "-m",
                "datamodel_code_generator",
                "--input-file-type",
                "jsonschema",
                "--no-treat-dot-as-module",
                "--input",
                str(subset),
                "--output",
                str(generated),
                "--output-model-type",
                "pydantic_v2.BaseModel",
                "--target-python-version",
                "3.10",
                "--use-schema-description",
                "--collapse-root-models",
                "--disable-timestamp",
            ],
            check=True,
        )
        os.replace(generated, output)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    generate(args.output)


if __name__ == "__main__":
    main()
