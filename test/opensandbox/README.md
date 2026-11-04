# OpenSandbox SDK E2E

The suites consume the control-plane implementation in
[PR #1059](https://github.com/openkruise/agents/pull/1059). Endpoint discovery,
API configuration, runtime selection and additional initialization follow the
[control-plane contract](../../docs/proposals/20260918-opensandbox-compat.md#endpoint-discovery-and-runtime-initialization).
The command/file increment supplies the compatibility runtime, image, deployment
fixtures and complete cluster environment.

| Suite | Test selection | Scenarios | Execution |
| --- | --- | --- | --- |
| Control plane | `test_create.py`, `test_sandbox.py` | Six creation and thirteen lifecycle scenarios | Host Python process through the Manager HTTP port; SDK `1.1.0` and `latest` tracks |
| Data plane | `test_commands.py`, `test_files.py` | Four command and two file scenarios | In-cluster Python Job through ingress and direct Pod IP; SDK `1.1.0` |
| Optional UUID profile | `test_auth.py` | One execution-authentication scenario | Data-plane Job when `OPENSANDBOX_AUTH_E2E=true` |

## Control-plane scenarios

The nineteen scenarios use the official Python SDK's generated Lifecycle client,
including request serialization and response parsing. Kubernetes reads verify
creation persistence and discover marked deliveries for cleanup. Cleanup uses
OpenSandbox DELETE; E2B deletion is a fallback that leaves the original failure
visible. High-level `Sandbox.create()` endpoint and runtime-health setup are
exercised by the data-plane fixture; Endpoint API behavior also has control-plane
Go coverage.

The six creation groups cover image cold creation, Pool Pod IP, empty-Pool
fallback, configured lifetime expiry, never-timeout, and metadata/labels.
Lifecycle test names correspond to `test/e2b/test_sandbox.py`:

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

The lifecycle scenarios do not add E2B-only automatic pause or wake-on-traffic,
public template/snapshot management, network-policy CRUD, quota or fault-injection
matrices. Pause/resume uses the existing Pool/runtime configuration; this suite
does not require a memory, filesystem or CSI recovery backend.

### Control-plane configuration and execution

Set `OPENSANDBOX_API_URL` (default `http://127.0.0.1:18080/v1`) and
`OPENSANDBOX_API_KEY` (or `E2B_API_KEY`). Forward the Manager HTTP port directly;
the default gateway routes E2B control requests but does not forward `/v1`.
Optional environment variables are `OPENSANDBOX_KUBE_CONTEXT`, `TEST_NAMESPACE`,
`OPENSANDBOX_IMAGE`, `OPENSANDBOX_POOL_REF`, and `OPENSANDBOX_EMPTY_POOL_REF`.
Pool defaults are the existing E2B `code-interpreter` and `code-interpreter-0`
assets. The configured image must be available to the cluster and contain
`sleep`.

The Manager must include the PR #1059 control-plane routes and enable the
adapter. Cluster execution additionally requires the compatibility runtime,
its image and the `opensandbox-runtime` injection template. Cold creation
selects that template; Pool workloads must already contain the compatibility
container. Native initialization delivers the runtime credential to port 44772.
API, Manager and shared runtime initialization unit tests remain independently
runnable without the command/file service deployment.

The cluster workflow
[`.github/workflows/e2e-opensandbox.yaml`](../../.github/workflows/e2e-opensandbox.yaml)
uses the complete runtime image and injection configuration. It retains SDK
`1.1.0` and `latest` tracks, daily latest execution and the two existing Pool
scenarios. Its host-side runner explicitly selects
`test/opensandbox/test_create.py` and `test/opensandbox/test_sandbox.py` through
two `--test-file` arguments, keeping the command/file tests in the data-plane
Job.

Select the same two files for collection or a local run:

```sh
uv run --no-project --with-requirements test/opensandbox/requirements.txt python -m pytest test/opensandbox/test_create.py test/opensandbox/test_sandbox.py --collect-only -q
uv run --no-project --with-requirements test/opensandbox/requirements.txt python -m pytest test/opensandbox/test_create.py test/opensandbox/test_sandbox.py -v --junitxml=/tmp/opensandbox-control-plane.xml
```

Local runs may point to isolated copies of existing pools to preserve their stock.
When loading the E2B `conftest_base` plugin against an existing cluster, set
`KUBECONFIG` to that cluster and `E2E_DEBUG=true` to bypass its namespace-wide
preflight cleanup. Check pool readiness before starting. OpenSandbox's own
marked-delivery cleanup remains enabled with this setting.

## Command and file scenarios

The data-plane suite pins `opensandbox==1.1.0` and uses the same Kind version,
component images and JUnit reporting as E2B E2E. Both ingress and direct-mode
deployments run the same scenarios from an in-cluster Python Job.

| Area | Selected tests | Scenario scope |
| --- | --- | --- |
| Foreground commands | `test_commands_run`, `test_commands_run_error_handling`, `test_commands_realtime_callbacks`, `test_commands_interrupt` | Four E2B-equivalent cases: basic execution, nonzero exits, live output and active foreground interruption; completed background state/logs remain outside scope |
| Files | `test_read_write_file`, `test_read_write_multifile` | Single-file and multi-file SDK round trips with actual content assertions |
| UUID authentication, optional | `test_execution_authentication` | Existing UUID gateway profile; reject invalid/missing execution tokens and perform a valid command |

Fixtures perform high-level SDK create, endpoint discovery for 44772 and 18080,
health checks and actual execution. They inspect the SDK's real request
destinations and headers. Signed file URLs remain unsupported.

### Run against a test cluster

Build and load Manager, Controller, gateway and runtime images using the existing
Make targets. Deploy the control plane and gateway as in E2B E2E, then run:

```sh
bash hack/run-opensandbox-e2e-test.sh both
# Or select one deployment:
bash hack/run-opensandbox-e2e-test.sh ingress
bash hack/run-opensandbox-e2e-test.sh direct
```

The [runner](../../hack/run-opensandbox-e2e-test.sh) installs test-only injection
templates, enables existing OpenSandbox routes and changes the Manager endpoint
strategy between runs. It uses the current kubectl context and the `default`
and `sandbox-system` namespaces. Use an isolated test cluster. Logs and JUnit XML
are written to `opensandbox-e2e-results/`.

The Job ConfigMap contains `test_commands.py`, `test_files.py`, `test_auth.py`
and their fixtures/configuration; it does not include the nineteen control-plane
tests. The service account can inspect and delete test Sandboxes and read Pod
logs in `default`; it has no Secret access.

For an existing UUID-authenticated gateway profile, set
`OPENSANDBOX_AUTH_E2E=true`. The script does not change gateway authentication.
The default is six command/file cases, with the authentication case selected
under that profile.

Failed tests collect status and compatibility-service logs before deleting
recorded instances through Kubernetes. Sandbox IDs are recorded as soon as
create returns, including when subsequent high-level SDK initialization fails.
Instance lifecycle routes are provided by the control-plane dependency;
code-interpreter protocols remain outside this suite.

## Runtime deployment prerequisites

Endpoint access-mode flags, origin validation, address/credential headers,
service-port mapping and additional-initialization orchestration are defined by
the [control-plane contract](../../docs/proposals/20260918-opensandbox-compat.md#endpoint-discovery-and-runtime-initialization).
The deployment must supply a reachable ingress/gateway or in-cluster Pod IP
path. SDK 1.1.0 uses its connection protocol for execution URLs; match that
protocol to the configured ingress origin. Direct tests use an internal HTTP
control-plane URL.

[`assets/runtime-injection.yaml`](assets/runtime-injection.yaml) supplies the
compatibility container, shared credential-state volume and native runtime
injection used by the cluster tests. Pool workloads and checkpoints require
compatible source configuration. The service calls envd at
`127.0.0.1:49983`; commands and files operate inside the workload container.
The envd startup wrapper and native E2B routes remain unchanged.

The compatibility service receives the native token through `/init`, verifies
it against the already protected envd, stores it in its own `0600` file and
validates `X-EXECD-ACCESS-TOKEN` on execution requests. It uses an HTTP native
runtime. External TLS terminates at ingress; no new runtime TLS/mTLS listener
is introduced. The control-plane contract defines the explicit failure
boundary for TLS-only additional initialization.

Logical execd 44772 and egress 18080 reach the compatibility listener. Egress
policy and Vault requests return HTTP 501 (`NOT_SUPPORTED`). Files retain
native envd path, owner and filesystem behavior; explicit upload modes/groups
use literal native command arguments and byte Range downloads reuse envd
handling. Numeric command UID/GID overrides, line-based download, independent
file management, background state/logs, PTY, code APIs, metrics, signed endpoints
and Lifecycle Server Proxy remain unsupported.
