"""Gateway credentials across snapshots on checkpoint-capable test clusters."""

import os

import conftest_base
import pytest
import requests
from e2b_code_interpreter import Sandbox
from gateway_utils import (
    WORKLOAD_PORT,
    assert_workload_reached,
    gateway_request,
    gateway_request_eventually,
    get_sandbox_resource,
    start_workload_server,
)

pytestmark = [
    pytest.mark.gateway_uuid_auth,
    pytest.mark.skipif(
        os.environ.get("CHECKPOINT_E2E") != "true",
        reason="requires CHECKPOINT_E2E=true and a checkpoint-capable cluster; Kind does not support checkpoints",
    ),
]


@pytest.mark.parametrize(
    "skip_init_runtime",
    [pytest.param(False, id="runtime-init"), pytest.param(True, id="skip-init-runtime")],
)
def test_gateway_uuid_auth_after_snapshot_clone(
    sandbox_context: conftest_base.SandboxContext,
    config: conftest_base.TestConfig,
    skip_init_runtime: bool,
) -> None:
    # Given a source sandbox protected by gateway UUID authentication.
    source = sandbox_context.add(
        Sandbox.create(
            template=config.templates.code_interpreter,
            timeout=600,
            metadata={"e2b.agents.kruise.io/skip-init-runtime": str(skip_init_runtime).lower()},
            headers={"x-request-id": sandbox_context.request_id},
        )
    )
    source_token = source._envd_access_token
    assert source_token, "source create response must contain an access token"
    source_resource = get_sandbox_resource(source.sandbox_id)
    source_has_init = "agents.kruise.io/init-runtime-request" in source_resource["metadata"].get("annotations", {})
    assert source_has_init == (not skip_init_runtime)
    source_response = gateway_request_eventually(config, source.sandbox_id, source_token)
    assert source_response.status_code in (200, 404), source_response.text
    unauthorized = gateway_request(config, source.sandbox_id)
    assert unauthorized.status_code == 401, unauthorized.text

    # When the real snapshot API completes and a sandbox is cloned from it.
    headers = {"X-API-Key": config.api_key, "x-request-id": sandbox_context.request_id}
    snapshot_response = requests.post(
        f"{config.api_url.rstrip('/')}/sandboxes/{source.sandbox_id}/snapshots",
        headers={**headers, "x-e2b-kruise-snapshot-wait-success-seconds": "300"},
        json={},
        timeout=310,
    )
    assert snapshot_response.status_code in (200, 201), snapshot_response.text
    snapshot_id = snapshot_response.json()["snapshotID"]
    assert snapshot_id
    clone_tokens: list[str] = []
    try:
        for _ in range(2):
            clone = sandbox_context.add(
                Sandbox.create(
                    template=snapshot_id,
                    timeout=300,
                    headers={"x-request-id": sandbox_context.request_id},
                )
            )

            # Then skipped-init clones get fresh credentials; normal clones keep theirs.
            clone_token = clone._envd_access_token
            assert clone_token, "clone response must contain a gateway access token"
            clone_resource = get_sandbox_resource(clone.sandbox_id)
            annotations = clone_resource["metadata"].get("annotations", {})
            token_matches = annotations.get("agents.kruise.io/runtime-access-token") == clone_token
            assert token_matches, "clone response token must match its persisted credential"
            clone_has_init = "agents.kruise.io/init-runtime-request" in annotations
            assert clone_has_init == (not skip_init_runtime)
            if skip_init_runtime:
                token_is_new = clone_token != source_token and clone_token not in clone_tokens
                assert token_is_new, "skipped-init clones must receive independent credentials"
            else:
                token_is_preserved = clone_token == source_token
                assert token_is_preserved, "initialized clone must retain its runtime credential"
            ready = gateway_request_eventually(config, clone.sandbox_id, clone_token)
            assert ready.status_code in (200, 404), ready.text
            start_workload_server(clone)
            assert_workload_reached(gateway_request_eventually(
                config, clone.sandbox_id, clone_token, port=WORKLOAD_PORT,
            ))
            host = f"{WORKLOAD_PORT}-{clone.sandbox_id}.{config.e2b_domain}"
            host_valid = requests.get(
                f"{config.gateway_url}/",
                headers={"Host": host, "x-access-token": clone_token},
                timeout=10,
            )
            assert_workload_reached(host_valid)
            rejected_tokens = [None, "wrong-token-value"]
            if skip_init_runtime:
                rejected_tokens.extend([source_token, *clone_tokens])
            for token in rejected_tokens:
                rejected = gateway_request(config, clone.sandbox_id, token, port=WORKLOAD_PORT)
                assert rejected.status_code == 401, rejected.text
                host_headers = {"Host": host}
                if token is not None:
                    host_headers["x-access-token"] = token
                host_rejected = requests.get(
                    f"{config.gateway_url}/", headers=host_headers, timeout=10,
                )
                assert host_rejected.status_code == 401, host_rejected.text
            clone_tokens.append(clone_token)
    finally:
        cleanup = requests.delete(
            f"{config.api_url.rstrip('/')}/templates/{snapshot_id}",
            headers=headers,
            timeout=30,
        )
        assert cleanup.status_code in (200, 204, 404), cleanup.text
