---
title: OpenSandbox-Compatible API Adapter
authors:
  - "@zhuangzhewei09"
reviewers:
  - "@TBD"
creation-date: 2026-09-18
last-updated: 2026-09-27
status: provisional
see-also:
  - "https://github.com/openkruise/agents/issues/690"
  - "https://github.com/openkruise/agents/pull/989"
---

# OpenSandbox-Compatible API Adapter

## Summary

Add OpenSandbox-compatible routes to the existing sandbox-manager listener,
sharing Manager orchestration and API-key storage with E2B. A valid create must
apply its configuration to the delivered sandbox and complete the official
SDK's endpoint and health flow. The existing static image alias implementation
is a control-plane PoC; it does not yet meet this contract.

PR1 is limited to the create API: all 16 fields and nested values,
image/snapshot/ordinary Pool/explicit-template source handling, and the internal
capabilities needed to apply creation settings. PR2 includes PR1 and adds the
subsequent lifecycle endpoints; those changes do not flow backward into PR1.
Public template/credential management, endpoint and command/file APIs remain
separate dependent work. Complete SDK acceptance is checked across the relevant
PRs, not inferred from PR1's create response alone.

The selected image direction is configuration/revision-compatible virtual
templates with cold fallback. Official documentation and source inspection also
establish ordinary poolRef -> SandboxSet -> ClaimSandbox, the single target
template of each pool, and the startup/clone gaps described below. Template and
pool resolution should converge on shared allocation. The remaining storage,
artifact-placement and runtime mechanism choices remain a **design for review**;
the complete implementation is still pending. Detailed
source-backed comparisons live in the [field appendix](./20260918-opensandbox-create-field-comparison.md).

## Contents

- [Compatibility baseline and current implementation](#compatibility-baseline-and-current-implementation)
- [Goals and boundaries](#goals-and-boundaries)
- [Verified capabilities and remaining choices](#verified-capabilities-and-remaining-choices)
- [Architecture and missing capabilities](#architecture-and-missing-capabilities)
- [Creation sources](#creation-sources)
- [Initialization and readiness](#initialization-and-readiness)
- [Identity and credentials](#identity-and-credentials)
- [Failure ownership and cleanup](#failure-ownership-and-cleanup)
- [Decisions for review](#decisions-for-review)
- [Implementation and validation plan](#implementation-and-validation-plan)
- [Compatibility and rollout](#compatibility-and-rollout)
- [Alternatives](#alternatives)
- [Implementation history](#implementation-history)

## Compatibility baseline and current implementation

| Surface | Fixed baseline | Meaning |
| --- | --- | --- |
| OpenSandbox OpenAPI, Server and Python SDK | `f59755922d92b4e98df805ed7fd4861bc2a38f21` | Target includes `templateId`, template-plus-pool creation and source-aware SDK readiness |
| Agents native E2B, Manager, Infra and runtime; PR #989 | `8771012c3e6e81550888931ab522e9d865f8ab00` | Implementation snapshot used by the field comparison |
| Earlier PoC | Server `v0.2.3`, Python SDK `v0.1.16` | Historical low-level create verification; not full SDK or current-contract acceptance |

The public [OpenAPI][os-spec] and official behavior documentation define the
target. Reference Server/provider/runtime code explains execution and exposes
ambiguities. Docker, BatchSandbox, OpenSandbox's agent-sandbox provider, and
FastSandbox have different capabilities. The OpenSandbox agent-sandbox provider
is a separate project from OpenKruise Agents.

The field appendix separates the public contract, reference behavior, Agents
native behavior, current adapter, proposed mapping and observable acceptance.
Reference implementation defects and backend-specific restrictions require an
explicit compatibility decision; they do not automatically become Agents
requirements.

Reverification on 2026-09-27 also inspected upstream Agents master
`1015db48209922e18de347198980b98f88a9a188`. The relevant API types, SandboxSet
revision construction, claim/clone, pause/resume and agent-runtime sources are
unchanged from the implementation baseline. The runtime transport has newer TLS
changes; those do not supply the missing startup capability. Live documentation
is supporting evidence; fixed source remains the behavior baseline.

The audited [create snapshot][compat-create] resolves `image.uri` using a static alias,
then calls `ClaimSandbox`. It delivers env through InitRuntime, writes metadata
annotations and sets a deadline. It rejects snapshot restoration and unknown
fields, ignores several parsed fields and only echoes entrypoint. Its missing
alias response is currently **500**, not the 400 described by the earlier
proposal. It registers only [POST create][compat-route]; real SDK endpoint,
health and cleanup routes are incomplete. That snapshot also
leaves `CreateOnNoStock` false, unlike native E2B's default.
The compatibility switch is `--enable-opensandbox-compat`; the alias table is
read once from `OPENSANDBOX_IMAGE_ALIASES` (typically ConfigMap envFrom), not
the alias flag described in the original proposal. Registration occurs after
shared dependencies are initialized and before the listener starts.

[Issue #690][issue] proposes the shared adapter, virtual-template and runtime
provider directions. The source-selection, version, capacity, persistence and
initialization details below extend that direction for review. The issue's
initial expectation of no Infra/CRD/controller changes is not yet demonstrated
for the complete create contract; concrete missing capabilities are listed
before selecting any such change.

The subsequent PR1 revision `4cdaa77` corrects nullable timeout and its minimum,
request validation, existing-template cold creation, metadata cleanup tracking,
Pending state reporting and the OpenSandbox error body. PR2 revision `0b39132`
merges that PR1 revision into the lifecycle branch. These changes do not close
the remaining source/startup or SDK acceptance gaps described in this proposal.

## Goals and boundaries

- Preserve public request shapes, source combinations, defaults, response and
  error semantics, including missing/null/empty distinctions.
- Resolve image requests through compatible virtual templates and SandboxSet
  claims, with cold creation when compatible inventory is unavailable.
- Resolve snapshots through checked recovery artifacts and CloneSandbox;
  resolve explicit templates independently from capacity selection.
- Apply env, resources, mounts, egress and credentials before the first
  request-specific hook or user command that depends on them.
- Deliver usable endpoints and enforce access controls with real runtime and
  gateway integration; clean up failed deliveries.
- Preserve E2B behavior and keep the adapter disabled by default.

The implementation does not introduce the OpenSandbox Lifecycle Server or
require its BatchSandbox/FastSandbox controllers. Internal object types,
checkpoint formats, task executors and daemon placement may differ if the
observable contract is satisfied. Unresolved fields remain required work, not
accepted no-ops or unsupported placeholders.

## Verified capabilities and remaining choices

These conclusions follow from official documentation and executable source.
They establish capabilities and gaps, rather than approval of a new subsystem
or a report of runtime acceptance.

| Question | Verified result | Design consequence / remaining choice |
| --- | --- | --- |
| Virtual template and cold creation | Claim selects an existing SandboxSet; create-on-no-stock materializes that set's effective template. Selection prefers the latest revision but can return older stock. [Claim source][ag-claim-full] | Reuse native claim/cold creation. Ensure of an absent virtual template and exact compatibility filtering remain adapter/Manager additions. Manual-pool adoption, tag refresh and GC are policies, not answered by the native API. |
| Ordinary poolRef and template-plus-pool | A named poolRef maps directly to a namespaced SandboxSet. Each set has one target inline template or templateRef; rolling updates can retain old/new revisions. Native claim has one pool selector. FastSandbox supplies artifact and capacity independently. [Pool manual][ag-pool-manual], [types][ag-set-types], [mapping][os-template-map] | Close the ordinary-pool mapping question. Merge resolution into the shared claim path while retaining template compatibility. A general multi-artifact capacity pool has no direct native counterpart; the minimal representation still needs design review. |
| Snapshot contents and overrides | Native Checkpoint distinguishes podInfo, filesystem and memory. Clone copies checkpoint init data, including the runtime token. OpenSandbox's container restore creates a new workload from a filesystem image plus current request settings. [Checkpoint][ag-checkpoint-type], [clone][ag-clone-restore], [restore][os-snapshot] | Use Checkpoint/CloneSandbox as the backend basis. Check actual recovery content and add current startup overrides; podInfo alone cannot implement filesystem recovery. Driver and memory-restoration behavior need execution tests. |
| Hooks, entrypoint and readiness | OpenSandbox specifies preStart before user entrypoint, failure blocking, and non-overlapping periodic hooks. Agents claim env affects runtime-launched commands, not an already running main process; InitRuntime has no hooks/entrypoint fields. [Hooks][os-spec], [claim manual][ag-claim-manual], [init options][ag-init-options] | Startup order and missing capabilities are established. Evaluate the in-sandbox shim against the shipped envd implementation; a new supervisor is only one candidate. #669 supplies proposals, not helper implementation. [Runtime build][ag-runtime-build], [#669][ag-helper-pr] |
| Asynchronous pause | OpenSandbox specifies 202 plus observable state convergence. Agents already persists pause intent on Sandbox, then its native method waits for completion. [Contract][os-spec], [native pause][ag-pause-source] | Preserve the public contract when implementing lifecycle compatibility; separate durable submission from waiting if needed. There is no technical basis for requiring another Lifecycle Server or removing pause. A scope change would be a separate decision. |
| Identity and endpoint credentials | OpenSandbox tenant records can contain multiple API keys. Agents passes Key ID as owner and separately restricts namespace by Team. Static endpoint access, signed routes and execd authentication have different validation paths. [Tenant][os-tenant-model], [auth][os-auth], [native ownership][ag-owner-source], [endpoint][os-lifecycle-api] | The identity mismatch and credential separation are established. Stable tenant ownership, quota attribution and route integration remain concrete compatibility design choices; header renaming alone cannot implement them. |

The claim manual describes waiting as the default on stock miss, whereas the
pinned native E2B extension parser defaults create-on-no-stock to true. The
adapter must set allocation policy explicitly instead of inheriting that
documentation ambiguity. [Pinned extension parser][ag-ext]

## Architecture and missing capabilities

Both implementation and design remain tracked under [#690][issue]. PR1 changes
only the create API. PR2 carries PR1's changes and adds the lifecycle routes on
top; lifecycle changes must not be merged backward into PR1. Full SDK acceptance
also depends on the later endpoint/health/cleanup work, so a successful create
response alone does not establish complete SDK compatibility.

Create errors need a protocol-specific body for both handler and authentication
failures. Add an opt-in `web.RegisterRouteWithErrorFormatter` entry point that
preserves the existing middleware, tracing, panic recovery and transport flow,
while formatting errors as `{code: string, message: string}` for OpenSandbox.
Existing `RegisterRoute` callers pass no formatter and retain their native
representation. Request IDs remain in response headers on OpenSandbox errors.
This is an API-layer transport change; it adds no Manager/Infra interface.

The dependency direction remains `API -> Manager -> Infra`.

| Layer | Responsibility in this design | Existing basis / required addition |
| --- | --- | --- |
| `pkg/servers/opensandbox` | Wire models, presence-aware decoding, source/combination rules, auth, authorization, public status/error/endpoint translation | Replace the alias-only planner; complete create-related routes; do not convert into an E2B request |
| `pkg/sandbox-manager` | Neutral template/artifact resolution, allocation/capacity policy, quota, delivery stages and compensating cleanup | Reuse ClaimSandbox/CloneSandbox; add only capabilities that the selected path requires |
| `pkg/sandbox-manager/infra` and `infra/sandboxcr` | Neutral backend contracts and concrete template/inventory/artifact/storage operations | Ensure/read managed templates, exact compatible inventory, clone startup overrides and resource ownership are gaps |
| `pkg/utils/runtime` and runtime implementation | Initialization, controlled user startup, hook scheduling, real health and transport | Existing InitRuntime sends env/token; it does not set arbitrary resources or launch a requested entrypoint |
| Network/storage implementations | Enforce policy and credential injection, prepare mounts and report completion | Existing TrafficPolicy/security rules/CSI are partial building blocks, not equivalent implementations of all fields |
| `cmd/sandbox-manager` | Compose dependencies and register routes before serving | Keep business logic out of the entrypoint |

The following are **proposed capabilities**, not existing Go method names or
final interface signatures:

| Existing interface / evidence | Missing behavior | Smallest proposed extension | Required observation |
| --- | --- | --- | --- |
| [ClaimSandboxOptions][ag-options] selects an existing pool | Prepare an absent virtual template | Manager ensure orchestration over neutral Infra template operations | Concurrent identical creates converge; registry credentials stay isolated |
| [Claim selection][ag-template-selection] prefers new revision then permits old inventory | Exact configuration/artifact pinning | Optional compatibility/revision requirement per claim; existing callers retain their policy | An old revision never satisfies an incompatible create |
| `CreateOnNoStock` creates from an existing SandboxSet | Distinguish cold fallback from explicit capacity limits | Source-specific allocation options and bounded waits | Image can cold-create; a named capacity pool is not silently escaped |
| [Clone options][ag-options] and [clone preparation][ag-clone-restore] restore old init data | Current env/resources/entrypoint and fresh credentials | Optional startup overrides and artifact content requirements before restore/start | Restored files plus current config; old credentials fail |
| [InitRuntime][ag-init] delivers env/token and treats reinit 401 as already initialized | Confirm a new delivery's config and release its entrypoint exactly once per startup | Runtime startup operation keyed by delivery and startup generation | Retry does not run entrypoint twice; rejected config is not reported as applied |
| E2B applies network after allocation | Protection before hooks/entrypoint | Manager-controlled preparation and runtime release barrier | No unprotected first connection from user code |
| CSI mounts an identified source | PVC provision/ownership and all public volume backends | Neutral storage preparation/release with owned-resource receipts | Pre-existing volumes survive failures and termination |
| SandboxTemplate stores workload configuration; Claim/clone choose execution sources | Public template build state/artifact lookup and template-plus-pool resolution | Reuse native template/checkpoint/pool resources; add only missing catalog/build metadata and resolution | Restart, build failure, deletion and template-plus-pool cases; no artifact or pool constraint lost |
| Manager uses `User` for ownership and quota admission | Tenant visibility with separate API-Key accounting | A stable authorized subject plus explicit provenance/accounting identity, preserving native defaults | Same-tenant key rotation works; foreign tenants and native E2B ownership remain isolated |

Concrete K8s reads/writes stay in Infra implementations. Manager consumes
neutral records and receipts; controllers do not depend on Manager/API code.
New operations use informer-backed reads. This design does not request a new
APIReader bypass, a global feature gate, or new metrics.

## Creation sources

### Source resolution

Resolve nonblank `templateId` first, then ordinary `poolRef`, then image versus
snapshot. The four modes are not four universally exclusive input fields.
Nested type validation and missing/null/empty rules still run before side
effects; see [F01–F16](./20260918-opensandbox-create-field-comparison.md#field-comparisons).

| Mode | Public input and effective configuration | Proposed Agents path |
| --- | --- | --- |
| Image | Actual URI/auth, platform, resources and permitted per-delivery fields | Resolve image/config -> ensure managed virtual template -> compatible claim or cold creation |
| Snapshot | Accessible Ready artifact, recovery content, current request settings | Resolve snapshot -> checkpoint/artifact check -> CloneSandbox with startup overrides |
| Ordinary Pool | No templateId; pool supplies base image/resources, request supplies permitted allocation settings | poolRef -> authorized SandboxSet -> ClaimSandbox -> delivery initialization |
| Explicit template | Fixed workload artifact/config; required timeout; metadata/networkPolicy; optional poolRef | Resolve template artifact and selected/default pool -> compatible shared claim plan; evaluate clone only for a recovery artifact |

An image object in ordinary Pool mode does not replace the pool image in the
reference BatchSandbox implementation. An explicit template can use poolRef
and networkPolicy together. Copying ordinary Pool restrictions into that
branch would reject valid template requests.

### Image: managed virtual templates and cold fallback

Selected direction: match virtual templates to the requested configuration and
version, with cold fallback when compatible inventory is unavailable. The
following matching, persistence, credential and GC details remain proposed:

1. Resolve the requested image and an immutable effective startup
   configuration. Resolve mutable tags to a digest for a new template
   revision; define refresh at request resolution, not by mutating an admitted
   delivery. This is a proposed policy, requiring registry-resolution tests.
2. Use a canonical configuration digest as an index scoped to the tenant.
   Compare the full effective configuration after an index hit. Include image
   digest, platform, resources, runtime/startup ABI and immutable mount/layout
   requirements. Include a private credential reference/version, never raw
   credentials, in reuse authorization.
3. Keep timeout, user metadata, per-delivery env, hooks, entrypoint and access
   credentials out of the index only when the runtime can apply them safely
   before user startup. Until that capability exists, use an exact compatible
   configuration/new instance; sharing a URI alone is insufficient.
4. Ensure only adapter-managed SandboxSets. Do not auto-adopt a manually
   managed pool. A concurrent ensure uses deterministic identity plus a
   conditional create/read/compare; cancellation releases only this request's
   reference, not another caller's shared template.
5. Claim compatible inventory with the pinned revision. If none exists,
   enable cold creation from the ensured template within quota/deadline limits.
   The runtime waits for delivery configuration before releasing user code.

Initial managed pools may have zero standby inventory and grow according to
operator policy. Template GC requires no active deliveries/builds/references
and a configurable idle period; its deletion must race safely with ensure and
claim. Fixed GC intervals and warm counts remain operator-policy choices.

### Snapshot: recovery content and current request overrides

The container reference path restores a filesystem image into a **new**
sandbox; it does not promise the original memory, PID or live connections.
FastSandbox snapshots have a separate backend routing path. Agents Checkpoint
can describe podInfo/filesystem/memory; these are distinct recovery profiles.

Recommended mapping binds a public snapshot to an accessible Ready checkpoint
and a declared content profile. Filesystem-compatible snapshots require actual
filesystem content. A podInfo-only checkpoint does not satisfy that profile.
Preserve the filesystem artifact while applying current env/resources/metadata,
network, mounts and deadline; default omitted entrypoint to the reference
keep-alive. A missing/null timeout clears inherited automatic expiry.

Allocate a new delivery identity and new access credentials. Apply resource
and startup changes before the restored user program can run. Clone's existing
reinit-401 treatment cannot prove that new settings were installed; a new
startup acknowledgement is required. Memory-restoring artifacts need a
separate reviewed compatibility profile because replaying a captured process
can violate these startup guarantees.

### Ordinary Pool: inventory and allocation-time execution

**Verified mapping:** a named poolRef selects a SandboxSet in the authorized
namespace and becomes `ClaimSandboxOptions.Template`. The E2B `templateID`
and SandboxClaim `templateName` already use this pool selector. This mapping
does not require a new pool kind. [Claim manual][ag-claim-manual],
[claim options][ag-options]

A SandboxSet maintains one desired workload template, inline or referenced by
`spec.templateRef`. It is not a general heterogeneous workload pool. This is
not an invariant that every live instance always has identical configuration:
rolling upgrades temporarily mix revisions, and claim can replace the selected
instance's main-container image or resize its CPU/memory. Claim removes the
pool owner reference, allowing stock replenishment. `replicas` counts unused
inventory, not a total active-sandbox capacity limit. These distinctions matter
when translating pool policies. [Types][ag-set-types],
[revision construction][ag-set-revision], [claim][ag-claim-full]

Resolve the requested work-instance pool in the authorized namespace; `"*"`
uses a deterministic eligible-pool selection policy. Validate combinations
before allocation. The recommended default is bounded waiting for available
inventory and a source-specific capacity error on exhaustion, without escaping
an explicitly requested pool. Image virtual-template cold fallback is a
different policy. Native E2B defaults remain unchanged.

The runtime starts a delivery-specific command after env/token/mount setup,
including the default keep-alive when entrypoint is omitted. Prewarming may
start the system daemon, but not this delivery's user command. A failed or
contaminated allocation is destroyed unless complete reset can be proven.

### Explicit template: artifact identity and capacity

A public template is a tenant-private build record with state and an immutable
artifact revision. It is not an arbitrary E2B templateID, SandboxSet name, or
image alias. A successful build records its input configuration, artifact
reference/digest and runtime compatibility. Create pins the resolved revision;
subsequent template updates cannot alter an admitted sandbox.

Agents already has SandboxTemplate and SandboxSet.templateRef; reuse these
workload/revision primitives before adding storage types. SandboxTemplate has
no public template-build status or build executor, so this does not itself
provide OpenSandbox's tenant catalog and asynchronous create/get/list/delete
flow. Its name alone also does not prove artifact identity: pin the admitted
configuration/revision and artifact content. [Native template type][ag-template-type],
[effective template construction][ag-set-revision]

The catalog's missing status/artifact/ownership metadata and build execution
need a concrete persistence design. Native resources with additional metadata,
a small separate record, and a dedicated CRD are alternatives to compare after
the artifact lifecycle is specified. This proposal does not select ConfigMaps
or build Jobs merely because the public API needs a catalog. Store secrets
separately and artifact bytes outside metadata records.

Template construction must preserve build-produced files and runtime wiring;
recording the original image URI alone is insufficient. Delete removes the
public lookup entry and prevents new admission; artifacts still referenced by
existing sandboxes/builds remain until reference-safe cleanup.

**Verified difference:** the FastSandbox mapping passes the resolved template
artifact as `image` and extensions.poolRef as a separate `pool_ref`. Its pools
prewarm Fastlet Pods that can host multiple compatible artifacts. Agents claim
has a single SandboxSet selector and optional limited image/resource overrides;
it has no equivalent independent artifact-plus-capacity selector.
[Reference mapping][os-template-map], [FastSandbox scheduling][os-template-pool],
[native options][ag-options]

Consolidate the adapter's template/pool resolution into a shared allocation
plan containing the authorized SandboxSet, required artifact/configuration
revision, allowed delivery overrides and allocation policy:

| Input | Resolution into the common path | Constraint to retain |
| --- | --- | --- |
| poolRef only | Resolve pool -> use that set's effective workload -> claim | Pool-defined image/resources and per-delivery command/env behavior |
| templateId only | Resolve successful build artifact -> select/ensure a compatible managed set -> claim | Exact admitted artifact/configuration; public template ID is not an arbitrary set name |
| templateId + poolRef with compatible stock | Resolve both -> verify the selected set and candidate revision -> claim that set | Both the artifact and requested pool; old incompatible revisions cannot satisfy the request |
| templateId + poolRef requiring a different artifact | Evaluate supported per-claim materialization or compatible inventory derived under the selected pool's policy | Current image replacement alone does not reproduce arbitrary build files, mounts, runtime configuration or capacity semantics; this general case remains a design gap |

For example, `(template=t1, pool=p1)` must produce t1's artifact under p1's
allocation constraints. Treating t1 and p1 as aliases, dropping either field,
or retargeting a shared operator-owned p1 for each request does not meet that
requirement. If one public capacity pool must span several physical
SandboxSets, shared admission/accounting needs a concrete design; the native
mapping alone does not establish that requirement or justify a new capacity
controller. All valid combinations remain in scope, including this general
case. The ordinary Pool `"*"` rule is not assumed for template mode.

## Initialization and readiness

Recommended per-delivery sequence:

1. Authenticate, decode, validate source/field combinations and authorize all
   referenced sources. Resolve effective configuration and allocate identity.
2. Resolve/pin artifact and capacity; reserve quota; prepare owned pull secrets
   and volumes; allocate the sandbox with user startup held.
3. Reach the system runtime, install env and fresh credentials, finish mounts,
   enforce requested egress/access protection and confirm effective resources.
4. Execute preStart in the workload execution environment. On success start
   periodic scheduling, then start the user entrypoint with exact argv.
5. Publish the appropriate endpoint/status and return the contract's response.
   SDK endpoint acquisition and health checking use the real execution path.

Runtime listening, Kubernetes Ready, startup-config acceptance, preStart
completion, user-entrypoint launch and SDK health are separate observations.
A compatible health route follows the reference readiness contract, not an
arbitrary TCP-listen test or a synthesized success response. Synchronous
image/Pool delivery can wait through configuration release; an asynchronous
artifact path may expose Pending only after a durable operation exists and
continues after the HTTP request. Public status must describe that operation.

The preferred candidate is an in-sandbox execd-compatible shim backed by
Agents runtime capabilities. The required capability is controlled startup;
whether it is supplied by extending the runtime or adding a supervisor remains
an implementation choice. Keep the RuntimeProvider direction and use the
current runtime client/capability structure where possible.
A concrete feasibility check must cover `/internal/init`-style semantics,
entrypoint process/signal handling, actual health and required SDK endpoints.
Full REST/SSE command/file parity remains separately tracked except where a
create dependency requires it.

At the rechecked upstream revision the shipped agent-runtime Dockerfile still
builds E2B envd from `2025.33`. PR #669 remains open and changes two proposal
documents; the described helper server/router is absent from that revision's
source tree. Its architecture is a reference candidate, not an available
implementation dependency. [Dockerfile][ag-runtime-build], [#669][ag-helper-pr]

Hooks execute per runtime startup using a stable startup generation. API
retries do not rerun them; container restart follows the hook restart contract.
preStart failure/timeout blocks entrypoint. periodic runs after preStart,
prevents overlap for the same name, and normally continues after a failed run;
an execution that cannot be stopped disables subsequent runs for that hook.
Neither initContainer nor livenessProbe alone implements this contract.

## Identity and credentials

Keep three independent data-plane mechanisms and API identity explicit:

| Mechanism | Direction and purpose | Proposed mapping |
| --- | --- | --- |
| API key | Client -> lifecycle API; authenticates tenant/caller | Normalize `OPEN-SANDBOX-API-KEY` with shared KeyStorage, then enforce OpenSandbox authorization in its API layer |
| Secure endpoint token | Client -> sandbox endpoint/gateway when secureAccess is true | Per-delivery token, endpoint headers, gateway validation and revocation |
| Signed endpoint URL | Client -> time-limited endpoint route | Map expiry, signature and route validation as a separate capability; headers alone do not implement a signature |
| execd/runtime token | Client or control plane -> sandbox daemon | Separate runtime credential and endpoint/header translation; rotate on new delivery/clone |
| Egress management token | Client -> policy/credential management endpoint | Protect policy mutation separately from the outbound credential being injected |
| Registry / outbound credentials | Runtime image pull / sandbox -> external service | Separate protected secret references; neither is an endpoint access token |

Native Agents records the claiming Key ID as owner and derives namespace from
Team. Reference tenant scope and same-Team multi-Key access are not equivalent.
This difference is verified in the target sources: TenantEntry holds several
api_keys for one name/namespace, auth resolves the key to that tenant, and
Agents GetSandbox separately compares its persisted owner with the caller's
Key ID. [Tenant record][os-tenant-model], [auth][os-auth],
[E2B caller][ag-user-source], [Manager owner check][ag-owner-source]
Recommended compatibility policy resolves authenticated keys to a stable,
protocol-qualified tenant principal. The API layer validates membership and
scope, then supplies that principal consistently for create and object access;
Manager retains an owner-equality check. Namespace membership alone does not
authorize access and a caller cannot supply another object's owner string.
Originating Key ID remains separate provenance/accounting data. Existing
Manager admission couples these identities through `User`, so separation needs
an explicit neutral option and quota-release design. Native E2B callers keep
their Key owner. Legacy PoC resources need an explicit migration policy rather
than automatic reassignment. This requires Manager/API review and cross-Key
tests before implementation.
Unknown and unauthorized object IDs use the same public not-found behavior.

The compatibility middleware keeps a private context key and the existing
canonical anonymous admin caller for auth-disabled deployments. Explicit
tenant membership and signed-route policy apply when authentication is enabled.
execd `X-EXECD-ACCESS-TOKEN`, Agents HTTP `X-Access-Token` and envd gRPC
`access-token` require transport-specific translation; sharing a token value
does not make their validation paths interchangeable.

For signed endpoints the reference also binds sandbox ID, port and expiry to
its route token. `expires` cannot be combined with server-proxy mode. A native
gateway token is reusable only after those endpoint and verifier semantics are
mapped, not simply because both systems issue tokens. [Endpoint route][os-lifecycle-api],
[signature implementation][os-signature]

`secureAccess=false` means no additional secure-endpoint token requirement,
not that lifecycle authentication, daemon credentials or all deployment-wide
protection disappear. How this works with Agents gateway routing/signing must
be demonstrated for both execd and business ports.

## Failure ownership and cleanup

| Failure boundary | Owner and compensation |
| --- | --- |
| Decode, combination, source authorization | API; no resource side effects |
| Shared template ensure/build | Manager releases this operation's reference; Infra removes only unreferenced owned records/artifacts |
| Pool/clone allocation or initialization | Manager coordinates deletion or validated reset; release admission/capacity after backend acceptance |
| Network, credentials, mount, preStart | Prevent user startup; revoke delivery routes/tokens; delete or quarantine sandbox; remove request-owned dependent resources |
| SDK endpoint/health after create response | SDK uses real delete route; server lifecycle/GC handles abandoned allocations according to durable ownership/deadline policy |
| Deletion or cleanup partially fails | Persist cleanup responsibility, return/log the real failure and retry; do not delete a shared template or pre-existing volume |

Track created-versus-borrowed ownership and reference identities. PVC cleanup
honors `createIfNotExists`/`deleteOnSandboxTermination`; a pre-existing PVC is
never treated as request-owned. Cancelled requests use bounded cleanup contexts
and do not leave background goroutines as the sole owner of durable operations.

Errors are translated per OpenSandbox route and source. Do not reuse the E2B
status map wholesale: missing snapshot/template (404), unready snapshot (409),
capacity exhaustion (429 with Retry-After), provisioning timeout (504), quota
(403) and invalid input have different meanings. Preserve the OpenSandbox
error envelope and stable machine-readable codes; exact validation envelopes
and provider-specific divergences are part of the contract fixtures.

## Decisions for review

The image direction in D01 is selected. Source investigation resolves the
capability/mapping facts in D02/D03/D08/D09; the table distinguishes those facts
from implementation choices. All 16 fields remain in the acceptance scope
while a choice is pending. A verified source mapping is not a runtime pass.

| ID | Recommendation | Consequence / alternative still requiring review |
| --- | --- | --- |
| D01 | Selected direction: configuration/revision-compatible virtual templates with image cold fallback | Tenant scoping, configuration index, tag freshness, exact claim API, manual-pool adoption and GC remain detailed policies for review |
| D02 | Verified: ordinary poolRef maps to SandboxSet/ClaimSandbox; template-plus-pool has no direct independent native selector. Consolidate their resolution into shared allocation and reuse SandboxTemplate/templateRef | Exact artifact binding/build state and the general multi-artifact capacity case still need a minimal design. A new capacity layer or ConfigMap catalog is not selected |
| D03 | Verified: hooks require controlled startup; claim env does not modify the main process; clone inherits old init data. Apply current config and fresh delivery credentials before user execution | Runtime extensions versus a supervisor and the clone override interface remain choices. #669 is a proposal dependency only, not available helper code |
| D04 | Omitted/null networkPolicy adds no request policy; present empty policy defaults deny | OpenAPI prose says empty allows all, while reference execution denies; recommend deny with explicit compatibility documentation and upstream clarification |
| D05 | OpenAPI metadata string values persisted as isolated annotations; env values follow the wire string map | Accepting spaces in metadata differs from reference label validation; null env values need an explicit validation fixture, not Go zero-value coercion |
| D06 | Requests default to limits only for absent/empty whole map; provided map applies independently | Correct reference builder's requests-only discard when limits is empty; document exact behavior and validate K8s constraints |
| D07 | Implement network enforcement/credential proxy and storage backends needed by the complete fields | Ordered rules, transparent HTTPS injection/CA trust, host/OSSFS permissions and Windows runtime profile are capability gates, not automatic deferrals |
| D08 | Verified: reference tenants can contain several keys; native ownership is per Key ID. Recommend stable OpenSandbox tenant ownership with separate provenance/accounting and unchanged E2B policy | Principal mapping, legacy resources, quota release and gateway/daemon/signature integration require implementation review |
| D09 | Verified: the public API requires 202/state convergence; native pause persists intent and then waits. Preserve that public contract in the independent lifecycle plan | Review a neutral submit/wait split and observable transition mapping; changing the public scope requires a separate explicit decision, not an inference from native synchronous waiting |

D01–D03 determine the main execution design. D04–D06 settle observable contract
conflicts. D07–D08 require concrete execution/authorization designs and tests;
this proposal does not claim those capabilities exist. D09 is not a reason to
remove any create field or endpoint/health dependency.

## Implementation and validation plan

### Create work and dependent follow-ups

| Package | Changes | Completion evidence |
| --- | --- | --- |
| W1: wire and contract | F01–F16 models, missing/null/empty fixtures, source selection, errors and responses | Table-driven HTTP contract cases with no side effects on invalid input |
| W2: image and ordinary Pool | Managed template ensure, compatibility/revision selection, bounded allocation/cold fallback | Actual image/config; concurrency, cancellation, stock miss and capacity tests |
| W3: snapshot and explicit template | PR1 recovery/source overrides, revision binding and template-plus-capacity; public catalog/build APIs are separate dependencies | Restored/build-produced files, current startup config, source permissions, restart recovery |
| W4: startup and dependencies | Runtime barrier/hooks/argv/env, resources/platform, storage, network and three credential paths | First user action sees intended config; failed prerequisites prevent it |
| W5: dependent API work | PR2 describe/delete and lifecycle routes; subsequent endpoint/health and public template/credential operations | Official SDK create and create_from_template with real endpoints, cleanup on failure |
| W6: reconciliation | Align proposal to final interfaces and observed results; E2B regressions | Fixed SHAs/configs, reproducible evidence and no unresolved acceptance gaps |

W1–W4 describe create behavior and its internal execution needs. W5 is dependent
work outside PR1's public route surface; PR2 must always include PR1's latest
changes. W6 and the complete SDK checks cover the combined delivery.

### Behavioral validation

Use the [field-level cases](./20260918-opensandbox-create-field-comparison.md)
as the checklist. Each case records reference SHA/provider, Agents SHA,
image/artifact digest, request/response, actual resource/process/network/file
effects and cleanup. Distinguish static inspection, unit tests and integration
results. This revision adds documentation only and records no new runtime pass.

- All source paths: image, snapshot, ordinary Pool named/automatic, template
  alone and template-plus-pool; compare warm and cold behavior where applicable.
- Missing/null/empty and invalid combinations; nested types and quantities;
  schema-required response keys; metadata round-trip and filtering.
- First-entrypoint env/argv/mount/resources; preStart failure/timeout/restart;
  periodic overlap/cancellation; no early user execution during prewarm.
- Network omitted/empty/ordered conflicts, DNS/IP/wildcards; secure access
  valid/missing/wrong token; signed route expiry; clone and reuse token isolation.
- Real outbound credential injection, HTTPS trust and secret separation; host,
  PVC and OSSFS mounts; ownership-sensitive rollback and deletion.
- Concurrent ensure/claim/delete, stale revisions, mutable tags, build failures,
  quota/capacity exhaustion, API cancellation and Manager restart.
- SDK endpoint/health budget and failure deletion, including template origin
  and egress routing. A fake endpoint or skipped health is not this test.

Implementation validation uses focused unit/package tests. The behavior matrix
requires explicit integration runs as a separate acceptance activity; it is
not a request to run the repository's full E2E suite for a documentation edit.

Independent follow-ups retain describe/list/pause/resume/renew/metadata and
command/file compatibility beyond the subset needed by create. Async pause's
public contract is established; its submit/wait implementation remains to be
reviewed under D09. Signed endpoints must be classified by
the advertised endpoint contract rather than silently omitted.

## Compatibility and rollout

Keep the existing opt-in compatibility flag. Add neutral per-call options so
native E2B callers preserve their current defaults, owner policy and allocation
behavior. Avoid global switches for source-specific behavior.

Treat existing aliases as migration inputs only after validating their actual
configuration, ownership and runtime readiness. Unconfigured aliases no longer
block a legal image request once virtual templates are implemented. Do not
adopt, mutate or garbage-collect operator-owned pools merely because an alias
points at them.

Version managed records/startup configuration so older managers fail clearly
rather than misinterpreting a new record. A rollout must exercise mixed
versions, in-flight builds/deliveries and rollback: disabling the adapter stops
new public requests but existing owned resources still have a cleanup owner.
Default-off operation and unchanged E2B behavior are regression gates.

## Alternatives

- **Static aliases as the final create design:** cannot express arbitrary legal
  images/configuration or template/artifact semantics; retain only migration
  or operator-managed integration use.
- **Independent image create path:** simpler cold provisioning but diverges from
  the virtual-template direction; keep cold creation within the shared managed
  template/allocation path unless review changes that choice.
- **Direct API writes to SandboxSet/Pod or reuse of E2B handlers:** conflates
  protocol defaults and backend orchestration; retain API -> Manager -> Infra.
- **initContainer for preStart / livenessProbe for periodic:** execution
  environment, timing, failure and overlap behavior differ; use runtime startup
  and scheduling capabilities.
- **Header rewriting as credentialProxy:** does not provide protected credential
  binding, transparent HTTPS interception and CA trust; evaluate the full path.
- **Separate Lifecycle Server or SDK fork:** duplicates the control plane or
  client maintenance; use the public API with Agents execution.

## Implementation history

- 2026-09-18: Initial proposal and static-alias create work started.
- 2026-09-20: Proposal #990 and implementation #989 submitted; historical
  low-level SDK create PoC completed. Full SDK create was not established.
- 2026-09-27: Revised for the complete create contract, source/runtime design,
  explicit template mode, field-by-field server comparison and review decisions.
  Reverified pool/template, snapshot, runtime, pause and identity questions
  against official docs and upstream `1015db4`; narrowed proposed additions
  to demonstrated gaps and removed the assumed new capacity/catalog layer.
  The implementation baseline remains `8771012`; validation is pending.

[issue]: https://github.com/openkruise/agents/issues/690

[ag-clone-restore]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/sandbox-manager/infra/sandboxcr/clone.go#L267-L497
[ag-init]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/utils/runtime/init.go#L39-L94
[ag-options]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/sandbox-manager/infra/types.go#L45-L137
[ag-template-selection]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/sandbox-manager/infra/sandboxcr/claim.go#L664-L708
[compat-create]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/opensandbox/create.go#L74-L349
[compat-route]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/opensandbox/routes.go#L95-L113
[os-spec]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/specs/sandbox-lifecycle.yml

[ag-checkpoint-type]: https://github.com/openkruise/agents/blob/1015db48209922e18de347198980b98f88a9a188/api/v1alpha1/checkpoint_types.go#L115-L146
[ag-claim-full]: https://github.com/openkruise/agents/blob/1015db48209922e18de347198980b98f88a9a188/pkg/sandbox-manager/infra/sandboxcr/claim.go#L595-L889
[ag-claim-manual]: https://openkruise.io/kruiseagents/user-manuals/sandbox-claim
[ag-ext]: https://github.com/zhuangzhewei09/agents/blob/8771012c3e6e81550888931ab522e9d865f8ab00/pkg/servers/e2b/models/extensions.go
[ag-helper-pr]: https://github.com/openkruise/agents/pull/669
[ag-init-options]: https://github.com/openkruise/agents/blob/1015db48209922e18de347198980b98f88a9a188/pkg/utils/runtime/config/types.go#L27-L32
[ag-owner-source]: https://github.com/openkruise/agents/blob/1015db48209922e18de347198980b98f88a9a188/pkg/sandbox-manager/api.go#L217-L258
[ag-pause-source]: https://github.com/openkruise/agents/blob/1015db48209922e18de347198980b98f88a9a188/pkg/sandbox-manager/infra/sandboxcr/sandbox.go#L511-L566
[ag-pool-manual]: https://openkruise.io/kruiseagents/user-manuals/warmpool-management
[ag-runtime-build]: https://github.com/openkruise/agents/blob/1015db48209922e18de347198980b98f88a9a188/dockerfiles/agent-runtime.Dockerfile
[ag-set-revision]: https://github.com/openkruise/agents/blob/1015db48209922e18de347198980b98f88a9a188/pkg/controller/sandboxset/revision.go#L36-L73
[ag-set-types]: https://github.com/openkruise/agents/blob/1015db48209922e18de347198980b98f88a9a188/api/v1alpha1/sandboxset_types.go
[ag-template-type]: https://github.com/openkruise/agents/blob/1015db48209922e18de347198980b98f88a9a188/api/v1alpha1/sandboxtemplate_types.go
[ag-user-source]: https://github.com/openkruise/agents/blob/1015db48209922e18de347198980b98f88a9a188/pkg/servers/e2b/sandbox.go#L44-L117
[os-auth]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/middleware/auth.py#L101-L135
[os-lifecycle-api]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/api/lifecycle.py
[os-signature]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/components/ingress/pkg/signature/signature.go
[os-snapshot]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/snapshot_restore.py#L29-L105
[os-template-map]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/services/fast_sandbox/create_mapping.py#L155-L207
[os-template-pool]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/docs/architecture/fast-sandbox/scheduling.md#L7-L30
[os-tenant-model]: https://github.com/opensandbox-group/OpenSandbox/blob/f59755922d92b4e98df805ed7fd4861bc2a38f21/server/opensandbox_server/tenants/models.py
