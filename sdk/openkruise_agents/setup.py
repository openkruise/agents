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

from setuptools import find_packages, setup
from setuptools.command.build_py import build_py as build_py_base
from setuptools.command.sdist import sdist as sdist_base

try:
    from setuptools.command.editable_wheel import editable_wheel as editable_wheel_base
except ImportError:
    editable_wheel_base = None

ROOT = Path(__file__).parent.resolve()
SOURCE_MODEL = ROOT / "openkruise_agents" / "models" / "k8s" / "__init__.py"


def generate_models(output: Path) -> None:
    subprocess.run(
        [
            sys.executable,
            str(ROOT / "scripts" / "generate_k8s_models.py"),
            "--output",
            str(output),
        ],
        check=True,
    )


class build_py(build_py_base):
    def run(self) -> None:
        super().run()
        generate_models(Path(self.build_lib) / "openkruise_agents" / "models" / "k8s" / "__init__.py")


class sdist(sdist_base):
    def make_release_tree(self, base_dir: str, files: list[str]) -> None:
        super().make_release_tree(base_dir, files)
        generated = Path(base_dir) / SOURCE_MODEL.relative_to(ROOT)
        if generated.exists():
            generated.unlink()


cmdclass = {"build_py": build_py, "sdist": sdist}
if editable_wheel_base is not None:

    class editable_wheel(editable_wheel_base):
        def run(self) -> None:
            generate_models(SOURCE_MODEL)
            super().run()

    cmdclass["editable_wheel"] = editable_wheel


setup(
    packages=find_packages(),
    include_package_data=True,
    cmdclass=cmdclass,
)
