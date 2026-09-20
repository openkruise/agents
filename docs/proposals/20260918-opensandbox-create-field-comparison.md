# OpenSandbox create: field-by-field server behavior

Companion to the [adapter proposal](./20260918-opensandbox-compat.md).
Status: static comparison and proposed behavior, pending design review and
runtime validation. Updated: 2026-09-27.

OpenSandbox OpenAPI/Server/SDK: `f59755922d92b4e98df805ed7fd4861bc2a38f21`.
Agents native E2B/Manager/Infra/runtime and current adapter:
`8771012c3e6e81550888931ab522e9d865f8ab00`.
Official Agents documentation and upstream `1015db48209922e18de347198980b98f88a9a188`
were rechecked on 2026-09-27 for the open pool/template, snapshot, startup,
pause and identity questions. The relevant native types and claim/clone sources
are unchanged; see the [verified capability table](./20260918-opensandbox-compat.md#verified-capabilities-and-remaining-choices).

The two server sides are **OpenSandbox Server** and **Agents native E2B plus
shared backend execution**. The current OpenSandbox adapter is a third column
of evidence, described separately in each field. A native capability does not
mean the adapter exposes it, and an adapter gap does not mean Agents lacks it.
Provider-specific behavior is explicitly qualified. All numbered F01–F16
entries remain in PR1; proposed behavior does not report implementation.

## Request routing and source combinations

Reference processing is lifecycle route -> service factory/composite service
-> provider -> workload/runtime. Agents native create looks up templateID as
a template first, then a checkpoint, and dispatches to ClaimSandbox or
CloneSandbox. The adapter currently only performs static alias -> claim.
[Reference route][os-route], [source model][os-source-schema],
[native dispatch][ag-source], [adapter][compat-create].

| Source | Required effective inputs | Other source/shape rules | Reference execution |
| --- | --- | --- | --- |
| Image | image.uri, nonempty entrypoint, non-null resourceLimits (empty map accepted) | No effective snapshotId/templateId; no ordinary poolRef | Pull/use requested image and create workload |
| Snapshot | nonblank snapshotId, non-null resourceLimits | No effective image/templateId/poolRef; entrypoint may be absent | Resolve Ready artifact; container path injects restore image and creates a new workload |
| Ordinary Pool | nonblank extensions.poolRef, without templateId | snapshotId and non-null lifecycle rejected; BatchSandbox also rejects platform/networkPolicy/volumes and credentialProxy.enabled=true | Allocate pool inventory; env/entrypoint feed a per-allocation task |
| Explicit template | nonblank templateId and timeout | image/effective snapshotId conflict; non-null workload overrides conflict; secureAccess=true conflicts, false is accepted by model | Resolve tenant Succeeded artifact; choose capacity using optional poolRef |

Template mode runs before ordinary Pool validation. Empty maps/arrays still
count as provided workload overrides there. Snapshot and template whitespace
normalization is not the same as allowing an empty image object: nested
validation applies first. `templateId=""` fails min length, whereas a
whitespace-only value is normalized by the reference model. Direct HTTP
fixtures must cover these distinctions without relying on SDK normalization.

## Field comparisons

### F01 — image

- **Wire/defaults:** optional object with required string `uri`; optional
  `auth` object with required string `username` and `password`. Absence/null
  means no image source. `{}` lacks uri; auth `{}` lacks credentials. Image
  mode requires a nonblank effective URI, entrypoint and resourceLimits.
- **OpenSandbox server:** Docker resolves/pulls the requested image and uses
  auth for the registry. BatchSandbox writes URI to the workload and prepares
  per-request pull secrets. OpenSandbox's agent-sandbox provider also supports
  request auth and merges template pull secrets with rollback on failure.
  In ordinary Pool mode a supplied image/auth does not replace the pooled
  image. Template mode prohibits image. [Docker values][os-docker-values],
  [BatchSandbox][os-batch-create], [agent-sandbox][os-agent-provider].
- **Agents native server:** templateID selects a SandboxSet; claim takes warm
  inventory or creates from it. The metadata image extension requests an
  InplaceUpdate of the selected sandbox; clone rejects InplaceUpdate. There
  is no matching username/password create field. [Claim][ag-claim],
  [clone][ag-clone], [extensions][ag-ext].
- **Current adapter:** exact static URI alias; missing alias returns 500;
  nested auth is unknown and returns 400. It does not establish that the
  claimed image equals the requested image. [Model][compat-model],
  [handler][compat-create].
- **Proposed mapping:** D01 ensures a tenant-scoped virtual template with
  compatible digest/config and authorized pull credentials, then claims or
  cold-creates. Credential reference/version affects reuse authorization;
  raw credentials never enter names, logs or ordinary metadata. Credentials
  and shared templates have separate ownership/refcounts.
- **Differences and validation:** image URI alone cannot select among pools.
  Observe actual digest and image content, private registry success/failure,
  warm/cold creation, mutable-tag refresh, concurrent ensure, cross-tenant
  isolation and secret cleanup. Pool-supplied image remains a non-override
  according to its source contract.

### F02 — snapshotId

- **Wire/defaults:** optional string; absent/null/blank is no effective
  snapshot source. A real ID excludes image/templateId and ordinary poolRef.
  The container snapshot path still requires resourceLimits and permits
  omitted entrypoint.
- **OpenSandbox server:** resolves a tenant-scoped record; unknown/foreign ID
  returns 404, non-Ready or missing recovery image returns 409. Container
  restore reads `restore_config.image`, defaults entrypoint to
  `["tail", "-f", "/dev/null"]`, and creates with the current request's
  configuration. Docker uses a committed image; K8s uses the snapshot
  workload's image artifact. This does not restore container memory/PIDs or
  imply mounted-volume recovery. FastSandbox artifacts route separately via
  their backend marker. [Resolver][os-snapshot], [backend dispatch][os-composite],
  [Docker snapshot][os-docker-snapshot], [K8s snapshot][os-k8s-snapshot].
- **Agents native server:** templateID reaches CloneSandbox only after template
  lookup misses. Infra reads Checkpoint plus its SandboxTemplate, creates a
  new sandbox with restore-from, waits ready and reinitializes using the old
  InitRuntimeRequest. PersistentContents can include podInfo/filesystem/memory;
  their guarantees differ. Native clone rejects resource/image InplaceUpdate
  and does not pass current envVars into reinit. [Clone][ag-clone],
  [restore][ag-clone-restore], [content types][ag-checkpoint].
- **Current adapter:** nonempty snapshotId returns 400; no restore execution.
- **Proposed mapping:** authorize the public source directly, bypassing native
  template-first ambiguity; require the selected recovery content profile;
  use CloneSandbox with explicit current startup overrides, cleared/replaced
  deadline and new delivery credentials. Preserve filesystem content while
  applying current configuration before user startup.
- **Differences and validation:** checkpoint naming is not equivalence. D03
  needs new override/startup capabilities; podInfo-only and memory-resuming
  artifacts cannot silently stand in for the container filesystem profile.
  Verify restored file hashes, current env/argv/resources, access isolation,
  unknown/unready/foreign IDs, old credential rejection and failed-clone cleanup.

### F03 — platform

- **Wire/defaults:** optional object; when provided, both `os` and `arch` are
  required strings. `{}` is invalid. Omitted/null uses runtime defaults,
  **not a fixed architecture guarantee**. Service validates linux/windows
  and amd64/arm64. [Model][os-schema], [validation][os-platform].
- **OpenSandbox server:** Docker resolves against image/daemon support. K8s
  Linux paths apply scheduling constraints and check conflicts. Windows
  follows a dedicated runtime profile, not simply a Windows nodeSelector;
  OpenSandbox's agent-sandbox provider rejects Windows. BatchSandbox Pool
  rejects platform; template mode fixes it during build.
  [Docker platform][os-docker-platform], [BatchSandbox][os-batch-create].
- **Agents native server:** scheduling OS/arch comes from template/Pod
  configuration. Native create has no platform field or corresponding
  metadata extension. [Model][ag-model], [extensions][ag-ext].
- **Current adapter:** decodes os/arch but ignores them, including required
  nested values and enums.
- **Proposed mapping:** resolve a compatible template/runtime profile and
  node constraints before allocation; persist the selected profile in the
  immutable configuration. Detect unsatisfiable image/platform combinations
  with the public error mapping, not a successful response with wrong OS/arch.
- **Differences and validation:** a Windows execution profile is a D07 design
  dependency, not demonstrated by CRD scheduling support. Verify actual OS,
  architecture, image compatibility, scheduling conflict, runtime startup and
  warm-inventory mismatch. Valid platform support is not closed by rejecting
  every request that needs an unimplemented profile.

### F04 — timeout

- **Wire/defaults:** optional integer seconds, minimum 60, deployment maximum.
  For ordinary image/snapshot/Pool, omitted/null means manual deletion; zero
  and 30–59 are invalid. Template mode requires a non-null timeout. It is not
  HTTP timeout, allocation wait timeout or SDK ready_timeout.
- **OpenSandbox server:** computes expiry during create preparation. Docker
  persists expiry for cleanup; BatchSandbox writes expireTime;
  agent-sandbox writes shutdownTime. The inspected K8s implementations handle
  no deadline; generic schema warnings do not prove those branches reject it.
  [Model][os-request], [context][os-context], [provider][os-batch-create].
- **Agents native server:** omitted/null/0 normalizes to 300, minimum 30;
  autoPause selects PauseTime versus ShutdownTime. `never-timeout` can clear
  the deadline. Wait budgets are separate. [Parsing][ag-parse],
  [modifier][ag-modifier].
- **Current adapter:** uses the native-style 300/30 defaults and writes
  ShutdownTime, so HTTP omission and 30-second input differ from OpenSandbox.
- **Proposed mapping:** retain field presence; normalize to no automatic
  expiry or a deadline based on a recorded server create timestamp; clear
  inherited pause/shutdown deadlines on clone. Return createdAt/expiresAt
  from the same lifecycle record. Allocation time consumes lifetime according
  to the selected contract; expiry during preparation must abort delivery.
- **Differences and validation:** do not import autoPause or SDK's default
  600 seconds. Test direct HTTP omitted/null/0/59/60/max/over-max, slow
  preparation, clone inheritance and actual termination/cleanup time.

### F05 — resourceLimits

- **Wire/defaults:** optional `map<string,string>`, required and non-null for
  image/snapshot; `{}` is accepted. CPU/memory are not individually mandatory.
  Typical keys are `cpu`, `memory`, `gpu`, plus runtime-supported resource names.
- **OpenSandbox server:** K8s converts `gpu` to `nvidia.com/gpu`, writes limits,
  rejects GPU `all`; some invalid GPU conversions are dropped. Docker consumes
  cpu/memory/gpu, validates provided positive values (GPU also allows `all`)
  and does not implement arbitrary K8s extended resources. Ordinary Pool uses
  its existing shape; template mode rejects a create override.
  [Resource conversion][os-k8s-resources], [container][os-container],
  [Docker][os-docker-values], [parsers][os-resource-parser].
- **Agents native server:** CPU/memory request/limit metadata extensions become
  InplaceUpdate for claim. Clone rejects that update; native create has no
  general request resource map. [Extensions][ag-ext], [claim][ag-claim],
  [clone][ag-clone].
- **Current adapter:** only cpu/memory keys decode and both are ignored;
  gpu/other keys return 400 due to the closed struct.
- **Proposed mapping:** preserve the open map and perform target-backend
  quantity validation, with explicit GPU conversion. Select/create compatible
  resource-shaped inventory or wait for a supported resize before releasing
  user code. Snapshot uses current limits through the new clone preparation
  path. Pool/template shapes remain governed by their source rules.
- **Differences and validation:** document target-backend validity separately
  from Docker/K8s quirks. Observe actual limits/cgroups/GPU allocation, empty
  map, extended keys, invalid values, stock mismatch and failure rollback.
  Applying an unsupported resource by dropping it is not completion.

### F06 — resourceRequests

- **Wire/defaults:** optional `map<string,string>`. OpenAPI describes limits
  fallback when omitted. Reference K8s also falls back for `{}`; a nonempty
  supplied map replaces the whole requests map, without per-key filling.
- **OpenSandbox server:** K8s resource construction is gated on nonempty
  converted limits, so `limits={}` with nonempty requests is currently
  discarded. Docker and ordinary Pool do not consume requests. Template
  create prohibits overrides. [Builder][os-container],
  [Pool branch][os-batch-create].
- **Agents native server:** CPU/memory request extensions populate claim
  InplaceUpdate; clone does not support it. No equivalent whole-map fallback
  or arbitrary resource map exists at native create. [Extensions][ag-ext].
- **Current adapter:** field is undeclared; even `{}`/null returns 400.
- **Proposed mapping:** D06 defaults absent/null/empty requests to effective
  limits and otherwise preserves the complete supplied map. Construct requests
  even when limits is empty; validate quantity and request/limit constraints
  using the target backend. Implement before user startup for image/snapshot.
- **Differences and validation:** requests-only behavior deliberately differs
  from the reference builder's discard and needs review. Verify resulting
  Pod resources and actual scheduling/QoS without assuming every equal map
  produces Guaranteed QoS for a multi-container Pod. Cover partial maps,
  requests-only, GPUs, unschedulable requests and no-stock behavior.

### F07 — env

- **Wire/defaults:** optional string map; absent/null/empty means no request
  variables. The Server model additionally accepts null values; OpenAPI says
  string values. Reserve `OPENSANDBOX_LIFECYCLE` for hook transport. Template
  create rejects any non-null env, including `{}`.
- **OpenSandbox server:** new-container variables are installed before user
  startup. Docker merges defaults then request values, skipping null values;
  K8s emits container env. Allowed `OPENSANDBOX_EGRESS_*` keys are separated
  for egress; without policy those keys are dropped with a warning. Pool
  uses each allocation's env in its task; snapshot uses the current request.
  [Context][os-context], [env split][os-env-helper], [Pool task][os-pool-task].
- **Agents native server:** claim sends envVars to POST /init unless skipped;
  this cannot alter already-running prewarmed processes. Clone reads old
  checkpoint init data; ReInit 401 is treated as already initialized.
  [Claim][ag-claim], [clone restore][ag-clone-restore], [init][ag-init].
- **Current adapter:** passes env to claim InitRuntime. Go string-map decode
  coerces null values to empty strings; no reserved/env-routing compatibility.
- **Proposed mapping:** D03 applies delivery env before preStart and entrypoint,
  and defines the same effective env for subsequent runtime commands. D05
  recommends the wire string-value contract with an explicit null-value
  validation case; retain documented reserved/egress-variable behavior.
- **Differences and validation:** default env precedence, missing versus empty
  string, literal `$`/quotes, null, reserved values and clone overrides need
  independent tests. Read env from the first entrypoint/hook process and a
  later command, not just the init request body. Clear old delivery values.

### F08 — metadata

- **Wire/defaults:** optional string map; absent/null/empty contributes no user
  metadata. Public examples include a human-readable name with spaces.
- **OpenSandbox server:** container providers validate metadata as labels,
  including key/value syntax, 63-character value limit and reserved
  `opensandbox.io/` prefix. They store workload/container labels used for
  filtering. This conflicts with some public string-map examples.
  [Validation][os-labels], [context][os-context].
- **Agents native server:** extracts reserved E2B extensions first, then
  validates ordinary keys and stores annotations. Annotation values do not
  have the same label limit; `name` does not rename the Sandbox.
  [Parsing][ag-parse], [modifier][ag-modifier].
- **Current adapter:** writes annotations, prohibits Agents reserved prefixes,
  and appends a sandbox-resource locator to the response. It does not provide
  the same create/list/filter/patch semantics as OpenSandbox.
- **Proposed mapping:** D05 stores public metadata in a dedicated encoded
  annotation value/namespace, preserving the public string map without
  enabling E2B metadata extensions or overwriting internal ownership keys.
  Get/list/filter/patch use the same decoding. Runtime/source metadata and
  diagnostics remain separate from user metadata.
- **Differences and validation:** recommendation accepts public string values
  (including spaces) rather than copying label restrictions; reserve internal
  namespaces and define size limits explicitly. Validate round-trip, filters,
  empty values, reserved keys, isolation and removal on recycle. Exact label
  compatibility is an alternative requiring explicit review, not an accidental
  side effect of the storage choice.

### F09 — entrypoint

- **Wire/defaults:** optional argv array with at least one string when set;
  explicit `[]` fails model validation. Image mode requires it. Snapshot/Pool
  omission uses `["tail", "-f", "/dev/null"]`. Template mode prohibits
  non-null overrides and uses its recorded entrypoint.
- **OpenSandbox server:** wraps the argv with bootstrap/execd control startup.
  Arguments remain separate; shell behavior requires an explicit shell.
  Every ordinary Pool allocation gets a startup task, including the default
  keep-alive. [Container][os-container], [Pool task][os-pool-task],
  [snapshot][os-snapshot].
- **Agents native server:** command/args come from the template or restored
  SandboxTemplate. Native create and InitRuntime have no equivalent requested
  entrypoint launch operation. [Model][ag-model], [init][ag-init].
- **Current adapter:** only echoes the request; does not execute it and accepts
  omitted image entrypoint more leniently than the source contract.
- **Proposed mapping:** startup supervisor launches exact argv once per
  delivery/startup generation, after configuration and preStart; preserves
  process exit, signal forwarding and cancellation behavior. A prewarmed
  daemon does not imply an already-running user entrypoint is acceptable.
- **Differences and validation:** check argv boundaries, literal shell
  characters, first-write side effects, PID/process relationships, retry
  duplication, entrypoint exit and container restart. The response's
  entrypoint field follows its schema but is not execution evidence.

### F10 — networkPolicy

- **Wire/defaults:** optional object with optional `defaultAction` and ordered
  `egress[]`; each rule requires `action` and nonempty `target`. Actions are
  allow/deny. Targets include domains/wildcards; the inspected executor also
  supports IP/CIDR. Omission/null and present `{}` are distinct.
- **OpenSandbox server:** absent/null creates no request egress sidecar.
  `{}` creates one with an empty list and default deny. OpenAPI prose says
  empty allows all; the executor contradicts it. Domain rules use first-match
  order; dns and dns+nft have different enforcement paths. Ordinary Pool
  rejects policy, while template-plus-pool accepts policy through its own
  mapping. [Sidecar][os-egress], [executor][os-policy],
  [template mapping][os-template-map].
- **Agents native server:** internet access defaults true. allowOut accepts
  IP/CIDR/concrete FQDN but not wildcards; denyOut accepts IP/CIDR. Separate L7
  security rules exist. These grouped policies do not directly implement
  ordered OpenSandbox rules. Network creation follows claim/clone and failure
  kills the allocated sandbox. [Network][ag-network], [security][ag-security],
  [native create][ag-create].
- **Current adapter:** decodes and ignores policy.
- **Proposed mapping:** D04 recommends no request policy for omission/null and
  deny for present empty policy; document the public-prose conflict. Add an
  execution path preserving order, wildcards and destination semantics. Stage
  enforcement before user startup and return success only after enforcement
  is ready. Do not translate arbitrary ordered rules into two unordered lists.
- **Differences and validation:** executor choice is a D07 dependency. Test
  `{}`, explicit allow/deny, conflicting rules and order, DNS and direct IP,
  wildcard boundaries, first outbound request, template-plus-pool and removal
  on reuse. A stored policy object alone is not proof of enforcement.

### F11 — secureAccess

- **Wire/defaults:** boolean default false; explicit null is not the boolean
  contract. Template model rejects true but accepts false, despite its prose
  broadly saying secureAccess is prohibited.
- **OpenSandbox server:** true is implemented for Kubernetes ingress gateway,
  rejected by Docker. It generates a secure-access token in workload metadata
  and returns required endpoint headers; the gateway validates requests. It
  is distinct from execd authentication and signed URLs.
  [Guards][os-k8s-guards], [context][os-context], [endpoint][os-endpoint].
- **Agents native server:** claim normally generates runtime access credentials;
  clone can inherit checkpoint init credentials. Traffic token/gateway
  mechanisms also exist. Native create has no equivalent public boolean and
  credential response shape. [Claim][ag-claim], [clone][ag-clone],
  [response][ag-response].
- **Current adapter:** accepts then ignores it; no endpoint route.
- **Proposed mapping:** create a per-delivery endpoint protection policy and
  token, expose it through endpoint headers, and enforce at the gateway.
  Keep runtime credentials separate. False removes this additional protection
  only; source/API authentication still applies. Rotate on clone/reallocation
  and revoke on deletion.
- **Differences and validation:** D08 covers tenant/Key ownership and how
  unsigned or unprotected business endpoints fit Agents routing. Test false,
  true, null, valid/missing/wrong headers, execd versus business ports,
  cross-sandbox token use, expiry/revocation and recycled instances.

### F12 — credentialProxy

- **Wire/defaults:** optional object; `enabled` boolean defaults false;
  absent/null/`{}`/enabled=false do not enable MITM. Template create rejects
  any non-null object, including enabled=false. Enabled=true needs a policy;
  ordinary Pool rejects true.
- **OpenSandbox server:** requires dns+nft egress and forbids conflicting
  SSL_INSECURE settings; enables transparent MITM/credential injection.
  defaultAction other than deny currently produces a warning/deprecation
  rather than an unconditional rejection. This field carries no secret or
  injection rule; credential configuration is a subsequent API operation.
  [Validation][os-credential], [egress startup][os-egress].
- **Agents native server:** L7 header transformations can insert plaintext
  headers, but inline security rules reject tokenTransformation. No equivalent
  create-time Vault proxy or secret lifecycle is wired. [Security][ag-security].
- **Current adapter:** undeclared; even enabled=false returns 400.
- **Proposed mapping:** D07 adds an actual sandbox-scoped credential binding
  store/API and outbound proxy path with authenticated configuration, target
  matching, HTTPS interception/trust, secret isolation and revocation. Enable
  the proxy and trust material before startup; preserve disabled semantics.
  The concrete proxy/provider choice remains a design gate.
- **Differences and validation:** image auth, env, runtime tokens and plain
  header rewriting are different capabilities. Verify real external requests
  receive the intended credential, mismatched destinations receive none,
  HTTPS clients trust the intended CA, direct bypass is controlled and cleanup
  removes bindings. Include invalid combinations and initialization failure.

### F13 — lifecycle

- **Wire/defaults:** optional object. `preStart` is one optional object, not an
  array: required nonempty `command` argv, optional `timeoutSeconds` 1–10800
  (default 60 at execution). `periodic` is an optional array; each item has
  unique nonblank `name`, nonblank `schedule`, nonempty command and optional
  timeoutSeconds 1–300 (default 60). Names/schedules are trimmed; schedule
  validation supports five-field cron and supported descriptors, with `@every`
  requiring whole seconds. `{}` has no hooks; template rejects any non-null
  lifecycle, and ordinary Pool also rejects non-null lifecycle.
  [Models][os-hooks-schema], [runtime config][os-hook-config].
- **OpenSandbox server/runtime:** K8s transports hooks in reserved env. Normal
  startup listens on HTTP, runs preStart, starts periodic, then launches the
  entrypoint. Runtime-init mode waits for `/internal/init` to install startup
  settings. preStart failure blocks user entrypoint. Same-name periodic runs
  do not overlap; ordinary failure logs and continues, but a run that remains
  stuck after cancellation disables future execution. Docker rejects non-null
  lifecycle in this baseline. [Startup][os-execd-main], [periodic][os-periodic],
  [Docker guard][os-docker-create].
- **Agents native server:** autoPause/autoResume concern lifecycle deadlines,
  not these command hooks. Existing controller/template hooks have different
  execution actors and timing. Native create does not expose this command
  structure. [Native model][ag-model].
- **Current adapter:** raw nested JSON is decoded but not semantically
  validated or executed.
- **Proposed mapping:** D03 adds the runtime startup barrier/scheduler. Inject
  config and dependencies before hooks, execute in the workload environment,
  keep per-startup generation and cancellation state, and expose real startup
  outcome. Preserve restart behavior; retries alone do not duplicate hooks.
- **Differences and validation:** retain lifecycle as required create scope;
  initContainer/livenessProbe is not sufficient. Exercise ordering, working
  environment, success/failure/timeouts, bad/duplicate schedules, nonoverlap,
  uncancellable tasks, restart and no prewarm side effects. HTTP listening
  must not be equated with preStart or application success.

### F14 — volumes

- **Wire/defaults:** optional array; absent/null/empty means no mounts. Each
  element requires a unique local `name` (DNS-style, max 63), absolute container
  `mountPath`, exactly one host/pvc/ossfs object; `readOnly` defaults false and
  optional `subPath` is relative without traversal. Public local name is not
  a global PV identifier. [Volume model][os-volume-schema],
  [validation][os-volumes-valid].
- **Nested source fields:** `host.path` is an absolute host path subject to an
  allowlist. `pvc.claimName` is required (DNS-style, max 253);
  `createIfNotExists=true`, `deleteOnSandboxTermination=false` by default;
  optional `storageClass`, `storage`, `accessModes` are provisioning hints
  (cluster default class, server default capacity, ReadWriteOnce default).
  Existing sources ignore new provisioning hints. `ossfs.bucket` and endpoint
  are required; version is `1.0` or `2.0`, default `2.0`; options is an optional
  string array. accessKeyId/accessKeySecret look optional in the model but its
  validator requires both for inline credentials. subPath denotes a bucket
  prefix for OSSFS.
- **OpenSandbox server:** K8s uses same-namespace PVCs, prepares absent PVCs
  before workload creation and tracks ownership; current K8s volume helper
  supports host/PVC, not OSSFS. Docker maps PVC to a named volume and ignores
  K8s provisioning hints; OSSFS uses a host FUSE mount and bind mount. Ordinary
  Pool rejects volumes; template mode rejects non-null overrides. Cleanup
  protects pre-existing volumes and does not delete a PVC still used by a live
  workload. [K8s][os-k8s-volumes], [Docker][os-docker-volumes],
  [create/rollback][os-k8s-create], [cleanup][os-return].
- **Agents native server:** simple volumeMounts `name/path` convert to CSI PV
  source and mount path. Full CSI config adds mountID/pvName/mountPath/subPath/
  readOnly/attributes. The control/runtime path mounts an identified source;
  it is not a general host/OSSFS/PVC provisioning API. Read-only constraints
  also derive from PV access modes. [CSI types][ag-csi],
  [extensions][ag-ext], [validation][ag-volume-validation].
- **Current adapter:** volumes is undeclared; all occurrences return 400.
- **Proposed mapping:** D07 resolves backend identity and permissions, prepares
  host or CSI-backed storage, provisions missing PVCs and protected OSSFS
  access as needed, then mounts before user startup. Return an ownership
  receipt for cleanup. Reuse existing CSI where applicable; add distinct
  provision/unmount/release capabilities rather than overloading local name.
- **Differences and validation:** all public source variants remain mapped
  work; K8s reference's missing OSSFS is not evidence that Agents implemented
  it. Test each subfield's effect, content visibility, readonly/subPath escape,
  missing source with auto-create off, duplicate names/paths, malformed
  credentials, failed preparation, shared/pre-existing volume survival and
  owned-resource cleanup on failure/termination.

### F15 — extensions

- **Wire/defaults:** optional `map<string,string>`, not a nested pool object.
  Omitted/null/empty carries no extensions. Known keys have different
  semantics; arbitrary keys are not guaranteed to be persisted or executed.
- **OpenSandbox server — poolRef:** trimmed nonblank value selects ordinary
  Pool if templateId is absent. BatchSandbox creates an allocation and always
  a taskTemplate; env/entrypoint are delivery-specific, image/resources stay
  pool-defined. `"*"` selects a pool automatically; named pool existence is
  checked. Capacity has a separate wait and can return 429 + Retry-After.
  Docker rejects poolRef, and OpenSandbox agent-sandbox lacks the allocation
  consumer. Template mode uses poolRef as capacity and allows networkPolicy;
  omitted pool uses the FastSandbox default. Its wildcard behavior is not
  established by ordinary Pool's `"*"` support. [Pool][os-pool],
  [task][os-pool-task], [wait][os-wait], [template mapping][os-template-map].
- **OpenSandbox server — other keys:** `access.renew.extend.seconds` is an
  integer string in 300–86400, persisted for access-renewal integration;
  actual renewal requires that integration. `bootstrap.execd.isolation=enable`
  changes bwrap startup/permissions/mount requirements. Keys prefixed with
  `opensandbox.extensions.` are encoded into labels/annotations and decoded
  on reads; other keys have provider-specific/transient handling.
  [Validation][os-extension-validation], [codec][os-extension-codec],
  [startup][os-container].
- **Agents native server:** named ordinary poolRef maps directly to a
  namespaced SandboxSet through ClaimSandboxOptions.Template. The set has one
  target inline template or templateRef, with transient old/new revisions
  during rollout; claim-time image/resource overrides are also possible.
  `replicas` is unused inventory, not an active-sandbox capacity ceiling.
  Native E2B no-stock creation defaults true. autoResume is
  wake-on-access, not renewal. Native extensions are parsed from reserved
  metadata keys; its internal Extensions field is `json:"-"`, so there is no
  public top-level map with equivalent behavior. [Extensions][ag-ext],
  [model][ag-model], [claim options][ag-options], [pool manual][ag-pool-manual],
  [SandboxSet types][ag-set-types].
- **Current adapter:** field undeclared, including `{}`/null; no pool source.
  It also forbids the E2B metadata prefix, so that is not a bypass.
- **Proposed mapping:** decode per key; ordinary poolRef resolves to the
  authorized set, while template-plus-pool resolves both constraints into
  the shared claim plan in D02. This does not require a new pool type for
  ordinary poolRef. Implement access-renewal integration and
  runtime isolation behavior. Preserve documented opaque persistence without
  promoting arbitrary extensions to backend configuration. Native E2B defaults
  are not inherited wholesale.
- **Differences and validation:** test named/automatic/exhausted pools,
  disallowed combinations, per-allocation env/entrypoint, integer boundaries,
  renewal under actual traffic, isolation effects and prefix round-trip.
  Cold fallback must honor the source's explicit capacity policy.

### F16 — templateId

- **Wire/defaults:** optional string, minimum length one; absent/null has no
  template mode. Nonblank value takes source priority and requires timeout.
  It allows metadata/networkPolicy and optional extensions.poolRef.
  image/effective snapshot conflict; non-null entrypoint/env/limits/requests/
  volumes/platform/credentialProxy/lifecycle conflict (even empty values).
  secureAccess=true conflicts, false passes the model. [Source validation][os-template-source].
- **OpenSandbox server:** resolves the tenant-private catalog; unknown,
  cross-tenant or not-Succeeded template returns 404. Build produces immutable
  golden-image artifacts with runtime wiring, entrypoint/env and workload
  shape. Server create resolves the recorded artifact and its entrypoint,
  then sends identity/TTL/metadata/network policy and capacity to FastSandbox.
  [Catalog][os-template-resolve], [build semantics][os-template-doc],
  [mapping][os-template-map].
- **Capacity semantics:** FastSandbox SandboxPool prewarms Fastlet Pods;
  each hosts multiple sandboxes. Runtime and per-sandbox resource shape constrain
  compatibility. warmImages caches artifacts; it does not mean prebuilt user
  sandboxes, and one pool can serve multiple compatible business images.
  [Scheduling][os-template-pool].
- **Agents native server:** ClaimSandbox.Template selects one SandboxSet work
  pool; there is no independent public-template-plus-capacity selector.
  SandboxTemplate and SandboxSet.templateRef already represent workload
  configuration, but do not expose the public tenant build/catalog flow or
  build status. CloneSandbox.CheckPointID selects a recovery artifact. Claim
  can fall back to an older revision, which does not guarantee the resolved
  template artifact. The effective revision includes set-owned runtime and
  persistence settings, not only the referenced Pod template.
  [Options][ag-options], [revision selection][ag-template-selection],
  [native template][ag-template-type], [revision construction][ag-set-revision].
- **Current adapter:** undeclared; templateId returns 400.
- **Proposed mapping:** D02 reuses native template/pool/checkpoint primitives
  and adds missing public build state/artifact lookup. Resolve templateId and
  optional poolRef into one allocation plan. Compatible stock uses claim with
  exact artifact/config checks; saved build state may require clone. Image
  replacement alone does not reproduce arbitrary build files or runtime
  layout. Public fields retain both constraints; neither is silently dropped.
- **Differences and validation:** one FastSandbox pool can serve different
  compatible artifacts; one Agents pool has a single target workload template.
  The general multi-artifact case needs a concrete materialization/allocation
  design before choosing catalog storage or cross-set capacity accounting.
  A new capacity controller/ConfigMap catalog is not established as necessary.
  Test build-produced files,
  pending/failed/deleted/foreign templates, immutable revision pinning,
  template-plus-pool compatibility/exhaustion, old inventory, concurrent
  update/delete/create and Manager restart. Existing sandboxes keep their
  admitted artifact after catalog update/deletion. Run create_from_template
  through actual endpoint/health and failure cleanup.

## Response, errors and initialization observations

| Item | OpenSandbox server / client contract | Agents native / current adapter | Proposed acceptance |
| --- | --- | --- | --- |
| Create response | 202; id/status/createdAt/entrypoint required; source-dependent readiness | Native 201; adapter 202 after claim, echoes entrypoint | Required keys, correct field types and state derived from actual operation |
| createdAt / expiresAt | Delivery lifecycle timestamps; optional expiry for manual cleanup | Adapter prefers claim annotation, then CR creation time, then now; reads ShutdownTime, overriding it with nonzero PauseTime without comparing timestamps | New delivery time and selected expiry policy, no recycled-object timestamp or inherited pause deadline |
| metadata | User map and filterable semantics | Annotations and additional internal resource locator | Consistent isolated public map across create/get/list/patch |
| status | Pending/Running/Pausing/Paused/Resuming/Stopping/Terminated with reason/message | Native states need translation; claimed-but-not-ready needs explicit treatment | No state mapping used as proof of runtime health |
| Error envelope | HTTP status plus OpenSandbox error detail/code; validation and provider errors vary | Adapter currently uses shared web.ApiError and E2B-like classification | Fixed fixtures for malformed fields, source authorization/readiness, quota/capacity, timeout and cleanup failures |
| Response versus readiness | K8s service waits Running/Allocated; FastSandbox may return durable Pending; SDK obtains endpoints and checks health afterward | Claim/clone include backend readiness and init/mount stages; adapter lacks complete subsequent routes | Document each gate, exercise both service readiness and real SDK health |

The complete create response has eight top-level fields. `id`, `status`,
`createdAt` and `entrypoint` are required by OpenAPI; `metadata`, `extensions`,
`platform` and `expiresAt` are optional. `status` contains required `state`
and optional `reason`, `message`, `lastTransitionAt`. The adapter declares
state/reason/message, but only sets state/reason; it lacks lastTransitionAt,
extensions and platform. The reference Server response model makes entrypoint
optional, and routes exclude null, so an omitted Pool entrypoint may be absent
on the wire despite the schema requirement. Proposed responses retain all
schema-required keys and explicitly test this discrepancy. Image/resourceLimits
and updatedAt are not create-response fields. [Response contract][os-spec],
[Server model][os-schema], [adapter conversion][compat-create].

The error target is not a single status for every failure: missing/foreign
snapshot or template is 404, unready container snapshot is 409, bounded pool
capacity exhaustion may be 429 with Retry-After, provisioning wait timeout is
504, and namespace quota has 403 / KUBERNETES::QUOTA_EXCEEDED semantics in the
reference contract. Exact input-validation status/body must be checked against
route middleware and OpenAPI rather than inferred from a model exception.
[Wait/error handling][os-wait], [quota contract][os-quota-spec].

## SDK behavior is a separate layer

- Ordinary `Sandbox.create()` defaults timeout to 600 seconds, resourceLimits
  to cpu=1/memory=2Gi, and entrypoint to the keep-alive command. It sends
  secureAccess=false. `timeout=None` explicitly sends null. These are client
  choices, not direct HTTP defaults.
- The ordinary high-level SDK requires image or snapshot_id; a server-valid
  pool-only request needs the lower-level HTTP contract test. Empty maps may
  be omitted by conversion, so direct HTTP tests are also necessary.
- `create_from_template(template_id, timeout=...)` is a separate entrypoint;
  it avoids normal image/resource/entrypoint defaults and needs real template
  management/build support.
- ready_timeout (30 seconds by default), connection_config, health_check,
  health_check_polling_interval (200ms) and skip_health_check are client-side
  options. Skipping health still performs endpoint acquisition.
- Endpoint acquisition and health share the ready budget. Template mode uses
  its execd endpoint and control-plane policy path; ordinary mode discovers
  execd/egress endpoints and uses returned origin information. Failure cleanup
  requires the actual delete route.

[SDK create][sdk-create], [converter][sdk-converter],
[template/initialization flow][sdk-template], [connection][sdk-connection].

## Acceptance evidence

Every field above needs request/response evidence plus its stated observable
execution effect. Record exact SHAs, provider/runtime, image/artifact digest,
pool/template revisions and gateway/storage settings. Keep unit, static and
integration results separate. Existing aliases, accepted JSON, echoed fields,
written CRs, fake endpoints and skipped health do not establish behavior.

[ag-checkpoint]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/api/v1alpha1/checkpoint_types.go#L114-L175
[ag-claim]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/e2b/create.go#L122-L219
[ag-clone]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/e2b/create.go#L221-L312
[ag-clone-restore]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/sandbox-manager/infra/sandboxcr/clone.go#L267-L497
[ag-create]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/e2b/create.go
[ag-csi]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/api/v1alpha1/mount_types.go
[ag-ext]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/e2b/models/extensions.go
[ag-init]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/utils/runtime/init.go#L39-L94
[ag-model]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/e2b/models/sandbox.go#L34-L153
[ag-modifier]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/e2b/create.go#L395-L452
[ag-network]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/e2b/network.go#L37-L99
[ag-options]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/sandbox-manager/infra/types.go#L45-L137
[ag-parse]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/e2b/create.go#L314-L393
[ag-response]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/e2b/sandbox.go
[ag-security]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/e2b/securityrules.go
[ag-source]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/e2b/create.go#L82-L120
[ag-template-selection]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/sandbox-manager/infra/sandboxcr/claim.go#L664-L708
[ag-volume-validation]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/e2b/models/validation.go
[compat-create]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/opensandbox/create.go#L74-L349
[compat-model]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/opensandbox/models.go#L57-L128
[os-agent-provider]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/agent_sandbox_provider.py#L126-L255
[os-batch-create]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/batchsandbox_provider.py#L127-L342
[os-composite]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/composite_service.py#L57-L67
[os-container]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/provider_common.py#L116-L240
[os-context]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/create_helpers.py#L54-L142
[os-credential]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/validators.py#L553-L585
[os-docker-create]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/docker/docker_service.py#L613-L680
[os-docker-platform]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/docker/container_ops.py#L79-L220
[os-docker-snapshot]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/docker/snapshot_runtime.py#L150-L189
[os-docker-values]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/docker/container_ops.py#L283-L360
[os-docker-volumes]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/docker/volumes.py
[os-egress]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/egress_helper.py#L83-L159
[os-endpoint]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/endpoint_resolver.py
[os-env-helper]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/helpers.py#L254-L280
[os-execd-main]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/components/execd/main.go#L216-L317
[os-extension-codec]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/extensions/codec.py#L27-L90
[os-extension-validation]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/extensions/validation.py#L24-L106
[os-hook-config]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/components/execd/pkg/lifecycle/config.go#L33-L218
[os-hooks-schema]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/schema.py#L142-L207
[os-k8s-create]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/kubernetes_service.py#L919-L1087
[os-k8s-guards]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/kubernetes_service.py#L495-L577
[os-k8s-resources]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/provider_common.py#L46-L113
[os-k8s-snapshot]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/snapshot_runtime.py
[os-k8s-volumes]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/volume_helper.py#L40-L125
[os-labels]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/validators.py#L94-L130
[os-periodic]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/components/execd/pkg/lifecycle/periodic.go#L35-L97
[os-platform]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/validators.py#L221-L265
[os-policy]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/components/egress/pkg/policy/policy.go#L39-L165
[os-pool]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/batchsandbox_provider.py#L375-L425
[os-pool-task]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/batchsandbox_provider.py#L513-L549
[os-quota-spec]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/specs/sandbox-lifecycle.yml#L468-L484
[os-request]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/schema.py#L452-L649
[os-resource-parser]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/helpers.py#L70-L150
[os-return]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/kubernetes_service.py#L1087-L1215
[os-route]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/lifecycle.py#L59-L120
[os-schema]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/schema.py#L453-L652
[os-snapshot]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/snapshot_restore.py#L29-L105
[os-source-schema]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/schema.py#L576-L649
[os-template-doc]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/docs/architecture/fast-sandbox/templates.md
[os-template-map]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/fast_sandbox/create_mapping.py#L155-L207
[os-template-pool]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/docs/architecture/fast-sandbox/scheduling.md#L7-L30
[os-template-resolve]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/templates/template_service.py#L324-L344
[os-template-source]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/schema.py#L563-L649
[os-volume-schema]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/schema.py#L218-L419
[os-volumes-valid]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/validators.py#L622-L706
[os-wait]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/kubernetes_service.py#L333-L493
[sdk-connection]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/sdks/sandbox/python/src/opensandbox/config/connection.py
[sdk-converter]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/sdks/sandbox/python/src/opensandbox/adapters/converter/sandbox_model_converter.py
[sdk-create]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/sdks/sandbox/python/src/opensandbox/sandbox.py
[sdk-template]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/sdks/sandbox/python/src/opensandbox/sandbox.py#L638-L822
[os-spec]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/specs/sandbox-lifecycle.yml

[ag-pool-manual]: https://openkruise.io/kruiseagents/user-manuals/warmpool-management
[ag-set-revision]: https://github.com/openkruise/agents/blob/1015db48209922e18de347198980b98f88a9a188/pkg/controller/sandboxset/revision.go#L36-L73
[ag-set-types]: https://github.com/openkruise/agents/blob/1015db48209922e18de347198980b98f88a9a188/api/v1alpha1/sandboxset_types.go
[ag-template-type]: https://github.com/openkruise/agents/blob/1015db48209922e18de347198980b98f88a9a188/api/v1alpha1/sandboxtemplate_types.go
