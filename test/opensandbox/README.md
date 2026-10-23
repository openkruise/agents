# OpenSandbox SDK E2E

These six groups mirror only the creation assertions in `test/e2b/test_sandbox.py`.
Creation uses the official Python SDK's generated Lifecycle client, including
its request serialization and response parsing. Kubernetes reads verify persistence.
The fixture deletes only its marked deliveries through the existing E2B API.
The high-level `Sandbox.create()` also discovers endpoints and checks data-plane
readiness; those subsequent operations are outside this PR's create-only scope.

Set `OPENSANDBOX_API_URL` (default `http://127.0.0.1:18080/v1`) and
`OPENSANDBOX_API_KEY` (or `E2B_API_KEY`). Forward the Manager's HTTP port directly;
the default gateway routes E2B control requests but does not forward `/v1`.
Optional environment variables are `OPENSANDBOX_KUBE_CONTEXT`, `TEST_NAMESPACE`,
`OPENSANDBOX_IMAGE`, `OPENSANDBOX_POOL_REF`, and `OPENSANDBOX_EMPTY_POOL_REF`.
Pool defaults are the existing E2B `code-interpreter` and `code-interpreter-0` assets.
The configured image must be available to the cluster and contain `sleep`.

CI runs these six groups in the `E2E for OpenSandbox SDK` workflow
(`.github/workflows/e2e-opensandbox.yaml`) with separate checks, Kind clusters
and pools for SDK `1.1.0` (the proposal baseline) and `latest`.
PR, push and manual runs cover both tracks. Like E2B latest, the daily 02:00 UTC
run covers only `latest`, installed from PyPI without a version pin and with
`--upgrade`. Scheduled runs take effect after merging into the default branch.
It reuses the E2B provisioning runner and two pool manifests, selects only
`test/opensandbox`, and uploads JUnit, logs and the installed SDK version as
`opensandbox-create-results-<track>`. JUnit also records the actual SDK version.

```sh
uv run --no-project --with-requirements test/opensandbox/requirements.txt python -m pytest test/opensandbox --collect-only -q
uv run --no-project --with-requirements test/opensandbox/requirements.txt python -m pytest test/opensandbox -v --junitxml=/tmp/opensandbox-create.xml
```

Local runs may point to isolated copies of existing pools to preserve their stock.
Snapshot restoration, later lifecycle calls, and runtime commands are outside these groups.
