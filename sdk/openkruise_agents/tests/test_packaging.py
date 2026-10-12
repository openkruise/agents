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

import os
from pathlib import Path
import subprocess
import sys
import tarfile
import zipfile


ROOT = Path(__file__).resolve().parents[1]
MODEL_PATH = "openkruise_agents/models/k8s/__init__.py"


def test_distribution_artifacts_generate_models(tmp_path: Path) -> None:
    subprocess.run(
        [
            sys.executable,
            "-m",
            "build",
            "--no-isolation",
            "--sdist",
            "--outdir",
            str(tmp_path),
        ],
        cwd=ROOT,
        check=True,
    )

    sdist = next(tmp_path.glob("*.tar.gz"))
    with tarfile.open(sdist) as archive:
        members = archive.getmembers()
        assert not any(member.name.endswith(MODEL_PATH) for member in members)
        assert not any(member.name.endswith(".json") for member in members)
        extract_kwargs = {"filter": "data"} if sys.version_info >= (3, 12) else {}
        archive.extractall(tmp_path / "sdist-source", **extract_kwargs)

    source_tree = next((tmp_path / "sdist-source").iterdir())
    wheel_output = tmp_path / "wheel-output"
    subprocess.run(
        [
            sys.executable,
            "-m",
            "build",
            "--no-isolation",
            "--wheel",
            "--outdir",
            str(wheel_output),
        ],
        cwd=source_tree,
        check=True,
    )

    wheel = next(wheel_output.glob("*.whl"))
    with zipfile.ZipFile(wheel) as archive:
        assert MODEL_PATH in archive.namelist()

    target = tmp_path / "wheel-install"
    subprocess.run(
        [
            sys.executable,
            "-m",
            "pip",
            "install",
            "--no-deps",
            "--target",
            str(target),
            str(wheel),
        ],
        check=True,
    )
    environment = {**os.environ, "PYTHONPATH": str(target)}
    probe = (
        "import pathlib, sys\n"
        "import openkruise_agents.models.k8s as module\n"
        "assert pathlib.Path(module.__file__).resolve().is_relative_to(\n"
        "    pathlib.Path(sys.argv[1]).resolve()\n"
        ")\n"
    )
    subprocess.run(
        [sys.executable, "-c", probe, str(target)],
        check=True,
        cwd=tmp_path,
        env=environment,
    )
