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

"""Shared helpers for the hand-written Kruise business models."""

from pydantic import ConfigDict


def to_camel(name: str) -> str:
    """Convert a snake_case field name to camelCase for wire serialization.

    Field names without underscores (e.g. camelCase names inherited from the
    generated Kubernetes models) are returned unchanged, so this generator can
    be applied to models that subclass the generated ones.
    """

    head, *rest = name.split("_")
    return head + "".join(part[:1].upper() + part[1:] for part in rest)


camel_config = ConfigDict(alias_generator=to_camel, populate_by_name=True)
