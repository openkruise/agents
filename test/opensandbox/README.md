# OpenSandbox SDK E2E

The six creation groups in `test_create.py` and thirteen lifecycle groups in
`test_sandbox.py` use the official Python SDK's generated Lifecycle client,
including request serialization and response parsing. Kubernetes reads verify
creation persistence and discover marked deliveries for cleanup. Cleanup uses
OpenSandbox DELETE; E2B deletion is a fallback that leaves the original failure
visible. The high-level `Sandbox.create()` endpoint and data-plane health steps
are outside this control-plane suite.

## E2B scenario mapping

All lifecycle test names correspond to `test/e2b/test_sandbox.py`. The suite adds
only routes implemented by PR1059 and does not copy E2B-only lifecycle features.

| Lifecycle group | Existing E2B scenario | OpenSandbox assertions |
| --- | --- | --- |
| `test_lifecycle` | Creation then `get_info` | Description ID, Running state, metadata and creation time |
| `test_list_by_metadata` | Unique metadata filter | Only the created instance is returned |
| `test_list_by_state` | Running/Paused filter | The live instance is present with the requested state |
| `test_timeout` | Deadline modification and actual expiry | Strictly extend expiration, stay live beyond the old deadline, then disappear while another instance stays live; no deadline shortening |
| `test_pause_kill` | Paused-instance cleanup | Pause acceptance, Paused observation and deletion |
| `test_pause_state` / `test_resume_state` | Final state in filtered lists | 202 acceptance followed by bounded detail/list polling |
| `test_is_running` | Live instance becomes unavailable after kill | Control-plane visibility before DELETE and eventual 404, without a runtime health route |
| `test_never_timeout` | Native never-timeout extension across pause/connect | No expiration on description before and after pause/resume; no E2B-only timeout-reset call |
| Four `test_concurrent_*` groups | Three same-direction or two pause plus two resume requests | One-shot 202/409 results and the accepted transition's final state, using E2B's existing request counts; if both state-changing requests conflict, retain the initial state |

No endpoint, command/file, code interpreter, automatic pause, wake-on-traffic,
public template/snapshot management, network-policy CRUD, quota or fault-injection
matrix is added. Pause/resume uses the existing pool/runtime configuration;
no memory, filesystem or CSI recovery backend is required by this suite.

## Run configuration

Set `OPENSANDBOX_API_URL` (default `http://127.0.0.1:18080/v1`) and
`OPENSANDBOX_API_KEY` (or `E2B_API_KEY`). Forward the Manager's HTTP port directly;
the default gateway routes E2B control requests but does not forward `/v1`.
Optional environment variables are `OPENSANDBOX_KUBE_CONTEXT`, `TEST_NAMESPACE`,
`OPENSANDBOX_IMAGE`, `OPENSANDBOX_POOL_REF`, and `OPENSANDBOX_EMPTY_POOL_REF`.
Pool defaults are the existing E2B `code-interpreter` and `code-interpreter-0` assets.
The configured image must be available to the cluster and contain `sleep`.
The Manager must include the PR1059 lifecycle routes and enable the adapter.

The control-plane change keeps these nineteen SDK scenarios and their fixtures.
Cluster execution additionally requires the OpenSandbox compatibility runtime,
its image and the `opensandbox-runtime` injection template. Cold creation
selects that template; Pool workloads must already contain the compatibility
container. Native initialization then initializes port 44772 with the same
runtime credential. An unconfigured deployment cannot run this suite.

The cluster workflow is delivered with the command/file runtime implementation,
where the complete image and injection prerequisites can be built together.
It retains the SDK `1.1.0` and `latest` tracks, daily latest execution, the two
existing Pool scenarios, and all nineteen control-plane tests. API, Manager
and shared runtime initialization unit tests remain independently runnable in
this control-plane change.

```sh
uv run --no-project --with-requirements test/opensandbox/requirements.txt python -m pytest test/opensandbox --collect-only -q
uv run --no-project --with-requirements test/opensandbox/requirements.txt python -m pytest test/opensandbox -v --junitxml=/tmp/opensandbox.xml
```

Local runs may point to isolated copies of existing pools to preserve their stock.
When loading the E2B `conftest_base` plugin against an existing cluster, set
`KUBECONFIG` to that cluster and `E2E_DEBUG=true` to bypass its namespace-wide
preflight cleanup. Check pool readiness before starting. OpenSandbox's own
marked-delivery cleanup remains enabled with this setting.
