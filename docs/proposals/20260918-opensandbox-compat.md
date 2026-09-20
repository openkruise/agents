---
title: OpenSandbox-Compatible API Adapter
authors:
  - "@zhuangzhewei09"
reviewers:
  - "@TBD"
creation-date: 2026-09-18
last-updated: 2026-09-28
status: provisional
see-also:
  - "https://github.com/openkruise/agents/issues/690"
---

# OpenSandbox-Compatible API Adapter

## Table of contents

- [Summary](#summary)
- [Motivation](#motivation)
- [Compatibility baseline](#compatibility-baseline)
  - [Validation boundary](#validation-boundary)
- [Architecture](#architecture)
- [Creation sources](#creation-sources)
- [Initialization and runtime](#initialization-and-runtime)
- [Lifecycle and API behavior](#lifecycle-and-api-behavior)
- [Identity and credentials](#identity-and-credentials)
  - [Control-plane identity and authorization](#control-plane-identity-and-authorization)
  - [Control-plane compatibility boundaries](#control-plane-compatibility-boundaries)
  - [Credential responsibilities](#credential-responsibilities)
- [Failure handling and compatibility](#failure-handling-and-compatibility)
- [Create field contracts](#create-field-contracts)
  - [Request routing and source combinations](#request-routing-and-source-combinations)
  - [Field comparisons (F01–F16)](#field-comparisons)
  - [Response, errors and readiness](#response-errors-and-readiness)
  - [SDK behavior is a separate layer](#sdk-behavior-is-a-separate-layer)
- [Test plan](#test-plan)
  - [Control-plane identity tests](#control-plane-identity-tests)

## Summary

Add OpenSandbox-compatible APIs alongside the E2B APIs in sandbox-manager.
Both API layers share the same SandboxManager and API-key storage. The adapter
preserves OpenSandbox request and workload semantics while using Agents
lifecycle, allocation, runtime and validation behavior. Lifecycle Server
validation parity is outside the compatibility target.

Image creation uses virtual templates backed by SandboxSet, with cold creation
when compatible warm inventory is unavailable. Snapshot creation maps recovery
sources to Checkpoint and creation to CloneSandbox. Ordinary pools select work
instances; explicit templates identify workload artifacts and retain a separate
pool-capacity constraint when poolRef is supplied.

The architecture follows [issue #690][issue]. The
[field contract](#create-field-contracts) covers the
16 create fields and their nested values. The same design applies across the
implementation PRs for creation, lifecycle and runtime integration.

## Motivation

Agents exposes an E2B-compatible API, while OpenSandbox clients use different
request models, creation sources, lifecycle states and runtime protocols.
Adding an API adapter lets those clients use Agents without introducing a
second sandbox orchestration service or changing existing E2B behavior.

### Goals

- Preserve OpenSandbox request, response and workload semantics, with the
  Agents validation boundary described below.
- Reuse Agents allocation, lifecycle and execution capabilities through its
  API, Manager and Infra layers.
- Support image, snapshot, ordinary Pool, explicit template and
  template-plus-pool creation semantics.
- Apply each creation setting before the workload operation that consumes it.
- Keep API identity, endpoint protection and daemon authentication distinct.
- Preserve existing E2B defaults, ownership and runtime behavior.

### Non-Goals

- Deploying or reproducing the OpenSandbox Lifecycle Server.
- Requiring the reference provider's BatchSandbox, FastSandbox, task-executor
  or internal object topology.
- Replacing Agents orchestration with protocol-specific orchestration.
- Changing native E2B behavior to inherit OpenSandbox defaults.

## Compatibility baseline

The OpenSandbox OpenAPI, server and Python SDK baseline is
`f59755922d92b4e98df805ed7fd4861bc2a38f21`. The [OpenAPI][os-spec] and public
behavior define the external contract. Reference provider code supplies
behavioral context; its internal topology is not the Agents architecture.

Native Agents source references point to the `master` branch of the upstream
`openkruise/agents` repository and describe its existing backend behavior.

Request decoding and validation use the native E2B rules, including ordinary
Go JSON handling of missing, null and unknown fields. SDK defaults are separate
from direct HTTP defaults. Accepted configuration describes the sandbox's
observable behavior, rather than only its response representation.

### Validation boundary

Agents uses the checks already applied by its E2B create path and shared
Manager/backend capabilities. It does not reproduce Lifecycle Server model,
service or provider validation. Authentication, source authorization, admission,
metadata-key protection, resource-update constraints, network and mount checks
remain effective. Reuse their logic through neutral arguments; do not route
OpenSandbox requests through the E2B HTTP handler. [Native parsing][ag-parse],
[claim][ag-claim], [clone][ag-clone].

The OpenSandbox adapter additionally rejects requests it cannot decode or
dispatch unambiguously. The [source rules](#request-routing-and-source-combinations) enumerate those
dispatch checks and their reasons. A new adapter-only check needs an equivalent
explanation in this proposal; a Lifecycle Server rejection alone is insufficient.
Missing entrypoint/resourceLimits, empty optional objects, reserved OpenSandbox
environment names and template/Pool field bans do not by themselves justify
copying a Lifecycle Server validator. Field defaults and workload behavior
remain specified separately. Accepted configuration still has to take effect:
an unimplemented execution capability is a separate implementation gap.

The field comparisons retain OpenSandbox wire definitions and reference-server
restrictions as reference facts. Those restrictions are not Agents acceptance
criteria unless they also follow from the native checks or documented dispatch
rules. For example, metadata values use Agents annotation semantics, without
inheriting label-value limits. Every source uses the E2B timeout normalization
and bounds: omitted/null/zero resolves to 300 seconds, followed by the shared
30-second minimum and configured maximum. There is no separate 60-second
minimum or implicit unlimited lifetime for the OpenSandbox adapter.

JSON decoding follows the native create handler's single Decode operation.
Unknown fields are ignored; null values use the destination Go type's normal
behavior. The adapter adds no stricter nested null, required-field or trailing
JSON checks copied from Lifecycle Server. Incompatible JSON types still fail
decoding. Optional platform, entrypoint, resource and hook fields do not acquire
extra schema checks merely because the reference server has them.

SDK checks that reject before sending HTTP are the caller's responsibility.
They neither require an Agents validator nor constitute a server POC failure.
The create POC sends fixed requests directly over HTTP, without SDK model
construction, SDK normalization or an SDK installation/version gate.

## Architecture

| Component | Responsibility |
| --- | --- |
| `pkg/servers/opensandbox` | OpenSandbox routes, wire models, field/source validation, API authentication, authorization, status/error/endpoint translation |
| Shared `SandboxManager` | Protocol-neutral lifecycle and allocation orchestration, object ownership enforcement, admission and cleanup |
| `infra.Infrastructure` and its implementations | Backend operations and concrete Kubernetes resource access |
| Runtime provider | Initialization, command and file operations through the selected sandbox runtime |
| `cmd/sandbox-manager` | Dependency assembly and route registration |

The dependency direction is `API -> Manager -> Infra`. API handlers pass
neutral arguments to Manager, rather than converting OpenSandbox requests into
E2B request objects. Protocol fields, HTTP statuses and error envelopes stay in
the API layer. Concrete Kubernetes operations stay in Infra implementations.
The sandbox controller remains independent of both API layers and Manager.

Both protocol APIs run in the same sandbox-manager process. The OpenSandbox
adapter uses `OPEN-SANDBOX-API-KEY` and the existing `keys.KeyStorage` for API-key
lookup. E2B routes retain their existing models, middleware and responses.
OpenSandbox error formatting applies to its handler and authentication errors
without changing the E2B representation. [Adapter architecture][issue]

Each API layer resolves its own authentication header and request context,
then passes the authenticated key ID and namespace scope to shared Manager
operations. KeyStorage resolves identity; Manager enforces sandbox ownership
through the native backend path. Protocol-specific authorization responses
remain in the API layer. [Identity storage][ag-key-storage],
[ownership enforcement][ag-manager-auth]

## Creation sources

Source resolution distinguishes explicit template mode from ordinary Pool
mode. A nonblank templateId is resolved before ordinary poolRef rules; image
and snapshot rules apply outside those modes. Field presence and nested-type
validation precede allocation.

| Source | Meaning | Agents mapping |
| --- | --- | --- |
| Image | Requested image and applicable startup configuration | Prepare/reuse a virtual template backed by SandboxSet, then claim or cold-create from that template |
| Snapshot | Authorized recovery source plus current creation settings | Checkpoint identifies recovery content; CloneSandbox creates the new sandbox |
| Ordinary Pool | Select work instances from the requested pool | Resolve the authorized SandboxSet and use ClaimSandbox; retain pool-defined image/resources and allocation-time env/entrypoint |
| Explicit template | Resolve a fixed workload artifact and its configuration | Keep public template identity and artifact semantics separate from an arbitrary SandboxSet name or image alias |
| Explicit template + poolRef | Workload artifact plus an independent capacity constraint | Retain both template and capacity requirements in source resolution |

### Image and virtual templates

A legal image request does not require an operator-maintained image alias.
Virtual-template preparation precedes allocation. Reuse preserves the requested
image and applicable configuration. When warm inventory cannot satisfy the
request, cold creation uses the corresponding template within Agents admission
and allocation constraints. Cold fallback remains part of the virtual-template
path. [Virtual-template direction][issue]

Template preparation and claiming an existing template are separate operations.
Native create-on-no-stock behavior supplies the latter's cold path; it does not
by itself prepare an absent template. [Claim behavior][ag-claim-full]

### Snapshot and clone

A snapshot ID denotes a recovery source; clone is the creation action.
Resolution checks source access, readiness and recovery content. Checkpoint
content types distinguish pod information, filesystem and memory. The
container-snapshot contract preserves filesystem content in a new sandbox;
it does not promise the original PID, memory or live connections.

Current creation settings and new-instance access semantics remain part of
snapshot creation. The presence of a Checkpoint object alone does not define
its recovery guarantees. [Checkpoint contents][ag-checkpoint-type],
[reference restore contract][os-snapshot]

### Pool and explicit template

An ordinary pool supplies work instances and their base image/resources. The
allocation's env and entrypoint belong to the current request. Prewarming and
request-specific execution are distinct stages.

A public template identifies a workload artifact, including build-produced
content and its fixed version. With templateId + poolRef, selecting capacity
must preserve that artifact requirement. Ordinary Pool restrictions are not
applied to template mode merely because both use the poolRef key.

FastSandbox capacity pools and Agents SandboxSets have different allocation
units. Their names do not make their capacity semantics interchangeable.
[Template mapping][os-template-map], [capacity model][os-template-pool]

## Initialization and runtime

Creation separates prewarming from the current allocation's initialization.
Resources, environment, mounts, network configuration and credentials are
applied before the request-specific operation that uses them. Updating a
runtime's environment after a process starts does not change that process's
existing environment.

For lifecycle hooks, preStart executes in the workload environment before the
user entrypoint; failure prevents that entrypoint from starting. Periodic hooks
retain their scheduling and non-overlap semantics. The entrypoint preserves
its argv boundaries. These are execution behavior requirements, independent of
the reference provider's internal task or container arrangement.

Runtime listening, Kubernetes readiness, hook completion, entrypoint startup,
create response and SDK health are distinct observations. Responses reflect
the source mode's public state. SDK endpoint discovery and health checks use
the returned endpoint and credentials. [Create and lifecycle contract][os-spec]

### Generic runtime provider

The [runtime direction in #690][issue] uses a protocol-neutral RuntimeProvider
for initialization, command execution, file operations and runtime addressing.
EnvdRuntimeProvider preserves the existing envd behavior; ExecdRuntimeProvider
adapts those operations to execd's protocol. Provider selection follows the
sandbox's runtime configuration.

Manager uses the neutral runtime operations. Provider implementations own the
runtime-specific HTTP or streaming protocol and credential transport. The API
adapter does not embed envd or execd transport details in Manager/Infra models.

## Lifecycle and API behavior

The OpenSandbox API layer maps lifecycle requests to the shared Manager and
sandbox operations described in [#690][issue]: claim/clone for create,
PauseSandbox and ResumeSandbox for pause/resume, DeleteSandbox for deletion,
and Checkpoint operations for snapshot creation and listing.

Public lifecycle states derive from backend state and conditions. A successful
request acceptance and completion of the resulting transition are distinct.
Metadata patching follows the OpenSandbox merge-patch contract over sandbox
metadata; diagnostics expose the corresponding workload logs and events.

OpenSandbox response construction and error translation stay protocol-specific.
Authentication failures, validation failures, source access/readiness, quota,
capacity and operation timeouts retain their distinct public meanings.

## Identity and credentials

### Control-plane identity and authorization

The OpenSandbox API authenticates `OPEN-SANDBOX-API-KEY` through the existing
`keys.KeyStorage`, shared with the native E2B API. E2B continues to use
`X-API-Key`. The adapter retains the authenticated `CreatedTeamAPIKey` as the
request principal; each protocol keeps its own request context.
[Shared authentication direction][issue], [native authentication][ag-api-auth]

Namespace scope and sandbox ownership have different identities:

| Identity field | Meaning in the adapter |
| --- | --- |
| `CreatedTeamAPIKey.ID` | Caller and sandbox owner ID, passed as `user.ID.String()` |
| `CreatedTeamAPIKey.Key` / `Name` | Authentication credential / display name; neither replaces the key ID as owner |
| `Team.Name` | Kubernetes namespace for an ordinary team |
| `Team.ID` | Team UUID; neither a namespace identifier nor a sandbox owner |
| `sandboxId` | Opaque resource identifier; conveys no authorization by itself |

The identity fields come from authenticated key storage, not caller-supplied
owner or tenant fields in the create request. [Key and team model][ag-key-model]

For ordinary teams, `NamespaceOfUser(user)` uses `Team.Name`. Admin-team keys
retain the native empty-namespace scope for cross-namespace lookup. Legacy
keys without team metadata retain the `TeamForKey` admin fallback. Empty scope
is a lookup convention, not a request to create Kubernetes objects without a
namespace, and does not bypass ownership checks.
[Namespace resolution][ag-namespace], [legacy team handling][ag-team]

Creation authenticates the caller and authorizes the source before allocation,
then passes the resolved namespace and key ID to the shared creation operation.
The native allocation flow records the sandbox owner. Subsequent sandbox
operations use the shared Manager to enforce ownership; listing passes both
namespace and key ID as filters. Namespace membership alone grants no access.
[Native create arguments][ag-claim], [owner binding][ag-owner-binding],
[object authorization][ag-manager-auth], [list filtering][ag-owner-list]

Two keys in the same team do not automatically share sandboxes. A new key ID
does not inherit resources owned by a previous key ID. Admin-team keys can
look up objects across namespaces but still pass the same owner check.
Sandbox IDs remain opaque to the API adapter: short and legacy IDs are
resolved through the existing Manager/cache path, and the resolved object
still requires namespace and owner authorization. The API does not split IDs
to infer a tenant or grant access. [Owner check and lookup][ag-manager-auth]

With authentication enabled, missing or invalid API keys return HTTP 401
before allocation. With authentication explicitly disabled through the existing
configuration, requests use the canonical anonymous principal backed by
`AdminKeyID` and `AdminTeam`. Resources still have an explicit owner, corresponding
to the canonical admin key when authentication is enabled again.
[Native authentication modes][ag-api-auth]

Sandbox-scoped operations return identical HTTP 404 status, `code` and `message`
for a definitively missing sandbox and an ownership denial. Inconclusive
internal lookup failures remain HTTP 500. The API layer applies this response
mapping to Manager errors without exposing ownership details. OpenSandbox
errors use its protocol-specific string `code` and `message` envelope; E2B
error formatting stays unchanged. [Manager error distinctions][ag-manager-auth],
[native lookup status mapping][ag-lookup-status]

### Control-plane compatibility boundaries

The adapter preserves Agents' key-based identity model. The following
differences from the reference server are part of the design:

| Concern | OpenSandbox reference server | Agents adapter |
| --- | --- | --- |
| Tenant identity | `TenantEntry.name` and `namespace` are separate fields | Reuse `CreatedTeamAPIKey` and `Team`; a reference tenant name is not automatically `Team.Name` |
| Multiple keys | A tenant provider can resolve several keys to the same tenant namespace | Namespace scope comes from the team; sandbox ownership remains per key ID |
| Missing / invalid API key | HTTP 401 with `MISSING_API_KEY` / `INVALID_API_KEY` | Both return HTTP 401 with `UNAUTHORIZED` and `Invalid API Key` |
| Tenant provider failure | Provider unavailability can return HTTP 503 with `TENANT_PROVIDER_UNAVAILABLE` | Use the existing `LoadByKey` result and boolean; no separate tenant-provider failure model is introduced |

[Reference tenant fields][os-tenant], [reference authentication][os-auth],
[KeyStorage interface][ag-key-storage]

These mappings retain the existing ownership and admission model; they do not
introduce a tenant provider, cross-key sharing principal or owner/quota split.

### Credential responsibilities

| Mechanism | Request direction and responsibility |
| --- | --- |
| API key | Client to lifecycle API; authenticate the caller and apply the API's authorization policy |
| Secure endpoint token | Client to sandbox endpoint/gateway; apply the secureAccess endpoint protection contract |
| Signed endpoint route | Client to an expiring endpoint route; validate route identity, port and expiry |
| execd/runtime token | Client or control plane to sandbox daemon; enforce the selected runtime's access protocol |
| Registry/outbound credentials | Image pull or sandbox-to-external-service access; separate from inbound endpoint authentication |

The three endpoint/runtime mechanisms protect different paths. Their token
values, response headers and verification steps are not interchangeable.
`secureAccess=false` does not disable lifecycle API authentication, change
sandbox ownership or disable daemon credentials. E2B retains its native
identity and authorization behavior.

## Failure handling and compatibility

Validation and source authorization precede resource allocation. Allocation
and initialization failures retain cleanup responsibility for the sandbox and
request-owned dependent resources. Shared templates and pre-existing volumes
are distinguished from resources owned by the failed request.

The adapter remains opt-in. Native E2B callers retain their existing protocol
behavior and allocation defaults. Runtime-provider integration preserves the
existing envd execution path.

## Create field contracts

The fields cover the public create contract and the corresponding Agents
responsibilities. Reference-provider details explain source semantics; they do
not prescribe the adapter's internal component topology. The field set remains
F01–F16, including explicit template and template-plus-pool creation.

### Request routing and source combinations

Reference processing is lifecycle route -> service factory/composite service
-> provider -> workload/runtime. Agents native create looks up templateID as
a template first, then a checkpoint, and dispatches to ClaimSandbox or
CloneSandbox. [Reference route][os-route], [source model][os-source-schema],
[native dispatch][ag-source].

The following table records the reference server's routing and validation;
it does not prescribe Agents input rejection:

| Source | Reference required inputs | Reference source/shape rules | Reference execution |
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
fixtures can expose these distinctions, but they are not validation-parity
requirements for Agents.

Agents resolves effective selectors before allocation. Blank selectors do not
select a source. Only these additional source-decision checks are required:

- With no effective image URI, snapshotId, templateId or poolRef, reject with
  HTTP 400: there is no creation branch to execute.
- More than one of image, snapshotId and templateId selects competing workload
  sources; reject with HTTP 400 rather than discard one of those identities.
- snapshotId with poolRef is rejected with HTTP 400: restoring a recovery
  artifact and claiming ordinary work inventory are different operations, with
  no combined mode defined here.
- templateId with poolRef selects template mode and retains both artifact and
  capacity constraints. It is not an ambiguity and must not be rejected by an
  ordinary Pool rule.
- Without templateId or snapshotId, poolRef selects ordinary Pool mode; any
  image field does not replace the pool's image. Without poolRef, an effective
  image URI selects the virtual-template path. An existing precedence rule
  does not need another mutual-exclusion error.

These checks choose the creation operation; they do not import the reference
server's blanket bans on template overrides or Pool lifecycle/platform/network
fields. Remaining validation follows the native E2B and shared execution logic.

### Field comparisons

#### F01 — image

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
- **Adapter contract:** Image creation resolves the requested image and pull
  credentials through the virtual-template path, then claims compatible
  inventory or cold-creates. Image identity and applicable configuration are
  preserved.
- **Observable behavior:** Image content and pull authorization match the
  request; cold and warm allocation preserve the same creation semantics.

#### F02 — snapshotId

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
- **Adapter contract:** Resolve an authorized Ready recovery source and use
  Checkpoint/CloneSandbox. Preserve the declared recovery content while applying
  the current creation settings and new-instance access semantics.
- **Observable behavior:** Recovered filesystem content, startup settings,
  expiry and access boundaries follow the selected source contract.

#### F03 — platform

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
- **Adapter contract:** Apply the requested platform to the actual workload and
  execution environment.
- **Observable behavior:** The delivered sandbox uses the requested
  OS/architecture and a compatible runtime.

#### F04 — timeout

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
- **Adapter contract:** Use the native E2B normalization and bounds for all
  creation sources: omitted/null/zero uses 300 seconds; the normalized value
  must be at least 30 seconds and no greater than the configured E2B maximum.
  Apply that duration to the current delivery's deadline, replacing inherited
  pool deadlines. There is no template-only required-timeout check. Keep
  lifetime separate from allocation and SDK readiness budgets.
- **Observable behavior:** createdAt/expiresAt and automatic termination
  describe the same delivery lifetime, including a restored or pooled sandbox.

#### F05 — resourceLimits

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
- **Adapter contract:** Preserve the resource map and translate applicable
  quantities to the workload configuration. Pool and template modes retain their
  own override rules.
- **Observable behavior:** The workload observes the requested limits before
  execution; resource values are not satisfied by response echoing.

#### F06 — resourceRequests

- **Wire/defaults:** optional `map<string,string>`. OpenAPI describes limits
  fallback when omitted. Reference K8s also falls back for `{}`; a nonempty
  supplied map replaces the whole requests map, without per-key filling.
- **OpenSandbox server:** K8s resource construction is gated on nonempty
  converted limits. Docker and ordinary Pool do not consume requests. Template
  create prohibits overrides. [Builder][os-container],
  [Pool branch][os-batch-create].
- **Agents native server:** CPU/memory request extensions populate claim
  InplaceUpdate; clone does not support it. No equivalent whole-map fallback
  or arbitrary resource map exists at native create. [Extensions][ag-ext].
- **Adapter contract:** Keep resourceRequests distinct from resourceLimits.
  Apply the whole-map fallback for an absent/empty requests map; do not fill
  individual missing keys from old inventory.
- **Observable behavior:** The resulting requests reflect the request map and
  applicable backend defaults, rather than unrequested per-key inventory
  inheritance.

#### F07 — env

- **Wire/defaults:** optional string map; absent/null/empty means no request
  variables. Reserve `OPENSANDBOX_LIFECYCLE` for hook transport. Template
  create rejects any non-null env, including `{}`.
- **OpenSandbox server:** new-container variables are installed before user
  startup. Docker merges defaults then request values;
  K8s emits container env. Allowed `OPENSANDBOX_EGRESS_*` keys are separated
  for egress; without policy those keys are dropped with a warning. Pool
  uses each allocation's env in its task; snapshot uses the current request.
  [Context][os-context], [env split][os-env-helper], [Pool task][os-pool-task].
- **Agents native server:** claim sends envVars to POST /init unless skipped;
  this cannot alter already-running prewarmed processes. Clone reads old
  checkpoint init data; ReInit 401 is treated as already initialized.
  [Claim][ag-claim], [clone restore][ag-clone-restore], [init][ag-init].
- **Adapter contract:** Apply the current request environment to the workload
  operations for which it is defined, including per-allocation execution.
- **Observable behavior:** Hooks and the user entrypoint observe their intended
  environment on first execution.

#### F08 — metadata

- **Wire/defaults:** optional string map; absent/null/empty contributes no user
  metadata. Public examples include a human-readable name with spaces.
- **OpenSandbox server:** container providers validate metadata as labels,
  including key/value syntax, 63-character value limit and reserved
  `opensandbox.io/` prefix. They store workload/container labels used for
  filtering.
  [Validation][os-labels], [context][os-context].
- **Agents native server:** extracts reserved E2B extensions first, then
  validates ordinary keys and stores annotations. Annotation values do not
  have the same label limit; `name` does not rename the Sandbox.
  [Parsing][ag-parse], [modifier][ag-modifier].
- **Adapter contract:** Keep public user metadata distinct from internal
  resource bookkeeping and preserve its protocol-level create/read/filter/patch
  semantics. Apply native metadata-key checks and annotation value semantics;
  do not copy the reference provider's label-value restrictions.
- **Observable behavior:** Metadata operations refer to the same user-visible
  map and do not expose or overwrite internal identity data.

#### F09 — entrypoint

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
- **Adapter contract:** Execute the source mode's entrypoint with its argv
  boundaries preserved, after the configuration and preStart behavior it depends
  on.
- **Observable behavior:** The requested command actually executes with the
  intended configuration; entrypoint response data alone does not supply that
  behavior.

#### F10 — networkPolicy

- **Wire/defaults:** optional object with optional `defaultAction` and ordered
  `egress[]`; each rule requires `action` and nonempty `target`. Actions are
  allow/deny. Targets include domains/wildcards; the inspected executor also
  supports IP/CIDR. Omission/null and present `{}` are distinct.
- **OpenSandbox server:** absent/null creates no request egress sidecar.
  Domain rules use first-match order; dns and dns+nft have different enforcement paths. Ordinary Pool
  rejects policy, while template-plus-pool accepts policy through its own
  mapping. [Sidecar][os-egress], [executor][os-policy],
  [template mapping][os-template-map].
- **Agents native server:** internet access defaults true. allowOut accepts
  IP/CIDR/concrete FQDN but not wildcards; denyOut accepts IP/CIDR. Separate L7
  security rules exist. These grouped policies do not directly implement
  ordered OpenSandbox rules. Network creation follows claim/clone and failure
  kills the allocated sandbox. [Network][ag-network], [security][ag-security],
  [native create][ag-create].
- **Adapter contract:** Preserve explicit rule ordering, actions and destination
  semantics, with source-mode validation. Apply effective access rules before
  the workload traffic they govern.
- **Observable behavior:** The workload's outbound connections follow its
  policy, including ordered rule conflicts and template-plus-pool handling.

#### F11 — secureAccess

- **Wire/defaults:** boolean default false; explicit null is not the boolean
  contract. Template model rejects true but accepts false.
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
- **Adapter contract:** Preserve secureAccess semantics through endpoint
  credential delivery and validation, separately from lifecycle API and daemon
  authentication.
- **Observable behavior:** Valid, missing and incorrect endpoint credentials
  produce the corresponding access behavior; false does not remove unrelated
  authentication.

#### F12 — credentialProxy

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
- **Adapter contract:** Keep outbound credential injection separate from
  image-pull credentials, environment values and inbound access tokens. Disabled
  configuration does not enable interception.
- **Observable behavior:** Enabled behavior provides the contracted
  authenticated injection and HTTPS trust; credentials remain isolated to the
  intended destination and sandbox.

#### F13 — lifecycle

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
- **Adapter contract:** Preserve the lifecycle-hook behavior: run preStart
  before the entrypoint, block startup on its failure, and retain periodic
  scheduling, cancellation and non-overlap semantics.
- **Observable behavior:** Hook order, execution environment, failure and
  restart behavior agree with the lifecycle contract; runtime listening is a
  separate observation.

#### F14 — volumes

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
- **Adapter contract:** Preserve source selection, mount path, read-only/subPath
  behavior and volume ownership semantics. Mount configuration is effective
  before the workload uses it.
- **Observable behavior:** Workload-visible content and access rights match the
  request; failure and termination cleanup respect pre-existing and shared
  storage.

#### F15 — extensions

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
- **Adapter contract:** Distinguish ordinary Pool selection from
  template-plus-pool capacity selection. For ordinary pools, retain pool-defined
  image/resources and the allocation's env/entrypoint. Interpret each known
  extension by its own contract.
- **Observable behavior:** Pool selection preserves source authorization and
  constraints; renewal/isolation extensions describe actual behavior rather than
  opaque parameter storage.

#### F16 — templateId

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
- **Adapter contract:** Keep the public template's identity, authorized artifact
  and version separate from capacity selection. Retain both constraints when
  poolRef is present; ordinary Pool rules do not overwrite template-mode rules.
- **Observable behavior:** The sandbox receives the resolved template artifact
  under the selected capacity constraint, including its build-produced content
  and recorded startup configuration.

### Response, errors and readiness

| Public behavior | Adapter responsibility |
| --- | --- |
| Create response | Return 202 and the required id/status/createdAt/entrypoint fields; derive state from the actual operation |
| createdAt / expiresAt | Represent the current delivery's creation time and expiry policy |
| Metadata | Keep a consistent user-visible map across its supported operations |
| Status | Translate backend phase and conditions to the OpenSandbox lifecycle state |
| Error envelope | Format errors according to the OpenSandbox route, including authentication and validation failures |
| Readiness | Keep request acceptance, runtime readiness, workload startup and SDK health distinct |

The create response has eight top-level fields: required id, status, createdAt
and entrypoint, plus optional metadata, extensions, platform and expiresAt.
Status contains required state and optional reason, message and lastTransitionAt.
[Response schema][os-spec]

Error translation distinguishes input validation, source authorization and
readiness, quota, capacity and operation timeout. The API layer owns the public
status and error representation. [Lifecycle contract][os-spec]

### SDK behavior is a separate layer

- Ordinary `Sandbox.create()` defaults timeout to 600 seconds, resourceLimits
  to cpu=1/memory=2Gi, and entrypoint to the keep-alive command. It sends
  secureAccess=false. `timeout=None` explicitly sends null. These are client
  choices, not direct HTTP defaults.
- The ordinary high-level SDK requires image or snapshot_id; a server-valid
  pool-only request can be sent directly over HTTP. Empty maps may be omitted
  by conversion. Such client restrictions and normalization are the caller's
  responsibility and are outside the create POC acceptance boundary.
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

## Test plan

Design checks cover the field contract across the valid creation sources,
source permissions, image/configuration selection, warm and cold allocation,
restored content, first-entrypoint configuration, hook ordering and failure,
endpoint/runtime access controls, state/error translation and resource cleanup.

The create POC uses a small set of fixed direct-HTTP profiles for image,
snapshotId, ordinary poolRef, templateId and templateId+poolRef. It observes
real resources, startup effects, recovery content and source constraints;
HTTP acceptance alone is insufficient. Observation, probes and cleanup can
use kubectl independently of lifecycle endpoints.

Validation tests follow native E2B rules and the documented source-decision
checks, not the Lifecycle Server's negative-case matrix. SDK pre-send rejection
is outside this POC and does not count as a failed server request. Endpoint
health and other SDK lifecycle integration are separate from create-only
acceptance. E2B regression checks verify unchanged native behavior.

### Control-plane identity tests

| Scenario | Required behavior |
| --- | --- |
| Valid key creates a sandbox | The request principal, namespace and owner passed to allocation match the authenticated key; caller-supplied identity fields cannot override them |
| Missing or invalid key with authentication enabled | HTTP 401 with the OpenSandbox error envelope; no allocation occurs |
| Ordinary team | Namespace uses `Team.Name`; owner uses key ID, not team UUID or key name |
| Admin team or legacy key without team metadata | Preserve native namespace scope and fallback while retaining a concrete owner |
| Authentication disabled, then enabled | Use the canonical anonymous/admin identity; the canonical admin key retains the corresponding owner identity |
| E2B and OpenSandbox in the same process | Preserve each header, request context and error format while sharing KeyStorage and Manager |
| Same-team or cross-team key with a different owner ID | Deny object access and omit other owners' sandboxes from lists, including for admin-team keys |
| Unauthorized versus definitively missing sandbox | Return identical HTTP 404 status, code and message |
| Inconclusive infrastructure lookup failure | Preserve HTTP 500 instead of reporting a definitive absence |
| Short and legacy IDs for the same protected object | Resolve through the shared path and apply the same namespace and owner checks |

Creation identity checks and subsequent object/list authorization checks are
independent of data-plane credential tests and the fixed create POC. Native
E2B regression tests cover its unchanged authentication and authorization.

[issue]: https://github.com/openkruise/agents/issues/690
[ag-key-storage]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/keys/interface.go#L38-L51
[ag-key-model]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/models/api_key.go#L28-L72
[ag-api-auth]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/routes.go#L126-L175
[ag-namespace]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/sandbox.go#L110-L117
[ag-team]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/keys/utils.go#L26-L36
[ag-owner-binding]: https://github.com/openkruise/agents/blob/master/pkg/sandbox-manager/infra/sandboxcr/claim.go#L941-L955
[ag-manager-auth]: https://github.com/openkruise/agents/blob/master/pkg/sandbox-manager/api.go#L216-L263
[ag-owner-list]: https://github.com/openkruise/agents/blob/master/pkg/cache/cache.go#L363-L403
[ag-lookup-status]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/sandbox.go#L79-L91
[os-auth]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/middleware/auth.py#L78-L153
[os-tenant]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/tenants/models.py#L21-L25
[os-spec]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/specs/sandbox-lifecycle.yml
[ag-claim-full]: https://github.com/openkruise/agents/blob/master/pkg/sandbox-manager/infra/sandboxcr/claim.go#L595-L889
[ag-checkpoint-type]: https://github.com/openkruise/agents/blob/master/api/v1alpha1/checkpoint_types.go#L115-L146
[os-snapshot]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/snapshot_restore.py#L29-L105
[os-template-map]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/fast_sandbox/create_mapping.py#L155-L207
[os-template-pool]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/docs/architecture/fast-sandbox/scheduling.md#L7-L30
[os-route]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/lifecycle.py#L59-L120
[os-source-schema]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/schema.py#L576-L649
[ag-source]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/create.go#L82-L120
[os-docker-values]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/docker/container_ops.py#L283-L360
[os-batch-create]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/batchsandbox_provider.py#L127-L342
[os-agent-provider]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/agent_sandbox_provider.py#L126-L255
[ag-claim]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/create.go#L122-L219
[ag-clone]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/create.go#L221-L312
[ag-ext]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/models/extensions.go
[os-composite]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/composite_service.py#L57-L67
[os-docker-snapshot]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/docker/snapshot_runtime.py#L150-L189
[os-k8s-snapshot]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/snapshot_runtime.py
[ag-clone-restore]: https://github.com/openkruise/agents/blob/master/pkg/sandbox-manager/infra/sandboxcr/clone.go#L267-L497
[ag-checkpoint]: https://github.com/openkruise/agents/blob/master/api/v1alpha1/checkpoint_types.go#L114-L175
[os-schema]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/schema.py#L453-L652
[os-platform]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/validators.py#L221-L265
[os-docker-platform]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/docker/container_ops.py#L79-L220
[ag-model]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/models/sandbox.go#L34-L153
[os-request]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/schema.py#L452-L649
[os-context]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/create_helpers.py#L54-L142
[ag-parse]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/create.go#L314-L393
[ag-modifier]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/create.go#L395-L452
[os-k8s-resources]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/provider_common.py#L46-L113
[os-container]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/provider_common.py#L116-L240
[os-resource-parser]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/helpers.py#L70-L150
[os-env-helper]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/helpers.py#L254-L280
[os-pool-task]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/batchsandbox_provider.py#L513-L549
[ag-init]: https://github.com/openkruise/agents/blob/master/pkg/utils/runtime/init.go#L39-L94
[os-labels]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/validators.py#L94-L130
[os-egress]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/egress_helper.py#L83-L159
[os-policy]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/components/egress/pkg/policy/policy.go#L39-L165
[ag-network]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/network.go#L37-L99
[ag-security]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/securityrules.go
[ag-create]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/create.go
[os-k8s-guards]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/kubernetes_service.py#L495-L577
[os-endpoint]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/endpoint_resolver.py
[ag-response]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/sandbox.go
[os-credential]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/validators.py#L553-L585
[os-hooks-schema]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/schema.py#L142-L207
[os-hook-config]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/components/execd/pkg/lifecycle/config.go#L33-L218
[os-execd-main]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/components/execd/main.go#L216-L317
[os-periodic]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/components/execd/pkg/lifecycle/periodic.go#L35-L97
[os-docker-create]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/docker/docker_service.py#L613-L680
[os-volume-schema]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/schema.py#L218-L419
[os-volumes-valid]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/validators.py#L622-L706
[os-k8s-volumes]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/volume_helper.py#L40-L125
[os-docker-volumes]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/docker/volumes.py
[os-k8s-create]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/kubernetes_service.py#L919-L1087
[os-return]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/kubernetes_service.py#L1087-L1215
[ag-csi]: https://github.com/openkruise/agents/blob/master/api/v1alpha1/mount_types.go
[ag-volume-validation]: https://github.com/openkruise/agents/blob/master/pkg/servers/e2b/models/validation.go
[os-pool]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/batchsandbox_provider.py#L375-L425
[os-wait]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/k8s/kubernetes_service.py#L333-L493
[os-extension-validation]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/extensions/validation.py#L24-L106
[os-extension-codec]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/extensions/codec.py#L27-L90
[ag-options]: https://github.com/openkruise/agents/blob/master/pkg/sandbox-manager/infra/types.go#L45-L137
[ag-pool-manual]: https://openkruise.io/kruiseagents/user-manuals/warmpool-management
[ag-set-types]: https://github.com/openkruise/agents/blob/master/api/v1alpha1/sandboxset_types.go
[os-template-source]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/schema.py#L563-L649
[os-template-resolve]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/templates/template_service.py#L324-L344
[os-template-doc]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/docs/architecture/fast-sandbox/templates.md
[ag-template-selection]: https://github.com/openkruise/agents/blob/master/pkg/sandbox-manager/infra/sandboxcr/claim.go#L664-L708
[ag-template-type]: https://github.com/openkruise/agents/blob/master/api/v1alpha1/sandboxtemplate_types.go
[ag-set-revision]: https://github.com/openkruise/agents/blob/master/pkg/controller/sandboxset/revision.go#L36-L73
[sdk-create]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/sdks/sandbox/python/src/opensandbox/sandbox.py
[sdk-converter]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/sdks/sandbox/python/src/opensandbox/adapters/converter/sandbox_model_converter.py
[sdk-template]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/sdks/sandbox/python/src/opensandbox/sandbox.py#L638-L822
[sdk-connection]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/sdks/sandbox/python/src/opensandbox/config/connection.py
