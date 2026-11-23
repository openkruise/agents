# OpenSandbox SDK E2E

The suite covers the implemented OpenSandbox control-plane interfaces: create,
list, describe, pause, resume, renew expiration, delete and endpoint discovery.
It uses the official Python SDK's generated Lifecycle client for request
serialization and response parsing, with HTTP requests through that client's
transport for authentication checks. Kubernetes reads inspect the real Sandbox,
Pod, SandboxSet and Checkpoint resources. Collection currently includes 43 test
instances, including parameterized authentication and resource-quota cases.

The high-level `Sandbox.create()` health handshake, command/file execution,
code interpreter, automatic pause, wake-on-traffic, public template/snapshot
management, signed endpoints and Lifecycle Server Proxy remain outside this
suite. Endpoint tests inspect addresses and headers without contacting an
execution service. Checkpoint tests inspect clone orchestration and inherited
configuration; content restoration requires its own backend validation.

## E2B scenario mapping

The reference suites are [E2B lifecycle](../e2b/test_sandbox.py),
[API keys](../e2b/test_apikey.py), [quota](../e2b/test_api_key_quota.py) and
[gateway routing](../e2b/test_gateway.py). The suite reuses the applicable
control-plane scenarios; current protocol and assertion differences are listed
below.

| OpenSandbox tests | E2B reference or shared behavior | Assertions |
| --- | --- | --- |
| [test_create.py](test_create.py): `test_lifecycle`, `test_pool_source_inherits_template`, `test_no_stock` | Creation from an image or native template, including an empty Pool | Real CR/Pod workload fields, owner, Ready condition, Pool inheritance and unchanged source Pool |
| [test_create.py](test_create.py): `test_checkpoint_source_inherits_template` | Native Checkpoint clone | Existing succeeded Checkpoint and same-name SandboxTemplate; restore source, owner/lock, workload, runtime, persistence and volume configuration; source template unchanged |
| [test_create.py](test_create.py): `test_sandbox_with_pod_ip`, `test_sandbox_with_labels` | Pod IP opt-in and `label:` metadata | Opt-in IP in create/describe/list and after pause/resume Ready, compared with the current CR; labels in create/describe responses, CR/template and live Pod |
| [test_create.py](test_create.py): `test_timeout`, `test_never_timeout` | Creation deadline and never-timeout extension | Returned deadline matches persisted shutdown time; never-timeout omits expiration |
| [test_sandbox.py](test_sandbox.py): `test_lifecycle`, `test_list_by_metadata`, `test_list_metadata_combines_filters`, `test_list_by_state`, `test_list_pagination` | Description and owner-scoped lists with metadata/state filters | ID, metadata and creation time; mixed Running/Paused candidates with single-state exclusion and combined-state inclusion, metadata filters, disjoint pages and pagination totals |
| [test_sandbox.py](test_sandbox.py): `test_timeout`, `test_shorten_expiration_and_repeat_deadline`, `test_never_timeout` | Deadline changes, actual expiration and never-timeout across pause/connect | Extend past the old deadline; shorten a finite deadline and accept the same deadline twice; observe deletion with a live control instance; preserve omitted expiration through pause/resume |
| [test_sandbox.py](test_sandbox.py): `test_pause_kill`, `test_pause_state`, `test_resume_state`, `test_pause_resume_preserves_expiration`, `test_delete_removes_description_and_list_entry` | Pause, reconnect, idempotent lifecycle intents and deletion | 202 acceptance, final CR conditions and detail/list state, unchanged expiration, deletion visibility and eventual DELETE 404 |
| [test_sandbox.py](test_sandbox.py): `test_concurrent_pause_running_sandbox`, `test_concurrent_resume_paused_sandbox`, `test_concurrent_pause_resume_on_paused_sandbox`, `test_concurrent_pause_resume_on_running_sandbox` | Three same-direction requests or two pause plus two resume requests | 202/409 results; mixed operations require final Running from a Paused start and final Paused from a Running start, without initial-state fallback |
| [test_endpoint.py](test_endpoint.py): `test_endpoint_discovery`, `test_endpoint_owner_isolation`, `test_endpoint_after_pause_resume_and_delete` | Owned-instance connection and gateway routing information | Direct/ingress address projection, execution-port aliases, runtime/routing credentials, valid port range, ownership, pause/resume and eventual removal |
| [test_auth.py](test_auth.py): `test_authentication_required`, `test_namespace_and_owner_isolation`, `test_auth_disabled_uses_anonymous_owner` | Shared API-key identity and auth-enabled/disabled deployments | Missing/invalid key rejection, same-namespace different-owner isolation, admin ownership boundary and anonymous ownership when auth is disabled |
| [test_quota.py](test_quota.py): `test_count_all_limit_blocks_then_delete_releases`, `test_count_all_limit_still_counts_paused_sandbox`, `test_running_pause_releases_and_resume_can_exceed_limit`, `test_running_resource_limits_pause_and_resume`, `test_resource_all_limit_uses_workload_limits`, `test_quota_unenforced_without_redis` | Native count/resource admission, pause/resume accounting and no-Redis behavior | 403 admission errors, delete/pause release for running count and CPU/memory limits, resume above a running limit, all-scope CPU/memory limits and unenforced quota without Redis |
| [test_quota.py](test_quota.py): `test_zero_count_limit_rejects_without_claiming_pool_candidates` | E2B hard-zero quota and rejected-claim resource checks | Redis-backed limit zero returns 403; no owned CR or retained owner/lock on previously ready Pool candidates |
| [test_quota.py](test_quota.py): `test_no_stock_path_stamps_owner_lock_and_counts_quota`, `test_checkpoint_clone_stamps_owner_lock_and_counts_quota`, `test_concurrent_creates_do_not_exceed_count_limit` | Empty-Pool and Checkpoint admission; ten concurrent creates with count limit three | Owner/lock and source identity; unchanged source Pool/template; three accepted and seven quota-rejected concurrent creates, matching the actual owned CR set; first-wave fail-open oversell permits one strict repeat only after measured recovery and cleanup |

Current protocol differences from E2B:

- Finite renewal with zero PauseTime accepts shorter, equal or longer future
  deadlines. A nonzero PauseTime retains strict extension of both deadlines;
  GET then prefers PauseTime while renew returns the new ShutdownTime. The
  adapter retains the shared timeout maximum and Running/Paused admission,
  whereas E2B `set_timeout` admits Running instances.
- Renewing an OpenSandbox never-timeout sandbox currently assigns a finite
  expiration. E2B `set_timeout` leaves a zero deadline unchanged. The existing
  never-timeout tests cover creation and pause/resume, without renewal.
- OpenSandbox resume preserves the existing deadline. E2B SDK `2.51.0` omits
  timeout by default; `/v2/connect` assigns a 300-second default when resuming a
  paused sandbox with a finite deadline. Running and never-timeout sandboxes
  follow different native branches, so preservation is not an equivalent
  assertion for the paused finite-deadline case.
- Pause/resume returns 202 after intent persistence. Tests poll description and
  CR `SandboxPaused`/`Ready` conditions; concurrent requests may return 409.
- Native SandboxSet creation uses `extensions.poolRef`; nonempty `templateId`
  creation is rejected by the current adapter. `snapshotId` selects a Checkpoint
  clone, whose workload and runtime configuration come from its source.
- OpenSandbox DELETE returns 404 after the backing delivery disappears.
  Terminating deliveries may remain deletable with 204 after describe hides
  them. Endpoint lookup can return 409 during that interval before its final 404.

Mixed pause/resume follows E2B's request policy: only Paused-start resume
retries an explicit `SandboxIsPausing` conflict, with at most 30 total attempts
and 29 two-second waits. Running-start resume stays one-shot. Requests are
submitted in E2B's order (pause before resume), without an additional start
barrier. Conflict response bodies are retained in pytest output. No sequential
repair operation is added to obtain the required final state.

Concurrent quota reads existing per-Pod admission and breaker counters. Pod
names, UIDs and restart counts must remain stable and counters cannot decrease.
Only first-wave oversell fully explained by fail-open deltas permits recovery:
within 180 seconds, wait 35 seconds when a breaker probe needs cooldown and
require new rejected decisions on every affected Pod. Successful probes join
cleanup. Before the single strict 3/7 repeat, require successful SDK cleanup
(with native fallback preserving any SDK error), CR removal, read-only Redis
count zero and at least three Ready, unowned and unlocked Pool candidates.
Missing evidence, undersell, unexpected errors or second-wave oversell fail.

The official SDK response models are unchanged. Opt-in Pod IP adds a string
metadata entry; callers should read required keys rather than compare an exact
key set. Shorter/equal renewal changes the initial adapter's 400 response to
200, so a caller relying on extend-only behavior must account for earlier
expiration. Concurrent renewal does not guarantee monotonically increasing
deadlines.

Native API-key CRUD and quota-configuration validation are setup or native API
tests, not OpenSandbox management-interface coverage. Runtime health, including
the E2B SDK's `is_running()`, uses a data-plane service; describe/delete visibility
checks do not cover that operation. Default CI Pools do not enable JWT or set
workload resources: conditional JWT-header and non-default Pool-resource
inheritance assertions therefore need separately configured sources to execute.

## Deployment and run configuration

Use a deployed Manager with the OpenSandbox adapter enabled. Forward its HTTP
listener directly for `/v1`; the default gateway's E2B control-plane routes do
not forward this prefix. The compatibility runtime image and
`opensandbox-runtime` injection template must exist. Cold creation selects that
template, and configured Pool workloads must already contain the compatibility
service. Native initialization also initializes port 44772 with the runtime
credential. The configured image must be accessible to the cluster and contain
`sleep`.

The Kubernetes identity must be able to read the tested namespace's Sandbox,
Pod and SandboxSet resources, plus Checkpoint/SandboxTemplate resources for the
clone case. The concurrent Redis case also needs Manager Pod proxy access for
metrics and Pod exec access for read-only `redis-cli HGET`. Check that both
configured Pools are ready before execution; the empty Pool must have zero
replicas and zero available replicas. Isolated copies of the existing E2B Pool
assets can preserve shared stock.

| Variable | Default and purpose |
| --- | --- |
| `OPENSANDBOX_API_URL` | `http://127.0.0.1:18080/v1`; Manager Lifecycle API base URL |
| `OPENSANDBOX_API_KEY` | Falls back to `E2B_API_KEY`; a valid shared key when authentication is enabled |
| `E2B_API_URL` | OpenSandbox URL without `/v1`, followed by `/kruise/api`; native key setup and fallback deletion endpoint |
| `OPENSANDBOX_KUBE_CONTEXT` | Current `kubectl` context; optional explicit context for all suite Kubernetes reads |
| `TEST_NAMESPACE` | `default`; namespace containing the tested Pools and deliveries |
| `OPENSANDBOX_POOL_REF` | `code-interpreter`; named native SandboxSet |
| `OPENSANDBOX_EMPTY_POOL_REF` | `code-interpreter-0`; zero-stock SandboxSet |
| `OPENSANDBOX_IMAGE` | Falls back to `CODE_INTERPRETER_IMG`, then `registry-ap-southeast-1.ack.aliyuncs.com/acs/code-interpreter:v1.6`; cold-creation image |
| `OPENSANDBOX_AUTH_ENABLED` | `true`; must match the Manager's actual `--e2b-enable-auth` setting |
| `OPENSANDBOX_ACCESS_MODE` | `ingress`; must match the Manager's OpenSandbox `ingress` or `direct` mode |
| `OPENSANDBOX_GATEWAY_URL` | Empty; set to the Manager's configured HTTP(S) ingress origin for ingress endpoint assertions |
| `OPENSANDBOX_QUOTA_PROFILE` | Falls back to `QUOTA_E2E_PROFILE`, then `none`; selects tests for an already-configured quota deployment |
| `OPENSANDBOX_KEY_SYNC_TIMEOUT` | `600` seconds; wait for newly created API keys to become visible through the shared key cache |
| `OPENSANDBOX_CHECKPOINT_REF` | Unset; existing Checkpoint CR name or `status.checkpointId` in `TEST_NAMESPACE` |

Authentication and quota isolation tests need an admin API key to prepare and
delete ordinary keys through the native `/api-keys` API. Those native requests
are fixture setup/cleanup; the tested Sandbox behavior uses OpenSandbox routes.
Ordinary keys share `TEST_NAMESPACE` while retaining distinct owners. These
tests check that the admin caller also respects Sandbox ownership. With actual
Manager auth disabled, set `OPENSANDBOX_AUTH_ENABLED=false`; the client uses an
anonymous placeholder when no key is configured.

### Profile and Checkpoint selection

Environment variables select assertions for the running deployment; they do
not configure the Manager, create Redis, switch access mode or install a restore
backend.

| Condition | Execution selection |
| --- | --- |
| Auth enabled | Missing/invalid-key, owner isolation and endpoint-owner tests run; `test_auth_disabled_uses_anonymous_owner` skips |
| Auth disabled | Authentication rejection and key-based isolation/quota tests skip; `test_auth_disabled_uses_anonymous_owner` runs |
| Quota profile `none` | All `test_quota.py` cases skip |
| Quota profile `redis`, `redis-clone` or `redis-fault` | Count and CPU/memory admission cases run against an already-working Redis quota deployment; the no-Redis case skips |
| Quota profile `no-redis` | Only `test_quota_unenforced_without_redis` runs from the quota module |
| Checkpoint ref unset | `test_checkpoint_source_inherits_template` and `test_checkpoint_clone_stamps_owner_lock_and_counts_quota` skip |
| Checkpoint ref set | The clone case requires a real succeeded Checkpoint, its same-name SandboxTemplate, compatible runtime configuration and functioning restore backend; invalid configured resources fail the test |

`redis-fault` selects the Redis-backed cases without injecting a Redis outage
or data loss. `redis-clone` does not supply a Checkpoint automatically; the clone
test still requires `OPENSANDBOX_CHECKPOINT_REF`. Memory, filesystem and CSI
content-recovery checks remain separate from its control-plane assertions.

## Running

### CI environment

Two version-specific workflows follow the existing E2B orchestration:

| Workflow entry | Triggers | Environment coverage inside the workflow |
| --- | --- | --- |
| [SDK 1.1.0](../../.github/workflows/e2e-opensandbox-1.1.0.yaml) | PR, master/release push and manual | One base `ingress-no-redis` job |
| [SDK latest](../../.github/workflows/e2e-opensandbox-latest.yaml) | Daily at 02:00 UTC and manual | Three environment profiles; no PR or push trigger |

The `latest` workflow expands its environment matrix internally. SDK version is
fixed by the workflow entry, rather than combined with profiles in a shared
version/profile matrix.

| Environment | Endpoint mode | Authentication | Quota deployment | Workflow coverage |
| --- | --- | --- | --- | --- |
| `ingress-no-redis` | ingress | enabled | no Redis | `1.1.0` base and `latest` |
| `direct-redis` | direct | enabled | Redis | `latest` |
| `direct-auth-disabled` | direct | disabled | no quota profile | `latest` |

Both workflows build the Manager, Controller and Gateway from the tested
checkout. The compatibility runtime is a fixed test dependency, built from
`zhuangzhewei09/agents@74b304e9e16516c1b9dc2fcc5d592b91e24cbf09` using that
commit's Dockerfile. Each workflow fetches a bare source archive and never
replaces the control-plane checkout or imports the dependency's test files.
`dependency-source.json` and `runtime-image.json` record the actual source and
image identity with the SDK version, collection, JUnit and execution logs.

The host [runner](../../hack/run-opensandbox-control-plane-e2e.sh) prepares the
actual Manager flags and creates isolated four-replica and zero-replica Pools
with the [manifest renderer](prepare_control_plane.py). Pool probes retain the
E2B workload's startup and readiness checks. Cold-create injection retains the
native env/volume/postStart initialization without the Pool's application
probe, because cold-create tests explicitly start `sleep infinity`.

Each job collects the same 43 instances from the five explicit control-plane
files. Profile-specific cases skip according to the table above; Checkpoint
cases require a real restore backend. The runner does not load E2B's pytest
plugin or run command, file, execution-health or code-interpreter tests.
After a successful suite, the runner also requires Manager and Gateway
container restart counts to remain zero and records `component-restarts.json`,
matching E2B's component-health acceptance boundary.

For local environment verification, first build/load the current component
images and pinned runtime into a dedicated Kind cluster, then select its
kubeconfig and run one profile:

```sh
KIND_CLUSTER_NAME=opensandbox-ci OPENSANDBOX_SDK_VERSION=1.1.0 \
  bash hack/run-opensandbox-control-plane-e2e.sh direct-redis
```

The current context must be `kind-$KIND_CLUSTER_NAME`. The runner refuses to
replace existing Manager/Controller/Gateway deployments without its ownership
label. It owns that test deployment, closes its port-forward on exit and leaves
the dedicated cluster available for diagnostics. Remove the Kind cluster after
local verification. Its Python venv is temporary; package installation needs
network access and uses PyYAML `6.0.3` for manifest rendering.

### Direct suite execution

Pinned SDK collection and execution use
[requirements.txt](requirements.txt), currently `opensandbox==1.1.0`:

```sh
uv run --no-project --with-requirements test/opensandbox/requirements.txt python -m pytest test/opensandbox --collect-only -q
uv run --no-project --with-requirements test/opensandbox/requirements.txt python -m pytest test/opensandbox -v --junitxml=/tmp/opensandbox.xml
```

The latest SDK can be selected separately:

```sh
uv run --no-project --with opensandbox --with pytest==9.0.3 --with requests python -m pytest test/opensandbox -v --junitxml=/tmp/opensandbox-latest.xml
```

The actual installed OpenSandbox SDK version is printed and recorded as a JUnit
suite property. Each create fixture marks its deliveries, records returned IDs
and discovers marked CRs even after failed/lost responses. Cleanup uses
OpenSandbox DELETE first, retains any failure, then uses native E2B deletion as
a fallback and waits for marked CRs to disappear. Sandbox cleanup precedes
fixture-created API-key deletion.

Standalone execution uses the suite's own fixtures. If the E2B `conftest_base`
plugin is explicitly loaded against an existing cluster, set `KUBECONFIG` to that
cluster and `E2E_DEBUG=true` to bypass its namespace-wide preflight cleanup.
OpenSandbox marked-delivery cleanup remains enabled.
