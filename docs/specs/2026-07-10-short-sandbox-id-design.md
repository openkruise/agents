# Short Sandbox ID Design

- Status: Approved in discussion, pending revised written-spec review
- Date: 2026-07-10
- Last revised: 2026-07-14
- Scope: `sandbox-manager`, its generic infra extension points, API/controller reserved-key enforcement, sandbox cache/routing consumers, and E2B presentation

## 1. Background

The current sandbox ID is calculated as:

```text
<namespace>--<sandbox-name>
```

This value is readable and reversible, but its length grows with both the namespace and Sandbox name. E2B-compatible traffic addresses include the sandbox ID in a DNS name, for example:

```text
<port>-<sandbox-id>.<domain>
```

Long namespaces and names can therefore make a DNS label or the full domain too long to resolve or access.

The sandbox ID is a sandbox-manager domain concept. Infrastructure implementations must not decide its format, and the E2B server must only present the ID selected by sandbox-manager core.

The ID identifies a Sandbox CR, not an individual claim or tenant session. A recycled CR keeps its identity while its owner and claim lifecycle may change.

## 2. Goals

1. Provide a short, stable ID for newly claimed or cloned Sandboxes when the feature is enabled.
2. Preserve compatibility for existing unlabeled Sandboxes by continuing to resolve their legacy ID.
3. Ensure one Sandbox uses one ID at any instant; do not expose simultaneous legacy and short aliases.
4. Allow an unlabeled recycled Sandbox to transition from a legacy ID to a short ID on a later claim.
5. Remove manager/gateway binary rollout-order coupling while the feature remains disabled.
6. Make a short ID directly searchable with `kubectl`.
7. Restore namespace/name observability in E2B success metadata and authorized error responses.
8. Keep ID format policy and label mutation in `pkg/sandbox-manager` core while exposing only the reserved metadata-key constant to API validation and recycle code.
9. Reject user-controlled or internal-callback mutation of the reserved label before it can reach cache or routing state.
10. Fail closed when one sandbox ID is observed on more than one Sandbox instead of selecting an arbitrary object or route.

## 3. Non-goals

1. Supporting both legacy and short route aliases for the same Sandbox.
2. Migrating all existing Sandboxes in a background job.
3. Rewriting IDs stored on existing Checkpoint resources.
4. Making sandbox IDs reversible back to namespace/name.
5. Removing the current namespace restriction on `--` while unlabeled legacy Sandboxes remain supported.
6. Validating or repairing a non-empty persisted sandbox-ID label while reading it.
7. Preventing a cluster administrator with direct Sandbox write permission from manually setting a non-empty sandbox-ID label.

## 4. Persisted State and Resolution Rules

### 4.1 Label

The selected short ID is persisted on the Sandbox as:

```text
agents.kruise.io/sandbox-id=<short-id>
```

In the normal manager flow, only Sandboxes assigned a short ID have this label. Legacy Sandboxes do not need a marker. If a non-empty value is introduced through an operational action, all readers still treat it as authoritative.

This is a label rather than an annotation so operators can locate a Sandbox directly:

```shell
kubectl get sbx -A -l agents.kruise.io/sandbox-id=<short-id>
```

The same qualified key already exists as `AnnotationSandboxID` on Checkpoints. Reusing the string is intentional and does not merge the Kubernetes label and annotation maps:

- the Sandbox label is its current authoritative ID;
- the Checkpoint annotation is a historical snapshot of its source Sandbox ID at Checkpoint creation.

Readers never fall across metadata kinds. A Sandbox annotation with this key does not affect resolution, and a Checkpoint label with this key does not affect snapshot association. Code uses distinct label and annotation constants so callers select the intended metadata map explicitly.

`api/v1alpha1` declares `LabelSandboxID` as the schema-level name of the reserved Sandbox label, alongside the existing Checkpoint annotation constant. This API constant does not own resolution, generation, assignment, or migration policy. Those decisions remain in `pkg/sandboxid`; the API-level constant exists only so controller validation and recycle code can protect the persisted field without importing a manager-domain package.

The label is core-owned metadata. Every user-controlled path that can affect Sandbox labels must prevent it from being supplied:

- E2B rejects the exact `label:agents.kruise.io/sandbox-id` extension key during extension parsing, before claim or clone options and `UpdatedMetadataInClaim` are built. Ordinary E2B metadata remains covered by the existing internal-prefix blacklist;
- SandboxClaim reconciliation rejects `spec.labels[agents.kruise.io/sandbox-id]` as a deterministic validation error before building options or invoking infra;
- SandboxSet and SandboxTemplate inheritance continues stripping non-preserved `agents.kruise.io/` labels while materializing a Sandbox. The reserved ID label is explicitly tested as a key that is never preserved from a pool/template;
- any future public API that copies user labels into Sandbox `ObjectMeta.Labels` must reject this exact key at its input boundary.

Input validation is not the only protection. Sandbox-manager core guards both the existing pre-lock `Modifier` and the final `PostModifier`: it snapshots the exact key's presence and value before a caller callback, invokes the callback, and fails the operation if the callback added, changed, or deleted the key. The modified object is discarded and is not persisted. This guard applies whether short assignment is enabled or disabled. Core's own final `AssignShort` call is the only callback allowed to create the label.

Recycle provides an independent persistence boundary. The reserved key is excluded when `UpdatedMetadataInClaim` is built, and `resetMetadataForPool` skips it even if a historical or manually crafted `UpdatedMetadataInClaim` annotation lists it. A labeled Sandbox therefore cannot return to legacy resolution through metadata cleanup.

Direct writes to Sandbox objects by a cluster administrator remain operational actions and are trusted by the read path, consistent with the absolute-fact rule. Global ID uniqueness is therefore an operator-maintained invariant. Cache lookup and Route Store collision handling both fail closed rather than selecting or overwriting an arbitrary object with a duplicate manually assigned value.

### 4.2 Resolution

ID resolution has exactly two branches:

1. If the label value is non-empty, return it unchanged.
2. If the label is absent or empty, calculate `<namespace>--<name>`.

A non-empty label is an absolute persisted fact. Readers do not validate its alphabet, length, relationship to the UID, or whether it was generated by the current version. This avoids different components disagreeing about a Sandbox whose state has already been persisted.

Detecting that two objects claim the same opaque value is not read-time value validation: neither value is rewritten or rejected as malformed, but ambiguous lookup/routing fails closed.

The empty-label case is intentionally equivalent to an absent label. Legacy behavior does not delete the label; it simply applies when no non-empty value exists.

### 4.3 One-way State Transition

The state machine is:

```text
unlabeled/legacy --successful short assignment--> labeled/short
labeled/short   --all later operations---------> labeled/short
```

Disabling short-ID assignment does not remove an existing label and does not make a labeled Sandbox legacy again.

An unlabeled Sandbox may be returned to a pool after a legacy claim and receive a short ID during a later claim when the feature is enabled. Recycle therefore does not guarantee that an unlabeled Sandbox keeps its legacy ID forever.

At every observed resource version, however, resolution returns exactly one ID. There is no period in which both IDs are valid aliases.

Because recycle preserves the Sandbox CR, the ID is not unique per claim or across time. Authorization, metrics, and external caches must not infer the current owner or claim session from the ID alone.

## 5. Short-ID Encoding

For a Sandbox with Kubernetes UID `U`, short assignment performs:

1. Parse `U` as its 16 UUID bytes.
2. Encode the 16 bytes with RFC 4648 Base32.
3. Remove padding.
4. Convert alphabetic characters to lowercase.

The result is 26 characters using `[a-z2-7]`.

Example shape:

```text
n6lyz2y2m5g3fbbq4rq6r5kpte
```

The complete 128 bits are retained; the value is not truncated. With a five-digit port, the E2B dynamic-host first label is at most 32 characters:

```text
<5 digits>-<26 characters>
```

If short assignment is requested but the UID cannot be decoded as 16 UUID bytes, assignment fails. This check exists only while generating a new value. Once a non-empty label exists, read-time resolution trusts it without validation.

## 6. Configuration

Sandbox-manager exposes:

```text
--enable-short-sandbox-id=false
```

The flag controls assignment, not read format:

- Disabled: do not assign a label to an unlabeled Sandbox.
- Enabled: after a successful claim or clone, assign a short ID if the label is empty.
- In either state: always return an existing non-empty label.

This distinction is required for safe one-way migration. A labeled Sandbox remains usable after the flag is turned off, as long as all running binaries understand label-based resolution.

## 7. Ownership and Package Boundaries

### 7.1 Core package

Create a responsibility-specific top-level leaf package:

```text
pkg/sandboxid/
```

It owns:

- the semantic use of the reserved Sandbox label;
- legacy fallback calculation;
- UID-to-Base32 encoding;
- short-ID assignment behavior;
- the rule that a non-empty label is authoritative.

Its required operations are:

```go
const LabelKey = agentsv1alpha1.LabelSandboxID

func Resolve(sandbox metav1.Object) string
func Legacy(namespace, name string) string
func GenerateShort(uid types.UID) (string, error)
func AssignShort(sandbox metav1.Object) (changed bool, err error)
```

`Resolve` is the only label-or-legacy decision point. `Legacy` exists for the injected mixed-version NotFound fallback and is not used to reverse or validate client IDs. Exact function type aliases used for dependency injection live with the neutral consumer, so those packages do not import this leaf package merely for a type.

Manager and gateway composition roots may call the leaf resolver and inject it into neutral mechanisms. `pkg/cache`, `pkg/utils/proxyutils`, `pkg/sandboxroute`, and infra must not import `pkg/sandboxid` or reproduce the `label-or-legacy` branch locally.

### 7.2 Sandbox-manager core

`SandboxManager` owns the feature flag and decides whether to compose the short-ID assignment callback into claim and clone operations.

Core wraps every caller-supplied pre-lock `Modifier` and final `PostModifier` with the reserved-label guard from Section 4.1. The existing pre-lock modifier becomes error-returning so a guard failure stops before the first persistence. The guard compares both map-entry presence and value; deleting an existing empty entry is therefore also rejected rather than treated as unchanged.

If a caller supplies a generic final post modifier, composition order is:

1. guarded caller post modifier;
2. core short-ID assignment.

The composed callback reports `changed=true` when either callback changes the fresh object and stops immediately on the first error.

The guard is installed even when assignment is disabled. Core runs last and fills the key only when empty; `AssignShort` remains a no-op when the label is already non-empty. A guard rejection is a programming/input-boundary failure, not a signal to overwrite the caller's value with a generated ID.

Core exposes `ResolveSandboxID(sandbox metav1.Object) string` as the server-facing facade. E2B uses this facade for response conversion, logging, pagination, and operation context instead of importing the leaf package. Claim and clone expose the resolved final ID only after infra returns the refreshed object.

Core also owns Checkpoint orchestration. `SandboxManager.CreateCheckpoint` resolves the source ID, overwrites the internal `CreateCheckpointOptions.SandboxID`, and delegates persistence to infra. Server callers cannot supply or spoof this value.

Core owns manager route policy and lifecycle route synchronization. It passes the neutral `infra.Sandbox` capability to the shared projection function described in Section 11.2; it never asks infra to calculate a Route. Cache events and targeted direct observations reach core only through the required backend-neutral `infra.RouteSandboxSource` contract, keyed by `types.NamespacedName` with a nil `infra.Sandbox` as the sole authoritative-absence state.

### 7.3 Infra layer

Infra does not know what a sandbox ID is and does not import its policy. It only executes generic mutation callbacks, persists their result, and exposes backend-neutral Sandbox capabilities.

The backend-neutral `infra.Sandbox` interface removes both `GetSandboxID()` and `GetRoute()`. It adds only the format-neutral `GetPodIP() string` capability needed by manager route projection and E2B's opt-in Pod-IP metadata. Existing `GetState()` and embedded `metav1.Object` supply the remaining neutral projection data. Operations that must persist an ID, such as Checkpoint creation, receive the resolved value explicitly from core.

Infra may depend on an opaque Route reader for its existing cache-staleness check, keyed by the client-supplied opaque ID. It also exposes a required `RouteSandboxSource` whose concrete implementation adapts Sandbox reconcile events and targeted direct reads into `types.NamespacedName` keys and neutral `infra.Sandbox` values; nil means authoritative absence. Kubernetes CRDs, clients, NotFound classification, and cache-controller registration remain inside `sandboxcr`; inclusion policy, projection invocation, Repairer ownership, and Route mutation remain in manager composition. Infra never constructs or deletes an ID from namespace/name.

### 7.4 E2B layer

E2B code does not generate, select, validate, or migrate sandbox IDs. It uses the manager facade for the final ID, reads Pod IP only through the neutral Sandbox capability after authorization, and is responsible only for E2B response/error presentation.

## 8. Generic Infra PostModifier

Add an optional callback to `TryClaimSandbox` and `CloneSandbox` options:

```go
PostModifier func(sandbox metav1.Object) (changed bool, err error)
```

The callback is generic infrastructure plumbing. It has no sandbox-ID-specific behavior and receives only Kubernetes object metadata, which is sufficient for final label assignment. It cannot call `Pause`, `Resume`, `Kill`, `Request`, CSI, or other side-effecting `infra.Sandbox` methods.

The existing pre-lock callback remains broader because current create flows also set timeout, image, resources, and Pod-template metadata, but it becomes error-returning:

```go
Modifier func(sandbox infra.Sandbox) error
```

Pre-lock modifiers are mutation-only callbacks and must not invoke external lifecycle side effects. Existing callers return `nil` after their current mutation. The signature change allows the core reserved-label guard to abort before infra persists a caller mutation.

### 8.1 Execution point

The post modifier runs after all existing claim/clone post-process steps have succeeded, including readiness/runtime/CSI work, and immediately before the successful result is returned.

Infra performs the following sequence:

1. Read the current Sandbox directly through `APIReader`, bypassing the informer cache.
2. Deep-copy it.
3. Invoke `PostModifier` on the copy.
4. If `changed` is false, refresh the wrapper with the fresh object and skip Update.
5. If `changed` is true, persist the modified object with conflict retry.
6. Refresh the infra wrapper with the object returned by the successful Update.
7. Return the wrapper to core.

The first attempt and every conflict retry use `APIReader`, not the informer-backed client. The callback is re-run on a fresh copy during a conflict retry. It must therefore be deterministic and idempotent for the latest object and must not perform external side effects. Short assignment satisfies this rule: it assigns from UID only when the label is empty and otherwise returns `changed=false`.

If `PostModifier` is nil, no additional read or update is performed.

### 8.2 Failure semantics

An error from the callback, refresh, conflict retry, context cancellation, or update fails the overall claim/clone operation. Existing claim/clone cleanup handles the failed operation; infra does not return a partially successful result.

The Sandbox may already have completed readiness work before this failure. This is an accepted consequence of placing final identity mutation after existing post-processes.

An enabled assignment path performs one additional direct API-server Get. The first short assignment also performs an Update; a Sandbox that already has a non-empty label skips the Update when no preceding caller post modifier changed other state. Conflicts may require repeated direct Get/Update attempts. This added API traffic and the new final-stage failure point are accepted operational costs and are measured separately.

### 8.3 Allowed transition window

Before the final label update, controllers and gateway reconciliation may observe the Sandbox under its legacy ID. After the update, they observe the short ID and replace the old key. This internal transition is allowed.

The Create/Clone E2B success response is emitted only after infra refreshes the returned wrapper, so the client receives the final resolved ID. No legacy alias is retained to mask propagation delay.

## 9. Claim, Clone, and Recycle Flows

### 9.1 Claim with assignment disabled

1. Existing claim behavior completes.
2. No core ID post modifier is added.
3. A non-empty existing label still resolves as short.
4. An empty/missing label resolves as legacy.

### 9.2 Claim with assignment enabled

1. Existing claim behavior completes.
2. The final post modifier examines the fresh Sandbox.
3. An existing non-empty label is preserved.
4. Otherwise, a short ID is generated from the Sandbox UID and written as the label.
5. Infra persists and refreshes the wrapper.
6. Core returns the short ID.

The same sequence applies whether the Sandbox was newly created or selected from a warm pool.

### 9.3 Clone with assignment enabled

Clone uses the same final post-modifier contract. The clone's own UID generates its short ID; no sandbox-ID label is copied from the source/template as identity.

### 9.4 Recycle

Recycle does not delete the sandbox-ID label.

- A labeled Sandbox stays short through recycling and later claims.
- An unlabeled Sandbox stays legacy while assignment remains disabled.
- An unlabeled recycled Sandbox transitions to short at the end of a later successful claim with assignment enabled.
- `BuildUserMetadataKeys` never records `LabelSandboxID` as user metadata.
- `resetMetadataForPool` independently ignores `LabelSandboxID` in the decoded label-key list, including data written by an older binary or a manually crafted annotation.

## 10. Cache and Lookup

The claimed-Sandbox cache index uses exactly one key per Sandbox, produced by the core-owned resolver:

- non-empty label: short ID;
- empty/missing label: legacy ID.

When the label is added, the informer update moves the index entry from the legacy key to the short key. The cache does not retain both aliases.

Lookup accepts the client-provided ID as an opaque value. After a cache hit, any API-reader fallback continues to use the Sandbox object key; it never parses the sandbox ID into namespace/name.

Claimed-Sandbox lookup requests enough indexed results to distinguish zero, one, and multiple matches; it no longer uses a one-result limit that hides duplicates. Zero results retain the existing not-found behavior, one result succeeds, and multiple results return a distinct collision/ambiguity error. The caller must not choose the first item or fall back to an API lookup by parsing the ID. Collision errors are logged and counted without putting the ID or ObjectKey in metric labels.

During the allowed label propagation window, a legacy lookup may disappear before a short lookup becomes visible, or vice versa according to cache timing. Existing retry/eventual-consistency behavior handles this; the design does not add aliasing.

`pkg/cache` remains a neutral mechanism in this change. It accepts an optional Sandbox-ID resolver when registering the claimed-Sandbox index. Existing callers retain the current legacy resolver by default; sandbox-manager explicitly injects `sandboxid.Resolve` regardless of the assignment flag. The SandboxClaim controller does not query the claimed-ID index and is not restructured in this change. A separate cache refactor may later remove the existing cross-domain coupling.

## 11. Shared Sandbox Routing

### 11.1 Neutral package and physical stores

Create a responsibility-specific neutral package:

```text
pkg/sandboxroute/
```

It owns the Route data structure, projection with an injected resolver, the ObjectKey-aware Store, resource-version comparison, and the deduplicated targeted Repairer. It does not import `pkg/sandboxid`, cache, infra, E2B, proxy, or gateway packages, and it treats SandboxID as an opaque string.

Manager and gateway are separate processes and therefore keep separate in-memory Store instances:

1. sandbox-manager `proxy.Server.routes`;
2. sandbox-gateway `registry.Registry`.

Both instances use the same `sandboxroute.FromSandbox`, `Store`, and targeted `Repairer` implementation. `proxy.Server` and `registry.Registry` become component facades over their local Store rather than retaining independent `sync.Map` algorithms. This change may retain `proxy.Route` and `proxyutils.Route` as temporary type aliases to reduce call-site churn, but neither compatibility package may own projection or ID policy.

The shared `Route.String()` implementation preserves the existing security contract: `AccessToken` is always rendered as `***`, including when empty, and namespace/name additions do not cause default struct formatting to expose the token.

### 11.2 Route data and projection

Route state permanently carries:

- Sandbox ID;
- namespace;
- name;
- UID;
- resource version;
- existing routing data.

The additive fields are serialized as `namespace` and `name` with `omitempty`, so an old payload remains distinguishable by both fields being absent/empty and old decoders ignore the new JSON members.

Namespace/name are required for object-key lifecycle management. `ProjectionSource` resolves the ID and access token at the component boundary; `FromSandbox` never reads the sandbox-ID label or implements ID or token compatibility policy itself.

Projection accepts a minimal neutral capability rather than depending on `infra.Sandbox`:

```go
type ProjectionSource interface {
    metav1.Object
    GetPodIP() string
    GetState() (state, reason string)
    ResolveID() string
    GetAccessToken() string
}

func FromSandbox(source ProjectionSource) (Route, error)
```

`FromSandbox` copies namespace, name, UID, resourceVersion, Pod IP, normalized state, owner, and the ID and access token supplied by the source. Manager wraps `infra.Sandbox` in a manager-owned source that resolves the opaque ID and reads only the current runtime token annotation, without adding those policy methods to Infra. Gateway wraps the watched Sandbox CR in a lightweight source that resolves the label-aware ID, retains legacy-resolution observability and the envd token fallback, and snapshots `GetSandboxState` once so its inclusion decision and Route use the same state. The function constructs but does not validate the Route, preserving each caller's existing validation and retry behavior.

Manager's neutral `RouteSandboxSource` handler and gateway's controller-runtime Reconciler remain separate thin event adapters. For a present Sandbox, both call `FromSandbox` and offer the full Route to the Store; the Store performs ordering and no-op decisions. The concrete manager Infra source performs only Kubernetes event/read adaptation and propagates handler errors back to the cache controller for retry. Peer HTTP handlers and component-specific inclusion policies remain outside the shared projection and Store.

`proxyutils.DefaultGetRouteFunc`/`GetRouteFromSandbox` and the stateful `Projector`/`ProjectionInput` API are removed from production use. Manager composition installs the feeder callback and targeted Repairer through the neutral source after building Infra; `sandboxcr` only connects that callback to its Kubernetes cache controller. Gateway composition no longer carries a configurable projector dependency; its projection source retains legacy-resolution metrics and is passed directly to `FromSandbox`.

### 11.3 Records, indexes, and active-view invariants

Each Store maintains:

```text
ObjectKey(namespace/name) -> current full record
UID                       -> full, compatibility, or retired ownership fence
SandboxID                 -> active Route or collision marker
```

There are two record shapes:

- **full/ObjectKey-backed**: namespace and name are present and the record participates in object lifecycle, UID/RV fencing, and targeted authoritative repair;
- **compatibility ID-only**: both fields are absent, the record retains ID/UID/RV from an old peer, and it never invents an ObjectKey.

A retired-UID fence is not a Route and never appears in ID lookup. When a full ObjectKey changes from UID A to UID B, the Store retains A as retired compatibility ownership so a late ID-only update for A cannot create a separate legacy record beside B's current short ID. The fence is refreshed by both normal replacement and targeted authoritative replacement. It may be pruned only after the bounded old-peer retry/drain window; a deletion fence additionally requires a successful targeted observation of its ObjectKey before pruning. Pruning never activates an ID. Compatibility ID-only records carry a last-observed time and expire after the same bounded drain window because they have no ObjectKey that can be read directly.

The Store surface keeps event authority explicit rather than exposing a generic `Set/Delete` pair. Exact Go names may follow package style, but the implementation has distinct operations equivalent to `Upsert`, `DeleteAuthoritativeByObjectKey`, `DeleteConditionally`, and `ApplyAuthoritativeRepair`. `Upsert` and `DeleteConditionally` dispatch internally by Route shape (ObjectKey-backed vs ID-only). Every mutating operation returns a structured result (`applied`, `ignored`, `invalid`, `collision`, or `repair_required`) plus a fixed reason and the affected ObjectKeys when relevant.

ObjectKey is the serialization boundary for a logical Kubernetes object location. All indexes, collision state, and route-count changes are updated under one Store lock. A single `Get`, `List`, or snapshot observes either the old active ID or the new active ID. Two independent calls made on opposite sides of the switch may naturally observe old and then new; the design does not promise a cross-call transaction.

The active view enforces:

1. one full record per ObjectKey;
2. at most one active ID per full record/UID;
3. one active Route per ID;
4. once a UID has full ownership, an ID-only event cannot create or modify an active alias for it;
5. a retired UID cannot regain ID-only compatibility ownership;
6. an ID observed on different ObjectKeys is ambiguous and is not routable.

On a cross-ObjectKey ID collision, the Store does not use last-write-wins. It retains enough object records to recover after a later update/delete, marks the ID as collided, removes it from successful lookup, and returns a collision result plus all known claimant ObjectKeys to the adapter. Collision creation and resolution are logged structurally and counted without identity metric labels. A later accepted event or targeted repair recomputes ownership and reactivates the ID only when exactly one record claims it.

### 11.4 Resource-version and upsert state machine

The Store uses an explicit comparison with four outcomes: older, equal, newer, or unorderable. Identical strings are equal; otherwise both values must parse as base-10 unsigned integers to be ordered. Empty or non-numeric unequal values are unorderable. It does not reuse a helper whose malformed-old behavior implicitly accepts every new value.

For a full Route event at the same ObjectKey:

1. Validate non-empty ID, namespace, name, UID, and resourceVersion before touching the Store.
2. If no ObjectKey record or deletion fence exists, the event may establish current ownership after collision checks. Against a deletion fence, either the same UID (for example after recycle) or a different replacement UID requires a strictly newer numeric RV unless a targeted authoritative read establishes the current API state. An older comparable event is ignored; an equal or unorderable event keeps the route quarantined and enqueues the ObjectKey for repair.
3. If UID and ID both match the current full record, accept an equal or newer comparable RV and ignore an older or unorderable RV.
4. If UID matches but ID changes, require a strictly newer numeric RV. This is the normal persisted-label transition and prevents an equal-RV full payload from changing short back to legacy or to another ID.
5. If UID differs, accept object replacement only when the incoming numeric RV is strictly newer. Ignore an older comparable event; quarantine an equal or unorderable event and enqueue the ObjectKey for repair.
6. When a different-UID replacement is accepted, retain the previous UID as a non-routable retired fence.
7. For every accepted event, atomically retire the previous ID for that ObjectKey, remove any compatibility ID-only record and retired fence owned by the incoming UID, then install the full record and recompute ID ownership.

If a compatibility ID-only record with the same UID already exists, an equal/newer comparable full event upgrades it to ObjectKey ownership. A full short event therefore retires a same-UID legacy ID-only record in the same transaction; it never waits for targeted repair.

If the incoming full Route's target legacy ID is occupied only by an ID-only record with a different UID, the full Route may supersede it only when its numeric RV is strictly newer. The old compatibility UID becomes retired and the full Route gains ObjectKey ownership atomically. Equal, older, or unorderable RVs fail closed as a collision and enqueue the full Route's ObjectKey for targeted repair. This higher-authority conversion never applies when the target ID is owned by a full record at another ObjectKey.

An ID-only update follows a narrower compatibility state machine:

1. Validate non-empty ID, UID, and resourceVersion and require both namespace and name to be absent.
2. If the UID is full-owned or retired, or the target ID is owned by a full record, ignore the event without changing collision state; lower-authority ID-only traffic cannot downgrade/quarantine a full record or revive an old incarnation even when its RV appears newer.
3. With no existing ownership, accept one compatibility record.
4. For the same ID and UID, accept only an equal/newer comparable RV; ignore older or unorderable updates.
5. Among ID-only records, a different UID for an occupied ID, or a different ID for an already-recorded compatibility UID, is a collision and cannot create a second active route.

These rules close the required compatibility transitions:

| Current state | Incoming event | Result |
|---|---|---|
| ID-only legacy | full legacy, same UID, equal/newer RV | atomically adopt as ObjectKey-backed |
| ID-only legacy | full short, same UID, equal/newer RV | atomically retire legacy ID and activate short ID |
| ID-only legacy UID A | full legacy UID B on same ID, strictly newer RV | retire A and atomically establish B full ownership |
| full current | any ID-only update for same UID | ignore; never downgrade or add an alias |
| full current | ID-only update for another UID on its ID | ignore; full ownership remains active |
| short full current | late same-UID legacy ID-only update | ignore; legacy alias stays absent |
| new-UID short full current | late previous-UID ID-only update | ignore through retired-UID fence |
| full ObjectKey A owns ID | full ObjectKey B offers the same ID | quarantine ID as collided; no successful route lookup |

Normal full upserts deliberately remain conservative for cross-UID unorderable RVs. Section 11.8 defines a distinct asynchronous targeted repair operation that can resolve this state without weakening event-path fencing or blocking event delivery.

### 11.5 Delete modes and fencing

Deletion is not modeled as an unconditional `Delete(route.ID)`. Store APIs distinguish three authorities:

1. **Local authoritative ObjectKey delete.** A local reconciler that observed NotFound/DeletionTimestamp, or manager after a successful delete/recycle trigger, deletes the current full record at that ObjectKey regardless of its UID/RV and leaves ObjectKey/retired-UID deletion fences. If no ObjectKey record exists during mixed rollout, the same atomic operation may delete only an ID-only compatibility record at the separately injected legacy fallback ID. It never deletes a full record belonging to another ObjectKey through that fallback.
2. **Full peer conditional delete.** Namespace/name, ID, UID, and RV must all be present. It deletes a current full record only when ObjectKey, ID, and UID match and the delete RV is equal/newer and comparable, then leaves the same fences. A different UID never deletes the current incarnation, even if its RV is numerically newer. If no full record exists, it may conditionally remove an ID-only record only when ID/UID match and the RV fence passes.
3. **ID-only peer conditional delete.** It can delete only an ID-only compatibility record with the same ID and UID and an equal/newer comparable RV. It never deletes or downgrades an ObjectKey-backed record, including one with the same UID.

Older, unorderable, identity-mismatched, and already-absent deletes are no-ops with an explicit result/reason for aggregate metrics and debug logs. In particular, a late delete for UID A cannot remove UID B at the same ObjectKey, and a late ID-only delete cannot remove a full short record.

Deletion and any resulting collision resolution occur in the same Store transaction. Receivers never split an ID to derive ObjectKey.

### 11.6 Peer Route backward compatibility

Namespace and name are additive JSON fields:

- an old receiver ignores the new fields and continues using `route.ID`;
- a new receiver accepts an old Route whose namespace and name are both empty, treats `route.ID` as an already-calculated legacy ID, and sends it through the ID-only update/delete state machine;
- a Route with exactly one of namespace or name missing is invalid, returns HTTP 400, and is rejected with an error log;
- both full and ID-only payloads require non-empty ID, UID, and resourceVersion; malformed payloads return HTTP 400 before Store mutation;
- the receiver never splits `route.ID` to reconstruct ObjectKey.

Old senders cannot produce short IDs during the initial disabled rollout. After short assignment is enabled, every supported sender must include namespace/name in a short Route.

### 11.7 Feeders and ownership

The implementation plan covers every Store feeder explicitly:

| Store | Feeder | Route shape |
|---|---|---|
| manager | neutral Infra Sandbox event | full Route |
| manager | E2B lifecycle operation refresh | full Route |
| manager | peer refresh | full Route from new sender; ID-only legacy Route from old sender |
| manager | neutral Infra direct observation | one authoritative full Route or ObjectKey absence |
| gateway | Sandbox controller watch | full Route |
| gateway | peer refresh | full Route from new sender; ID-only legacy Route from old sender |
| gateway | direct-reader targeted repair | one authoritative full Route or ObjectKey absence |

Manager route policy and feeding composition are owned by sandbox-manager core, not `sandboxcr` infra. Infra exposes only neutral Sandbox events and observations; claim/clone/pause/resume/delete sync all pass the returned Sandbox to `sandboxroute.FromSandbox`. Owner checks read `AnnotationOwner` directly from the already-authorized Sandbox. Gateway retains its own controller and running-state policy. Each component supplies one inclusion/deletion predicate reused by its event adapter and targeted repair observation, including exclusion of `DeletionTimestamp` objects, so repair cannot re-add a route that the same component considers deleted. Peer HTTP status/state interpretation remains component-specific, but every accepted update/delete reaches the shared Store API matching its authority.

### 11.8 Asynchronous targeted repair

Manager and gateway each run the shared Repairer against authoritative direct observations, not informer-backed reads. Gateway owns its direct `APIReader`; Manager calls the required Infra source, whose `sandboxcr` implementation owns the direct `APIReader` and converts results into neutral Sandbox values, using nil for NotFound. The Repairer performs only ObjectKey Gets requested by Store mutation results; it never Lists all Sandboxes. Each component applies the same configured namespace/label visibility and inclusion predicate as its normal feeder, so direct access cannot broaden the local Store's scope.

The event path is non-blocking:

1. A normal Store mutation that encounters a different UID with an equal/unorderable RV or a deletion fence that cannot be safely crossed quarantines the affected ID and returns `repair_required` with the affected ObjectKeys and their mutation generations. A full-route collision retains its `collision` result for HTTP mapping and carries the same repair requests for all known claimants.
2. The event adapter enqueues those repair requests and immediately completes normal event handling without waiting for the API server.
3. The shared rate-limiting queue deduplicates by ObjectKey. If a newer request arrives while the key is queued or processing, the queued state retains the newest affected-record generation and the key is processed again.
4. A fixed, low-concurrency worker performs the direct Get and projection outside the Store lock.
5. Transient read/projection errors use rate-limited backoff. Context cancellation shuts down workers without accepting a partial observation.

The direct Get has three authoritative outcomes for that ObjectKey:

- a present, included, non-deleting Sandbox is projected into a full Route;
- NotFound, `DeletionTimestamp`, or exclusion by the component predicate is an authoritative absence;
- any other error leaves the Store unchanged and retries later.

`ApplyAuthoritativeRepair` compares the expected mutation generation with the current affected record or fence, not with unrelated Store activity. If that state advanced while the Get was in flight, the observation is stale and is ignored; a newer queued request owns the next attempt. Otherwise the Store atomically installs the current full Route or deletes the ObjectKey record, refreshes retired/deletion fences, and recomputes every ID collision involving that ObjectKey. A high event rate on unrelated Sandboxes therefore cannot starve a repair.

If direct Gets confirm that multiple live ObjectKeys still claim the same ID, the collision remains quarantined and the completed work items are forgotten rather than retried indefinitely. A later update or delete for any claimant recomputes the collision and may enqueue a new repair. Peer endpoints still return HTTP 409 for the collision even though its claimant verification is asynchronous.

The Store retains the deleted ObjectKey's last UID/ID/RV plus mutation generation when an accepted delete removes the active record. After the bounded compatibility-drain window, its ObjectKey is enqueued once for targeted confirmation; the fence is pruned only when a generation-matched direct Get confirms absence. A live object is projected and repaired instead. Retired-UID fences and ID-only records may expire after the same bounded window once all old replicas are gone; pruning never activates an ID.

Normal route population and missed-event recovery rely on informer initial synchronization, List/Watch reconnects, and existing reconcile retries. There is no periodic direct-reader List or claim that targeted repair can discover an object that produced no event and is absent from the Store. Direct API traffic is bounded by ambiguous ObjectKeys, queue concurrency, and QPS rather than total Sandbox count.

Adapters and request routing continue treating SandboxID as opaque.

## 12. Checkpoint and Pagination

### 12.1 Checkpoint

Checkpoint stores the sandbox ID that was final for the claim which created it. E2B calls `SandboxManager.CreateCheckpoint`; core resolves the source ID, overwrites `CreateCheckpointOptions.SandboxID`, and delegates to `infra.Sandbox.CreateCheckpoint`. Infra validates that the supplied ID is non-empty and only persists it.

Checkpoint does not know whether the value is legacy or short.

If a recycled Sandbox later transitions from legacy to short:

- existing Checkpoints keep their legacy source ID;
- later Checkpoints store the short source ID;
- exact snapshot filtering naturally associates each Checkpoint with the claim-visible ID at its creation time.

No historical Checkpoint migration is performed.

### 12.2 Pagination

Sandbox listing uses the final resolved ID as the existing uniqueness/tie-break component. It does not parse the ID or assume a particular format.

An ID transition can change a Sandbox's pagination key between list requests, just as other mutable list state can change. The change is accepted and does not justify retaining two identities.

## 13. E2B Diagnostics

Short IDs deliberately lose human-readable namespace/name information. E2B responses restore that context without changing identity semantics.

### 13.1 Success metadata

For successful E2B responses that expose Sandbox metadata, add exactly one response-only metadata entry:

```text
e2b.agents.kruise.io/sandbox-resource: <namespace>/<name>
```

This key:

- is generated from the authorized Sandbox resource;
- is protected from user spoofing/override;
- is not persisted into user metadata;
- is not used by metadata list filtering;
- carries both namespace and name in one value.

E2B first copies ordinary user-visible labels and annotations through the existing metadata filter, then writes this protected key last from the authorized Sandbox object. The `label:` extension parser rejects both exact protected keys:

```text
agents.kruise.io/sandbox-id
e2b.agents.kruise.io/sandbox-resource
```

The first protects identity assignment; the second prevents response-context spoofing. Neither can reach Sandbox labels, Pod-template labels, or `UpdatedMetadataInClaim`. This targeted reservation does not change the acceptance rules for unrelated label extensions.

### 13.2 Errors

After a Sandbox has been located and ownership/authorization has succeeded, E2B error messages include:

```text
sandboxResource=<namespace>/<name>
```

This context is added to downstream failures such as runtime, gateway, checkpoint, or lifecycle-operation errors.

Not-found responses and owner-mismatch/unauthorized responses must not include namespace/name, because doing so would disclose the existence or location of another tenant's Sandbox.

The error code and classification remain unchanged; only authorized diagnostic context is appended.

## 14. Rollout, Activation, and Rollback

### 14.1 Binary rollout

Initial migration has one precondition: no Sandbox already carries a short-ID label. A cluster that has previously enabled short IDs has crossed the rollback boundary and cannot use this first-rollout procedure with old binaries.

1. Deploy new sandbox-manager and gateway binaries with `--enable-short-sandbox-id=false`.
2. The two binaries may be rolled out in either order while assignment is disabled.
3. New Route senders include namespace/name; old receivers ignore those additive JSON fields. New receivers accept old ID-only legacy Routes under the compatibility rules in Section 11.6.
4. Verify all old replicas are terminated, at least the bounded peer retry window has elapsed, informer caches are synchronized, and every manager/gateway targeted repair queue is drained. Compatibility expiry and completed repairs must leave zero ID-only records and zero unresolved collisions before activation. Confirm all live replicas understand label resolution, conditional delete, collision quarantine, ObjectKey route replacement, and targeted direct-read repair.
5. Enable short assignment on sandbox-manager.
6. Observe assignment/guard errors, route replacement, ignored stale deletes, legacy-delete fallback, collision counts, targeted repair queue health, lookup failures, and E2B traffic.

While the flag is disabled, no new unlabeled Sandbox is transitioned by the new code, so rollout order is not coupled.

### 14.2 Rollback boundary

After any short label has been assigned, rolling back manager or gateway to a version that ignores the label is unsafe. The old binary would reconstruct a legacy ID and disagree with persisted state.

Turning the feature flag off is safe for stopping new assignments, but it is not a data rollback:

- existing labels remain;
- labeled Sandboxes continue resolving as short;
- unlabeled Sandboxes remain legacy.

If compatibility is eventually removed, that is a separate code change after operators confirm no supported unlabeled legacy Sandboxes remain.

## 15. Observability

Add aggregate metrics without namespace, name, UID, or sandbox-ID labels:

```text
sandbox_id_legacy_resolution_total{surface="e2b|gateway"}
sandbox_id_assignment_total{result="success|failure"}
sandbox_id_collision_total{surface="cache|manager_route|gateway_route"}
sandbox_route_legacy_fallback_total
sandbox_route_invalid_total
sandbox_route_records{shape="id_only|collision"}
sandbox_route_repair_queue_depth
```

Structured logs for assignment/update failures include namespace and name. Successful assignment is debug-level to avoid noisy per-Sandbox info logs.

Claim/clone metrics continue to expose total duration. Post-modifier stage Prometheus series are removed; conflict and failure details stay in structured logs. Assignment metrics expose only `sandbox_id_assignment_total{result=success|failure}`; assignment failure reasons, valid route mutation outcomes, repair outcomes, stale results, and retry details remain fixed fields in structured logs instead of Prometheus labels. Route metrics do not carry a manager/gateway label because the components run in separate processes and scrape-target metadata already identifies the component. The metric package uses explicit component-level registration: controller registers Sandbox ID metrics, while sandbox-manager and sandbox-gateway register both Sandbox ID and route metrics.

Metrics report only legacy resolution; they do not trigger validation of non-empty labels.

## 16. Repository Impact and Migration Inventory

The implementation plan must account for these current format dependencies:

1. The shared legacy helper in `pkg/utils/utils.go` currently calculates `<namespace>--<name>`.
2. `pkg/cache/index.go` currently uses that helper for the claimed-Sandbox field index; cache construction is shared with the SandboxClaim controller.
3. `GetClaimedSandbox` currently requests only one indexed result, which hides duplicate IDs.
4. `pkg/utils/proxyutils/route.go` currently uses the helper while constructing Route and must preserve token redaction when Route moves.
5. `pkg/sandbox-gateway/controller/gateway_controller.go` directly concatenates namespace/name for update and NotFound deletion.
6. Manager `proxy.Server.routes` and gateway `registry.Registry` are independent single-key stores, and the `proxyutils.Route` struct currently carries no namespace/name fields.
7. Manager `refreshRoute` looks up only the newly resolved ID and compares only State/IP; it cannot retire the previous ID.
8. Gateway `registry.Update` applies one `IsResourceVersionNewer` (`>=`) branch under a single key.
9. Manager `reconcileSandbox` deletes by a reconstructed ID, and `reconcileRoutes` runs from `sandboxcr.Infra` every five minutes using the informer-backed client.
10. Manager and gateway peer refresh endpoints exchange Route JSON and currently delete by `route.ID` without UID/RV fencing.
11. `sandboxcr.Infra` owns route feeder registration and a Proxy dependency also used as an opaque staleness signal after cache lookup.
12. Checkpoint creation in `pkg/sandbox-manager/infra/sandboxcr/clone.go` currently persists the helper result, while E2B calls `infra.Sandbox.CreateCheckpoint` directly.
13. `infra.Sandbox.GetSandboxID()` is used by E2B conversion, list pagination, create logs, and pause/resume/connect helpers.
14. `infra.Sandbox.GetRoute()` is used for manager ownership checks, lifecycle route sync/deletion, and E2B Pod-IP response metadata.
15. Existing claim/clone `Modifier` callbacks cannot return an error, so a reserved-key guard cannot stop persistence without changing their contract.
16. E2B `label:` extensions and SandboxClaim labels currently flow into Sandbox labels; `BuildUserMetadataKeys` records them and recycle deletes every recorded label key.
17. SandboxSet/SandboxTemplate materialization already strips non-preserved internal-prefix labels and must continue stripping the new constant.
18. Snapshot listing compares the Checkpoint's stored source ID exactly.
19. Pagination uniqueness currently includes the helper result.
20. `AnnotationSandboxID` already uses the same qualified string on Checkpoints.
21. Tests and E2B fixtures contain literal legacy IDs or split them on `--`.
22. Namespace validation currently reserves `--` to keep legacy IDs unambiguous.

Migration rules are:

- Replace production ID decisions with the core-owned resolver, manager facade, or an explicitly injected resolver.
- Do not let `pkg/cache`, `pkg/utils/proxyutils`, `pkg/sandboxroute`, or infra import the manager-domain leaf package.
- Define only the reserved label-key constant in `api/v1alpha1`; input/recycle code may compare that key but must not resolve, generate, assign, or validate ID values.
- Reject the reserved key at every user-controlled label boundary, guard manager caller callbacks before persistence, exclude it from `UpdatedMetadataInClaim`, and skip it during recycle cleanup.
- Change existing infra `Modifier` callbacks to return errors and restrict `PostModifier` to `metav1.Object`; do not add external side effects to either callback.
- Keep cache neutral by adding only an optional resolver injection; do not perform the planned cache-domain refactor in this change.
- Make claimed-ID lookup detect multiple indexed matches and return an ambiguity error instead of selecting one.
- Keep the old helper only as the internal legacy fallback while compatibility is required; do not let new call sites use it as the canonical resolver.
- Replace both physical route algorithms and all feeders with the shared full/ID-only Store state machine, conditional deletes, and collision quarantine.
- Move manager feeder registration/repair ownership out of `sandboxcr.Infra` into sandbox-manager composition; gateway reconciliation remains a thin adapter around shared stateless projection.
- Keep only an opaque Route reader in infra where the existing cache-staleness check needs it.
- Run the shared targeted Repairer with a direct API reader in both manager and gateway, using a deduplicated rate-limiting queue plus affected-record generation fencing; never perform a periodic full Sandbox List.
- Remove direct concatenation from gateway production code except the injected legacy delete fallback at the compatibility boundary.
- Treat IDs as opaque in all stores, filters, request adapters, and server APIs.
- Route Checkpoint creation through manager core and pass the final ID explicitly to infra.
- Remove both `GetSandboxID()` and `GetRoute()` from the infra Sandbox abstraction; add only `GetPodIP()`, read owner directly from annotations, and route all projection through manager-owned/shared projection.
- Remove production projection from `proxyutils`; compatibility aliases must continue using the shared token-redacting `Route.String()`.
- Keep the Sandbox label and Checkpoint annotation metadata kinds distinct even though they intentionally share a qualified key string.
- Update fixtures/helpers that assume every ID is reversible, while retaining explicit legacy cases.
- Keep namespace validation unchanged in this change.

## 17. Error Handling

1. Short generation errors fail the final claim/clone stage and run existing cleanup.
2. Kubernetes update conflicts are retried against a direct API-reader object.
3. Context cancellation stops retries and is reported distinctly.
4. NotFound during final modification is an operation failure, not a silent legacy fallback.
5. A non-empty existing label never produces a validation error.
6. A user-supplied reserved key is rejected before infra; a caller callback that changes it fails before its object is persisted. Guard failures are terminal for the claim/clone attempt and are not retried against another Sandbox.
7. Errors preserve existing domain error codes where possible and add stage/resource context without changing client-visible classification.
8. A PostModifier returning `changed=false` is successful and does not issue an Update.
9. A peer Route with partial ObjectKey or missing ID/UID/RV is malformed and returns HTTP 400 without Store mutation.
10. A well-formed stale, identity-mismatched, or lower-authority ID-only event dominated by a full record is an idempotent no-op and returns HTTP 204. A Store collision returns HTTP 409 and quarantines the ambiguous ID locally; it is not reported as a successful overwrite.
11. Cache ID ambiguity maps to a fail-closed manager internal error without disclosing the colliding ObjectKeys to an unauthorized E2B caller.
12. A targeted repair Get/projection error leaves the Store unchanged and is retried with rate-limited backoff; a generation-mismatched result is ignored as stale.
13. Checkpoint creation rejects an empty core-supplied SandboxID before persistence.

## 18. Testing Strategy

All Go tests are table-driven with descriptive `name` fields. Error cases use `expectError string`; an empty value means success, and a non-empty value must be contained in the actual error.

### 18.1 Prefer existing tables

When a changed behavior already has a table-driven test in the same directory and responsibility, add cases to that table first. Small additions to test-case fields or setup/assertion branches are allowed.

Create a new test function only when:

- the new `sandboxid` package has no existing table;
- no existing table covers the responsibility;
- the behavior requires a distinct lifecycle/concurrency harness that would make an existing table unclear.

Do not rewrite unrelated tests merely to force consolidation.

### 18.2 Core `sandboxid` cases

Use one compact table where practical to cover:

- non-empty label returned unchanged;
- absent and empty labels falling back to legacy;
- deterministic 26-character lowercase unpadded Base32 encoding;
- different UIDs producing different values;
- invalid UID failing generation;
- assignment to empty/missing label;
- non-empty label never overwritten or validated;
- assignment idempotency.

### 18.3 Manager core cases

Extend existing claim/clone option or orchestration tables to cover:

- flag disabled leaves an unlabeled Sandbox legacy;
- flag disabled still returns an existing short label;
- flag enabled injects/composes the post modifier;
- guarded caller post modifier runs before short assignment;
- existing label is preserved;
- pre-lock caller modifier cannot add the reserved label, including while the flag is disabled;
- pre-lock caller modifier cannot change or delete an existing reserved label;
- final caller post modifier cannot add, change, or delete the label;
- guard failure stops before persistence, while core's own `AssignShort` is allowed to write the label;
- guard failures are terminal rather than candidate/create retries;
- a recycled unlabeled legacy Sandbox transitions on a later enabled claim;
- post-modifier failure uses existing cleanup;
- `ResolveSandboxID` exposes the core result to E2B without leaf-package access;
- owner checks read the authorized Sandbox annotation without constructing a Route;
- manager lifecycle sync passes `infra.Sandbox` to `sandboxroute.FromSandbox` and never calls infra `GetRoute`;
- manager Checkpoint orchestration overwrites the internal option with the resolved ID;
- E2B does not independently choose the ID.

### 18.4 Public metadata and recycle cases

Prefer the existing extension, SandboxClaim option-building, SandboxSet materialization, metadata-key, and recycle tables:

- E2B `label:agents.kruise.io/sandbox-id` is rejected for both claim and clone before options are invoked, independent of the assignment flag;
- E2B rejects the protected response-resource label key as well;
- SandboxClaim labels reject the reserved ID key before `TryClaimSandbox`;
- SandboxSet/SandboxTemplate materialization strips the reserved internal label;
- `BuildUserMetadataKeys` excludes the reserved ID key defensively;
- recycle preserves an existing short label even when `UpdatedMetadataInClaim` maliciously or historically lists the key;
- rejected arbitrary/duplicate ID injection cannot call cache lookup or route projection/Store.

### 18.5 Infra cases

Extend existing `TryClaimSandbox` and `CloneSandbox` table tests to cover:

- nil callback causes no extra update;
- callback runs after current post-processes;
- first mutation read uses `APIReader`, not the informer client, and uses a fresh deep copy;
- `changed=false` refreshes the wrapper and skips Update;
- `changed=true` persists exactly once without a conflict;
- update conflict re-reads through `APIReader` and retries;
- callback remains idempotent across retry;
- callback errors from the error-returning pre-lock `Modifier` stop before the lock/create update;
- context cancellation stops retry;
- returned wrapper is refreshed to the persisted object;
- callback/update errors fail the operation and record the correct stage;
- `GetPodIP()` returns the backend-neutral value used by manager/E2B.

### 18.6 Cache, routing, and gateway cases

Merge into existing index/store/controller tables where possible. The shared projection function, Store, and Repairer receive focused table tests once; manager/gateway adapter tests cover only their wiring and component-specific policies:

- one legacy or short cache key, never both;
- label update moves the cache key;
- default cache construction remains legacy and manager injection resolves labels;
- multiple claimed cache matches return the collision error instead of an arbitrary first Sandbox;
- route legacy-to-short transition removes the old ID key;
- a single Store snapshot/list never contains both legacy and short IDs; separate reads spanning a switch are not asserted as transactional;
- a late old resource version cannot resurrect it;
- a same-UID equal-RV full event with a different ID is ignored, while a strictly newer label RV switches IDs;
- same ObjectKey with a new UID and newer resource version replaces the old incarnation;
- a late event from the previous UID cannot replace the newer incarnation;
- a different-UID event with equal or unorderable resource version is ignored;
- ID-only legacy to full legacy with the same UID adopts ObjectKey ownership;
- ID-only legacy to full short with the same UID atomically leaves exactly one active short ID;
- a strictly newer different-UID full legacy Route replaces an ID-only record on the same legacy ID, while equal/unorderable RV fails closed;
- a full record ignores all same-UID ID-only updates and deletes, even with an apparently newer RV;
- a late legacy ID-only update cannot revive an alias after a full short Route exists;
- a late previous-UID ID-only update cannot create a legacy alias beside the replacement UID's short Route, and safe fence pruning does not activate an ID;
- authoritative local NotFound deletion removes the ObjectKey's current ID;
- a deletion fence rejects late equal/older same-UID full and all ID-only updates, while a strictly newer recycled-UID/replacement-UID event or generation-matched targeted repair can establish current API state;
- missing ObjectKey uses the injected legacy fallback only against an ID-only record;
- a full conditional delete with an old UID cannot delete a recreated Sandbox;
- a same-UID older delete and a different-UID equal/unorderable delete are ignored;
- an ID-only conditional delete cannot delete a full record or a mismatched ID-only record;
- a full cross-ObjectKey duplicate ID is quarantined and lookup fails closed; deleting/fixing one participant restores a unique mapping;
- an old peer Route with both namespace/name absent follows only the compatibility state machine;
- a new Route is accepted by an old JSON decoder, while partial ObjectKey or missing ID/UID/RV receives HTTP 400 from the new receiver;
- route count remains stable through transition;
- manager core feeder and gateway reconciliation use the same `FromSandbox` function, while infra owns no projection/feeder/repair worker;
- `Route.String()` always redacts `AccessToken` after the type move;
- ambiguity returns the affected ObjectKeys without blocking the event adapter on an API read;
- the shared queue deduplicates ObjectKeys, retains the newest repair generation, and rate-limits transient failures;
- a targeted direct Get replaces a different UID with equal or unorderable RV only when the affected record generation still matches;
- NotFound/deleting/excluded repair observations perform a generation-matched authoritative ObjectKey delete;
- Get/projection errors perform no Store mutation, and a stale in-flight result cannot overwrite a concurrent event;
- unrelated Store mutations do not invalidate or starve a repair for another ObjectKey;
- collision repair enqueues and verifies every known claimant ObjectKey;
- a confirmed live duplicate remains quarantined without an unbounded retry loop;
- deletion-fence confirmation and compatibility expiry do not reactivate an ID;
- no repair path issues a full Sandbox List;
- gateway never directly constructs `<namespace>--<name>` outside the injected compatibility fallback;
- adapters treat IDs opaquely.

### 18.7 E2B, Checkpoint, and pagination cases

Extend existing response/error/list/snapshot tables to cover:

- authorized happy-path metadata contains the protected resource key;
- user metadata cannot spoof that key;
- the `label:` extension rejects the protected response-only key;
- the key is response-only and excluded from metadata filtering;
- authorized downstream errors include `sandboxResource`;
- not-found and unauthorized errors do not disclose it;
- opt-in Pod-IP metadata uses `GetPodIP()` and does not construct a Route;
- Checkpoint persists the explicit final ID without format knowledge;
- empty Checkpoint SandboxID is rejected;
- a Sandbox annotation with the shared key does not affect ID resolution, and a Checkpoint label does not affect source-ID lookup;
- old Checkpoints remain legacy after a later Sandbox transition;
- pagination uses the resolved ID opaquely.

### 18.8 Static boundary checks and verification

Code review/static search must confirm:

- only sandbox-manager core policy writes the sandbox-ID label;
- API/controller code outside core only compares the reserved key for validation or recycle preservation;
- infra and E2B contain no ID assignment/format policy;
- cache, proxy utilities, shared routing, and infra do not import `pkg/sandboxid`;
- `infra.Sandbox` exposes neither `GetSandboxID()` nor `GetRoute()`;
- `sandboxcr.Infra` exposes only neutral Sandbox event/direct-observation adaptation and does not project Routes or start targeted route repair;
- no user label mapper or caller mutation hook can persist the reserved ID label;
- gateway has no direct namespace/name concatenation for SandboxID;
- no new code parses a client-provided ID with `--`.

Run focused unit tests only for changed packages under `pkg/`. Do not run E2E tests under `test/`. After focused tests pass, build sandbox-manager and gateway binaries to `/private/tmp` for final integration verification.

## 19. Acceptance Criteria

1. A short-enabled successful claim/clone of an unlabeled Sandbox returns the persisted 26-character short ID.
2. A non-empty label is returned unchanged in every component, independent of the assignment flag.
3. An unlabeled Sandbox resolves to the legacy ID.
4. E2B, SandboxClaim, pool/template inheritance, caller modifiers, and recycle cannot introduce, alter, or remove the core-owned label outside core assignment.
5. No Sandbox has simultaneous active legacy and short route/cache aliases.
6. Recycled unlabeled Sandboxes may transition on a later enabled claim; labeled Sandboxes never transition back.
7. Duplicate IDs fail closed in cache and routing; no arbitrary Sandbox or last writer is selected.
8. Manager and gateway use the same `FromSandbox`, full/ID-only Store, conditional-delete, and collision semantics.
9. Late old-UID or old-RV peer updates/deletes cannot remove a new incarnation or revive a retired legacy alias.
10. Both targeted Repairers resolve known unorderable cross-UID state asynchronously without blocking event delivery, issuing a full Sandbox List, or overwriting a newer affected-record generation.
11. Gateway accepts old ID-only legacy Route payloads during disabled rollout and deletes them only through conditional compatibility or the local authoritative legacy fallback.
12. `infra.Sandbox` has no ID/Route format decision; manager owns route projection, owner checks, and ID resolution.
13. Checkpoint stores the explicit claim-visible ID supplied through manager core and requires no format awareness.
14. E2B success metadata and authorized errors expose namespace/name; not-found and unauthorized errors do not.
15. New assignment remains disabled through manager/gateway rollout and is enabled only after old-peer drain, compatibility expiry, and drained repair queues report zero ID-only/collision records, removing rollout-order dependency without carrying stale compatibility state into activation.
16. The ID is treated as CR identity, never as proof of current owner or claim session.
17. Tests follow the repository's table-driven and existing-table-first requirements.

## 20. Alternatives Considered

### 20.1 Raw Kubernetes UID

Using the textual UID is simple and unique, but a UUID is 36 characters. Base32 encodes the same 128 bits in 26 DNS-safe characters and better addresses the domain-length constraint.

### 20.2 Truncated hash or UID

A shorter truncation would further reduce domains but introduces an avoidable collision budget and collision-handling policy. The full 128-bit UID encoding is already short enough.

### 20.3 Random persisted ID independent of UID

This also works, but requires randomness, collision handling, and additional failure paths. UID already provides a stable per-resource identity.

### 20.4 Format flag or annotation

A separate `sandbox-id-format` marker can disagree with the stored/computed value and adds another migration state. The label itself is sufficient: non-empty means persisted ID, empty means legacy fallback.

### 20.5 Global response-format switch without persisted state

A global switch can make the same Sandbox alternate IDs during rollout or configuration changes. Persisting the chosen ID prevents that ambiguity.

### 20.6 Permanent dual legacy/short aliases

Aliases simplify some rollout sequences but violate the one-ID-per-Sandbox requirement, complicate route cleanup, and prolong legacy coupling. The disabled-by-default assignment gate provides the required rollout control without aliases.

### 20.7 Read-time label validation

Validation appears defensive but creates divergent readers and recovery ambiguity after state is persisted. The label is therefore trusted as authoritative, with validation limited to new-value generation.

### 20.8 A different qualified key for the Sandbox label

Using a new string such as `sandbox-short-id` would avoid a same-string label/annotation pair, but the existing Checkpoint annotation and new Sandbox label both represent SandboxID. Kubernetes keeps label and annotation maps separate, and explicit constants plus no cross-kind fallback prevent ambiguity. Retaining `agents.kruise.io/sandbox-id` also preserves the operator-facing selector chosen for this feature.

### 20.9 Separate route implementations in manager and gateway

Duplicating ObjectKey indexes, resource-version fencing, peer compatibility, and targeted repair in two components would make identity-transition bugs likely. A neutral shared implementation keeps the two required physical stores behaviorally consistent while leaving event-source and state-policy differences in thin component adapters.

### 20.10 Input validation without callback guards

Rejecting the key only in E2B and SandboxClaim would address known callers but would let a later internal modifier bypass the invariant. Making pre-lock modifiers error-returning and guarding both callback stages in manager core prevents persistence at the mutation boundary without teaching infra the sandbox-ID key.

### 20.11 Sandbox-ID-specific protection in infra

Infra could directly reject `agents.kruise.io/sandbox-id` changes, but that would make a backend-neutral layer own manager identity policy. The selected design keeps infra callbacks generic: manager core supplies the guard, while API/controller code receives only the shared key constant needed for public validation and recycle preservation.

### 20.12 Periodic full-store repair

A periodic direct API-server List could provide a collection snapshot, but its cost scales with hundreds of thousands of Sandboxes and every manager/gateway replica even when no route is ambiguous. An informer-backed scan avoids API-server traffic but is not authoritative enough to replace a cross-UID record whose RV is equal or unorderable. The selected design keeps informer List/Watch as the normal event source and performs asynchronous direct Gets only for ObjectKeys that the Store identifies as ambiguous.

### 20.13 Retaining `infra.Sandbox.GetRoute()`

Injecting projection into the concrete infra wrapper could remove direct format logic while retaining the method, but it would leave Route construction and ID selection hidden behind the infra abstraction. Removing both `GetRoute()` and `GetSandboxID()`, adding only `GetPodIP()`, and letting the existing `infra.Sandbox` satisfy `sandboxroute.ProjectionSource` keeps the ownership boundary explicit without adding a route-specific Infra method.
