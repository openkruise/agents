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

from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[1]


def pytest_sessionstart() -> None:
    subprocess.run(
        [
            sys.executable,
            "scripts/generate_k8s_models.py",
            "--output",
            "openkruise_agents/models/k8s/__init__.py",
        ],
        cwd=ROOT,
        check=True,
    )
