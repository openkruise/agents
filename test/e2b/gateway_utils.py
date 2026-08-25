"""Shared helpers for sandbox-gateway end-to-end tests."""

import json
import subprocess

from utils import resolve_sandbox_cr


def get_sandbox_resource(sandbox_id: str) -> dict:
    """Return the Sandbox resource backing an opaque sandbox ID."""
    namespace, name = resolve_sandbox_cr(sandbox_id)
    if not namespace or not name:
        raise LookupError(f"cannot resolve Sandbox CR for sandbox ID {sandbox_id}")
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
