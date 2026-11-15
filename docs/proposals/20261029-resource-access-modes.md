# Compatible Resource Access Modes

## Contract

The operator selects `--resource-access-mode=trusted-domain|isolated` at startup.
The default is `trusted-domain` for compatibility with existing deployments.
This is a resource-sharing policy, not a switch that disables API authentication.
Mutually untrusted customers holding API keys must not share an unrestricted
trusted-domain deployment.

Existing legacy CSI constructors, enrichment hooks, storage-auth JSON and API
behavior remain unchanged in trusted-domain mode. Bare/shared PVs, request
attribute overrides and the legacy inner policy projection remain available.
No automatic migration or retroactive validation of existing deliveries occurs.

Snapshot ownership and delivery identity use the existing nonempty owner:
API-key ID for E2B and Claim UID for the standalone SandboxClaim controller.
This does not redefine existing API ownership as Team membership. Snapshot
sharing grants and a new tenant identity service are outside this change.

Isolated snapshot consumption checks the source owner before reading its paired
template or creating the clone. Dynamic CSI uses shared, platform-controlled PVs
as mount templates. It does not use PVCs, ClaimRef, Bound state or a PV owner as
customer authorization. Isolated CSI validates the complete batch and permitted
dynamic selectors, then derives requested identity scopes from that exact final
mount context. The existing IdP/storage service independently authorizes the
customer for the selected identity/provider, resource and operations. Local
scope construction and successful token issuance alone are not entitlement proof.

## Shared PV Authorization

The deployment owner has confirmed that customers share PVs and customer resource
authorization is performed by the existing IdP/storage service. The external
authorization implementation is not in this repository; this is a deployment
prerequisite, not a new local authorizer or a claim of independently verified
cloud-side isolation.

```text
authenticated delivery namespace and owner
  -> shared platform PV (no PVC required)
  -> merge permitted resource selectors and validate final scope
  -> persist Sandbox identity and issue its agent token
  -> IdP/storage checks customer entitlement during issuance/exchange
  -> CSI mounts only with the permitted resource credentials
```

The standard enterprise token request carries Sandbox namespace, name and UID,
not a dedicated API-key owner field. The service must establish the real customer
from a trusted lookup or its own authenticated mapping. It must not use a
caller-selected agent-name, credentialProviderName or resource scope as proof of
ownership. Permission must be an intersection with independently granted access;
the requested metadata must not create permissions by itself.

Isolated shared-PV mounts require `authType=agent-identity`. Shared static Secret
credentials do not identify the individual customer, so they are rejected before
any credential read. Neither scope preparation nor execution preparation reads
PVCs or Secrets. An agent-identity PV's unused NodePublishSecretRef is ignored,
as on the legacy identity path. Trusted-domain static mounts remain unchanged.

Only selectors represented in the scoped credential contract can vary:

| Driver | Dynamic request attributes |
| --- | --- |
| OSS | `credentialProviderName`, `sandboxCredProviderName`, `bucket`, `bucketSpace`, `bucketSpacePrefix`, `kmsKeyId`, `region` |
| NAS | `credentialProviderName`, `sandboxCredProviderName`, `server` (validated access-point hostname) |

Provider aliases must agree after explicit selection. SubPath remains derived
from the PV base path, and existing KMS/readonly restrictions cannot be silently
disabled. Reserved fields remain prohibited; other attributes may only echo an
identical PV value, not introduce or alter endpoint, credential or unknown
driver behavior. Unsupported driver-specific authorization schemas fail closed.

External authorization may require the persisted Sandbox UID. Therefore local
format, scope, provider and encoding preflight precedes quota/persistence, but a
real entitlement denial can occur during token issuance or the storage exchange.
Issuer rejection prevents token propagation and mounts; refresh rejection does
not advance expiration. Per-mount exchange or mount failures do not make the
runtime batch transactional, and no rollback of all mount effects is promised.
For isolated ID-token issuance, an HTTP 403 from the enterprise IdP is classified
as terminal access denial. Claim/clone must not repeatedly provision Sandboxes
for that deterministic rejection. The trusted/unmarked path, access-token path
and other HTTP statuses retain their existing error handling and diagnostic text.

## Policy Lifetime

New deliveries are stamped with platform-owned
`agents.kruise.io/resource-access-policy` (`trusted-domain-v1` or `isolated-v1`).
Request metadata cannot supply this marker. Recycled controller-managed pool
objects lose the old delivery marker only on successful reset.

Refresh, resume/recreation and recovery read the persisted marker, not today's
process default. Unmarked existing objects retain legacy behavior; present but
empty or unknown markers fail closed. Clones receive a newly admitted delivery
policy rather than blindly copying the source policy. Isolated source snapshots
retain their use restriction even when consumed by a compatible-mode Manager.

Upgrade all lifecycle consumers before enabling isolated admission. An older
binary that ignores markers is not a safe rollback for isolated deliveries.
Direct Kubernetes writers able to alter owners, policy markers or shared PVs
remain privileged: API-key-only isolation does not protect against such writes.
RBAC/admission must protect that state in Kubernetes-facing multi-tenant setups.

## Implementation Boundaries

Mode values and marker encoding live in a neutral package with no clients,
feature gates or Manager/controller dependencies. Startup options are immutable
instance configuration and become per-call operation options. New isolated CSI
backend reads occur inside Infra after Sandbox selection; the legacy API eager
path is retained unchanged. Existing controller-to-Infra dependency debt is not
expanded with new Manager policy imports.

## Acceptance

- Default and explicit trusted-domain behavior match the pre-hardening baseline,
  including inner hooks and persisted input formats.
- Unknown configured modes fail startup; unknown persisted policies fail before
  credential issuance or mount execution.
- New isolated claims/clones, including admin calls in non-default namespaces,
  perform snapshot admission and local CSI preflight before persistence and quota
  acquisition; candidates rejected by local preflight remain usable. External
  authorization is enforced during the identity/storage service interaction.
- Missing delivery identity, deleting/invalid PVs, static authentication, invalid
  scopes and forbidden attribute changes fail local preflight without PVC or
  Secret reads. Read-only restrictions are kept; external denials cannot become
  successful credentials or a fallback to shared static authentication.
- Empty isolated mount sets remove stale storage-auth metadata. Refresh performs
  no static Secret reads or mounts and does not advance expiration on failure.
- Existing unmarked objects and explicitly compatible deliveries are not
  reinterpreted when a process default changes.
- Tests cover community/static storage and inner/agent-identity behavior, clone
  inheritance/override, standalone claim, initialization, refresh and recovery.

The runtime redirect and peer-refresh Aone findings are separate workstreams;
this change neither fixes them nor disables their transport protections.

## Configuration

Add the selected option to each process's existing command line:

```text
sandbox-manager:           --resource-access-mode=trusted-domain
agent-sandbox-controller:  --resource-access-mode=trusted-domain
```

Omitting the option has the same compatible default. Explicitly empty or unknown
CLI values are rejected before Kubernetes setup. Programmatic zero-value options
remain compatible with existing constructors.

For isolated new API deliveries, configure the Manager with:

```text
--resource-access-mode=isolated --e2b-enable-auth=true
```

For isolated new standalone SandboxClaim deliveries, also configure that
controller with `--resource-access-mode=isolated`. This controller setting governs
its own new claims, not the policy of existing Sandboxes created by the Manager.
All lifecycle binaries must nevertheless understand the marker before either
producer starts creating isolated deliveries.

The Manager rejects isolated mode when E2B authentication is disabled. Snapshot
authorization still checks the actual caller's owner identity, including admin
keys. CSI passes the actual selected Sandbox namespace and its trusted delivery
identity into the existing identity lifecycle; it does not assign shared PVs to
one namespace or key. Static CSI Secrets retain their existing system-namespace
behavior in trusted-domain mode only. No PVC or Secret informer widening is
needed for isolated shared-PV operation.

## Compatibility Matrix

| Behavior | trusted-domain / existing unmarked deliveries | isolated new deliveries |
| --- | --- | --- |
| Snapshot sharing accepted by the baseline | Preserved for legacy/trusted sources | Same nonempty owner required; explicit sharing grants are not implemented |
| Bare/shared PV without a PVC | Baseline behavior preserved | Supported as an agent-identity mount template; customer entitlement is checked by the IdP/storage service |
| Ordinary per-request attributes | Existing passthrough and overrides preserved | Scoped OSS/NAS resource selectors are dynamic; other fields must match PV defaults |
| Legacy inner hooks and metadata | Original hooks, eligibility, NAS parser and sub-path JSON retained | Separate validated projection, with final OSS path and complete NAS AP hostname |
| Read-only mode | Baseline behavior preserved | Combines request, CSI PV and PV access-mode restrictions; no PVC dependency |
| Static Secret authentication | Baseline behavior preserved | Rejected for shared-PV customer isolation; no fallback from agent identity |
| Restore/refresh after changing a process flag | Original accepted policy retained | Persisted isolated policy retained |
| Explicit empty clone mount list | Baseline semantics retained | Clears inherited mounts and stale storage-auth scope |
| Resource resolution and batch encoding | Baseline execution order retained | Complete local preflight before quota/persistence; real entitlement decisions remain in the IdP/storage service |

New deliveries add an internal versioned annotation; public request schemas,
legacy mount annotation encoding and token-provider interfaces are unchanged.
The marker is hidden by the existing internal-metadata filtering. New isolated
authorization denials use generic HTTP 404; compatible-mode error mappings remain
unchanged. Existing informer filtering and unrelated PVC-backed volume APIs are
unchanged; isolated CSI no longer reads PVCs or expands their cache visibility.

Enabling isolated mode is an explicit policy migration for new admissions:
requests that reference old ownerless snapshots, static-credential PVs or
unsupported dynamic fields can be rejected. A shared/unbound PV is not itself a
reason for denial. This is not an automatic tenant-grant migration. Existing unmarked
running Sandboxes are not silently revalidated or revoked by changing the flag.
Use a customer-reviewed rollout rather than describing this flag as a complete
retroactive fix for all legacy resources.

## Recovery CLI Limits

The existing recovery CLI does not configure a runtime TLS bundle or register an
enterprise identity provider. Its compatible/unmarked path remains unchanged.
For marked isolated Sandboxes it preflights the full shared-PV batch. Nonempty
isolated mounts require agent identity, so the stock CLI rejects recovery that
requires identity issuance or runtime TLS rather than using static credentials,
downgrading transport or minting a placeholder token. Provider-success unit tests
use test injection, not a newly implemented CLI provider configuration path.

Use the appropriately configured controller's recovery/reinitialization path for
isolated agent-identity/TLS workloads until the CLI gains explicit configuration
for those capabilities. `--skip-csi-mount` does not read static Secrets or require
a local driver registry; both skip flags together perform no recovery effects.

## Validation

Regression tests retain the original compatible-mode assertions and separately
exercise isolated admission, source protection after a default-mode change,
metadata reconstruction, snapshot overrides, shared-PV use across customers,
zero PVC/Secret reads, count limits, binary publish-context encoding failures,
provider readiness and local-preflight denials. External issuer-denial tests
verify that no token is propagated or mount attempted and refresh does not
advance expiration. Real loopback IdP HTTP-adapter tests verify isolated 403
classification and unchanged trusted/other-status behavior. Tests also cover nil clients, absent/invalid policy markers,
recovery skip flags, standalone recycle cleanup and legacy inner behavior. Unit
and race tests do not replace real CSI/IdP or customer-data rollout validation.

This corrects the in-flight PVC-based isolated design. If that earlier design
has already been deployed with isolated static-credential deliveries, this
revision must be treated as a policy migration for those objects, not an
automatically compatible replacement. Unmarked/trusted deliveries are unchanged.

## Scope Preparation

`GenerateIsolatedMountOptionsWithScope` returns the validated mount requests and
the freshly computed storage-auth JSON. Admission persists that returned scope
before creating/claiming the Sandbox instead of projecting it again. The original
`GenerateIsolatedMountOptions` signature remains available as a wrapper.

Provider-returned protobufs are deep-copied before validation and storage, so
reusing or changing a provider-owned request cannot mutate an earlier result.
The final driver, target, VolumeContext, read-only and static-credential checks
remain authoritative; a second identical JSON projection is unnecessary.

At issuance, `PrepareSandboxToken` still rebuilds scope from supplied mounts and
overlays it onto a copy of the current Sandbox. This deliberately preserves the
latest UID, runtime credentials and status after create/init; it does not reuse
a pre-create Sandbox or trust an old storage-auth annotation. The returned mounts
are mutable caller-owned values, not a cached authorization capability.

| Isolated path | Previous scope projections | Current scope projections |
| --- | --- | --- |
| Claim/clone with ID-token issuance | 4 | 2: preflight and issuance |
| Initialization/recovery with ID-token issuance | 4 | 2: preflight and issuance |
| Refresh without a supplied mount snapshot | 2 | 1: PV-only preparation |

These are local projection counts, not IdP calls or measured latency gains. There
is no process-global or context cache. `nil` mounts mean re-resolve persisted
intent; a non-nil empty list means clear stale scope without re-reading it.

## User Requests

Native `POST /sandboxes` and `POST /v2/sandboxes`, and their
`/kruise/api/...` equivalents, use the same API-key authentication. The simple
`volumeMounts` form only supplies PV name and in-container path:

```json
{
  "templateID": "replace-template",
  "metadata": {
    "security.agents.kruise.io/agent-name": "replace-authorized-agent"
  },
  "volumeMounts": [
    {"name": "shared-oss-template", "path": "/data"}
  ]
}
```

For subpaths, read-only flags or dynamic selectors, use the existing multi-mount
metadata extension. Its value is a JSON array encoded as a string:

```json
{
  "templateID": "replace-template",
  "metadata": {
    "security.agents.kruise.io/agent-name": "replace-authorized-agent",
    "e2b.agents.kruise.io/csi-volume-config": "[{\"pvName\":\"shared-oss-template\",\"mountPath\":\"/data\",\"subPath\":\"customer-a\",\"readOnly\":true,\"attributes\":{\"credentialProviderName\":\"replace-authorized-provider\",\"bucket\":\"replace-authorized-bucket\"}}]"
  }
}
```

Send `Content-Type: application/json` and the existing `X-API-Key` header. Do not
send a top-level `extensions` object, internal resource-access-policy, or generated
storage-auth metadata. Avoid mixing input forms: legacy single-mount metadata
takes precedence over multi-mount metadata, which takes precedence over
`volumeMounts`. Selecting an agent/provider or resource requests access; it does
not grant access.

On an isolated clone, omitted mounts inherit the checkpoint's intent. Explicit
`volumeMounts: []` or extension value `"[]"` clears it. This does not detach a
volume from an existing running Sandbox. Standalone SandboxClaims use
`spec.dynamicVolumesMount` and still need independently authorized agent identity.

## Administrative Operations

There is no PV-to-tenant grant API or grant CRD in this repository. The `/volumes`
routes are disabled, and the local `AgentRole` action enum covers principal/OAuth2
token exchange, not CSI mount or storage STS grants. Do not create a fictional
`AgentRoleBinding` or PV owner annotation as a storage authorization mechanism.

Administrators must perform two separate tasks:

1. Provision protected shared PV templates, install the required driver/runtime,
   configure identity-provider transport and enable the chosen access mode.
   `DYNAMIC_STORAGE_DRIVER_LIST` registers exact comma-separated driver names on
   the manager/controller processes; it does not install plugins or grant access.
2. In the existing external authorization system, bind the real customer identity
   to permitted agent identities/providers, backend targets/prefixes/access points,
   KMS/region constraints and operations. That system's actual API/CLI and schema
   must come from its own implementation/runbook, not this proposal.

The optional admin key-creation endpoint accepts `name`, `teamName` and `quota`;
creating a key only establishes API identity and quota, not storage permissions.
An administrator key is not a storage-authorization bypass. Neither Team membership
nor a shared PV name proves entitlement to a backend object.

Mount `readOnly` does not automatically create read-only cloud credentials: the
current storage-auth JSON contains no operation-permission field. Configure the
appropriate provider/storage grants as well as the mount restriction. Validate
both positive and cross-customer negative cases using the actual IdP/storage
service before exposing isolated shared storage to customers.
