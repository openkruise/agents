"""Shared helpers for sandbox-gateway end-to-end tests."""

import json
import subprocess


def get_sandbox_resource(sandbox_id: str) -> dict:
    """Return the Sandbox resource identified by namespace--name."""
    namespace, separator, name = sandbox_id.partition("--")
    if not separator:
        namespace, name = "default", namespace
    result = subprocess.run(
        ["kubectl", "get", "sandbox", name, "-n", namespace, "-o", "json"],
        capture_output=True,
        text=True,
        check=True,
    )
    return json.loads(result.stdout)


def get_sandbox_access_token(sandbox_id: str) -> str:
    """Return the runtime access token annotation, if present."""
    sandbox = get_sandbox_resource(sandbox_id)
    annotations = sandbox.get("metadata", {}).get("annotations", {})
    return annotations.get("agents.kruise.io/runtime-access-token", "")


def get_sandbox_uid(sandbox_id: str) -> str:
    """Return the immutable Sandbox UID."""
    return get_sandbox_resource(sandbox_id)["metadata"]["uid"]
