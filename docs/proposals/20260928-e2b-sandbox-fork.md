---
title: E2B-Compatible Running Sandbox Fork
authors:
  - "@ywExcellent"
creation-date: 2026-09-28
last-updated: 2026-10-20
status: provisional
see-also:
  - "/docs/proposals/20260915-checkpoint-api-extension.md"
---

# E2B-Compatible Running Sandbox Fork

## API Changes

This proposal adds E2B-compatible endpoints:

```http
POST /sandboxes/{sandboxID}/fork
POST /kruise/api/sandboxes/{sandboxID}/fork
```

The first is the native E2B path and the second is the customized path. They
MUST be behaviorally identical.

### Request

The request body is optional:

```json
{
  "timeout": 60,
  "count": 2
}
```

| Field | Type | Default | Validation | Meaning |
|---|---:|---:|---|---|
| `timeout` | integer | 15 | `1 <= timeout <= maxTimeout` and the seconds value fits `time.Duration` | Lifetime of each new Sandbox, in seconds. |
| `count` | integer | 1 | `1 <= count <= 100` | Number of forks to attempt from one checkpoint. |

The Go request model MUST use pointers to distinguish omitted fields from
explicit zero values:

```go
type ForkSandboxRequest struct {
    Timeout *int `json:"timeout,omitempty"`
    Count   *int `json:"count,omitempty"`
}
```

`timeout: 0` and `count: 0` are invalid. Fork timeout validation is independent
from the existing create API and MUST NOT silently inherit its 30-second
minimum timeout. The [upstream OpenAPI schema](https://github.com/e2b-dev/E2B/blob/a1ad3266d7088eb0dcaf720adf6665ab59be12c4/spec/openapi.yml#L1198-L1206)
declares a minimum timeout of zero, but the [E2B Runtime validator](https://github.com/e2b-dev/runtime/blob/e46b165d8d7e9e8dc28587a3c90ce9e5f20327cb/packages/api/internal/handlers/timeout_helper.go#L18-L42)
rejects explicitly supplied values less than or equal to zero. This endpoint
follows Runtime behavior; zero does not mean an unlimited lifetime.

### Success response

The endpoint returns `201 Created` only after the source checkpoint succeeds
and every fork has completed its creation attempt. The result array MUST have
exactly `count` entries in request-slot order and permits partial success:

```json
[
  { "sandbox": { "sandboxID": "sbx-new-a" } },
  { "error": { "code": 429, "message": "sandbox concurrency quota exceeded" } }
]
```

Each result MUST contain exactly one of `sandbox` or `error`:

```go
type ForkSandboxResult struct {
    Sandbox *Sandbox       `json:"sandbox,omitempty"`
    Error   *ForkItemError `json:"error,omitempty"`
}

type ForkItemError struct {
    Code    int    `json:"code"`
    Message string `json:"message"`
}
```

A successful item returns the complete E2B `Sandbox` representation, including
new credentials issued by the existing create/clone contract, not only the
`sandboxID`.

### Request-level errors

| HTTP status | Meaning |
|---:|---|
| 400 | Malformed request body or invalid timeout/count. |
| 401 | Authentication failed. |
| 404 | The source Sandbox is absent, terminal, or belongs to another tenant. Ownership mismatch MUST remain indistinguishable from absence. |
| 409 | The source cannot be forked in its current state, including Paused, or another checkpoint of the same Sandbox is in progress. |
| 429 | Request-level rate limiting. |
| 500 | An unexpected control-plane, checkpoint, or transient backend error occurred. |

After checkpoint success, individual clone failures are written into the `201`
result array. Quota exhaustion maps to `429`, conflicts map to `409`, and other
failures — including transient backend errors — map to
`500`. Fork quota exhaustion MUST NOT reuse the existing create API's `403`
mapping.

### Sandbox annotation

This proposal adds one internal Sandbox annotation:

| Key | Writer | Meaning |
|---|---|---|
| `agents.kruise.io/checkpoint-lock` | sandbox-manager sets it before creating a checkpoint and releases it afterwards; recycle clears it | Holder of the checkpoint in progress on this Sandbox. Absent means unlocked. |

```json
{
  "holder": "4f1c2b9e-8d3a-4c51-9f0e-2a7b6c5d4e3f",
  "operation": "fork",
  "expireTime": "2026-10-20T08:02:00Z"
}
```

`holder` is unique per acquisition, `operation` names the operation that
requested the checkpoint (`fork` or `snapshot`) and is reported in conflict
errors, and `expireTime` is the RFC 3339 UTC time after which the lock is free.
The key and its JSON value type are defined in `api/v1alpha1`, like
`UpdatedMetadataInClaim`, rather than in a sandbox-manager package; Infra
returns and accepts this type directly:

```go
type CheckpointLock struct {
    Holder     string      `json:"holder"`
    Operation  string      `json:"operation"`
    ExpireTime metav1.Time `json:"expireTime"`
}
```

The key is distinct from `agents.kruise.io/lock`, which identifies a claim
delivery. Users and other components MUST NOT write it.

### Snapshot conflict

| Endpoint | Change |
|---|---|
| `POST /sandboxes/{sandboxID}/snapshots` | Returns `409` when another checkpoint of the same Sandbox, from a fork or another Snapshot, is in progress. Concurrent Snapshots previously each created their own Checkpoint CR. |

The Snapshot handler adds the mapping from Manager conflicts to `409` and
passes the operation name `snapshot` to the Manager. Upstream E2B waits for an
observed concurrent `Snapshotting` transition before returning `409`; this
endpoint returns `409` as soon as it observes an active checkpoint lock.

### Fork source lookup failures

Both fork source lookups are bounded by two seconds. A lookup deadline or a
transient backend error returns `500`, the same mapping every other E2B
endpoint uses; absent or foreign
Sandboxes remain indistinguishable `404` responses. Snapshot checkpoint-lock
retry exhaustion also continues to return `500`.

Using an internal fork checkpoint ID with the ordinary Sandbox create endpoint
is an invalid request and returns `400`, not a control-plane failure.

## Background and Motivation

E2B `Sandbox.fork` branches an initialized running microVM without repeating
setup. Every child in one request MUST come from the same running-state point,
while receiving an independent identity, lifecycle, credentials, writable root
layer, and quota reservation.

OpenKruise Agents already checkpoints running Sandboxes and clones from a
Checkpoint. Fork adds a Manager use case that creates one private checkpoint per
batch and fans out through the existing clone admission path. It MUST NOT compose
the Snapshot and create HTTP handlers: doing so exposes internal artifacts,
cannot guarantee one checkpoint per batch, and leaves cancellation and cleanup
ownership in the API layer.

### Goals

- Implement the E2B fork HTTP contract, including defaults, batch results,
  request-level errors, and native/customized path equivalence.
- Ensure all successful children of one request use one immutable checkpoint.
- Preserve source identity, quota reservation, and lifecycle deadline after a
  successful fork.
- Give each child a new Sandbox ID, route identity, credentials, lifecycle,
  quota reservation, and independent writable root filesystem layer.
- Serialize every checkpoint that sandbox-manager creates on one Sandbox,
  including fork, Snapshot, and future checkpoint features, through a checkpoint
  lock stored on that Sandbox, without changing the concurrency semantics of
  pause, resume, or delete.
- Keep fork checkpoints private to their source Sandbox and reclaim each
  batch's checkpoint when the fork flow ends, with the TTL covering only a
  crash between creation and cleanup or a failed deletion.
- Preserve the `API -> Manager -> Infra` layering.
- Reuse the project's established running-state checkpoint/restore capability
  without changing its backend contract.

### Non-Goals

- Implementing Linux `fork(2)` semantics.
- Guaranteeing that existing TCP, HTTP/2, WebSocket, or other long-lived source
  connections survive checkpointing.
- Duplicating external persistent-volume data.
- Adding an idempotency key not defined by upstream E2B.
- Changing public Snapshot retention or listing, general Checkpoint retention,
  or existing create API semantics. The only Snapshot change is the concurrent
  checkpoint conflict.
- Applying the checkpoint lock to controller-side checkpoints for pause or
  upgrade.
- Adding Prometheus metrics.

## Current Implementation Constraints

Fork reuses the existing Checkpoint CR, owned SandboxTemplate, and clone path;
it adds no checkpoint backend. Each fork checkpoint is a new full-memory
artifact, so every finished batch needs exact deletion, with a bounded TTL
covering only crashes and failed deletions. Clone creation can outlive an
ambiguous Kubernetes response, so
fork also retains the existing orphan cleanup and limits concurrent children.

## Proposal

### Checkpoint Reuse and Inheritance

Fork uses the existing checkpoint/restore implementation with
`KeepRunning=true`. Each request creates or refreshes an internal fork
checkpoint owned by the source Sandbox. Every child in that request restores
from the same immutable checkpoint, while the source is restored through the
existing checkpoint lifecycle. A later fork creates a new checkpoint from the
source's current state rather than reusing stale running state.

A fork checkpoint is private and never exposed as a public Snapshot. When the
fork flow ends — success, partial failure, or cancellation — the Manager deletes
that batch's checkpoint by its exact checkpoint ID. A manager crash between
checkpoint creation and flow end, or a failed deletion, leaves the artifact
until its one-hour checkpoint TTL expires; source
Sandbox deletion cascades to remaining fork checkpoints through owner
references. Generic
checkpoint cleanup and resume selection MUST ignore fork checkpoints.

Each child receives a new Sandbox identity, lifecycle deadline, quota
reservation, runtime credentials, traffic token, and writable root filesystem
layer. A preparation deadline protects the child while it starts; after Ready,
runtime initialization, token issuance, and CSI mounts complete, the Manager
atomically writes the requested lifecycle deadline and restores its inherited
auto-pause policy. Auto-pause children inherit the source's resolved
paused-retention annotation. Source credentials MUST NOT be copied through
Checkpoint annotations, template metadata, or response models.

Fork reads the source TrafficPolicy and the normalized L7 rules in
`agents.kruise.io/security-rules` inside the checkpoint lock, and restores both
on each child before it is created. L7 rules such as domain blocking and header
injection may exist without a TrafficPolicy and MUST NOT be lost. This does not
add L7 rules to the general checkpoint annotation allow-list or change ordinary
Snapshot/clone inheritance.

Auto-pause/auto-resume, runtime configuration, CSI mount configuration, resource
shape, and workload-identity inputs follow the existing checkpoint inheritance
behavior. Fork does not redefine the checkpoint backend persistence contract.

External-volume data is excluded from the isolation guarantee. Fork inherits
mount configuration, while whether children access the same external data is
defined by the volume's own semantics.

### Sandbox Checkpoint Lock

Checkpoints of one Sandbox run one at a time. Every sandbox-manager operation
that creates a checkpoint MUST first acquire that Sandbox's checkpoint lock.
Fork and E2B Snapshot are its current users; future checkpoint features go
through the same Manager entry point and get mutual exclusion without separate
integration. The lock covers checkpoints only: pause, resume, and delete do not
acquire it and are never blocked by a held lock.

The lock is stored on the checkpointed Sandbox as the
`agents.kruise.io/checkpoint-lock` annotation defined in API Changes, not on a
Checkpoint CR. Each Checkpoint CR is newly created with a generated name that
also becomes its SandboxTemplate name and Snapshot ID, so no fixed Checkpoint
object exists to lock atomically. The lock lives and dies with its Sandbox, so
it needs no separate object or garbage collection, and it is visible wherever
the Sandbox is inspected.

| Step | Behavior |
|---|---|
| Acquire | Read the Sandbox from the informer cache. An unexpired lock held by another holder fails with a conflict naming its operation. Otherwise, write a new value with a merge patch carrying the observed `resourceVersion` as a precondition. A write conflict re-reads the informer view with bounded backoff and re-evaluates. An absent, expired, or unparsable value is replaced. |
| Use | The successful patch response is the latest Sandbox on the API server. The caller runs its in-lock checks on that object without a separate read. |
| Release | A JSON patch tests that the annotation still equals the holder's value and removes it. When the patch fails, the informer view decides the outcome: a value that is no longer the holder's means the lock expired and was taken over, so release completes as a no-op. Any other failure leaves the lock to expire. |
| Expire | `expireTime` is fixed at acquisition and is never renewed. After a holder crashes, the next checkpoint of that Sandbox waits until then. |

Each acquisition derives its TTL from the checkpoint's own wait bound and is
never renewed:

```text
TTL = checkpoint wait timeout
    + 30s abandoned-checkpoint cleanup
    + 30s for release latency and clock skew between sandbox-manager replicas
```

| Caller | Checkpoint wait timeout | Lock TTL |
|---|---|---|
| Fork | 60 seconds, fixed | 120 seconds |
| Snapshot | `snapshot-wait-success-seconds` header, default 60 seconds, unbounded | 120 seconds by default; 360 seconds for a 300-second wait |

Every holder MUST finish its protected work, including detached cleanup, within
the TTL. A crashed sandbox-manager blocks the next checkpoint of the same
Sandbox for at most one TTL. In normal operation the lock is released as soon
as the checkpoint completes or fails.

Lock state never reaches children or the next tenant. Checkpoints copy only
allow-listed Sandbox annotations and SandboxTemplates copy only spec fields, so
Sandboxes created from a checkpoint never inherit the source's lock, and recycle
clears the key together with the other per-delivery annotations. Acquisition
and a failed release read only the informer cache, so neither reads the API
server directly. Each acquisition and release is one Sandbox write: the
Sandbox controller reconciles once, and Sandbox informer consumers observe an
update that leaves route and quota content unchanged.

Each replica judges the lock from its own informer view, preferring an object it
wrote itself when that object is newer. A request served by another replica just
after a release can still observe the released lock and receive `409`; callers
retry it like any other conflict.

Controller-side checkpoints for pause and upgrade do not take part. They follow
a different path that creates the Checkpoint CR and polls its status across
reconciles without a timeout, and the controller must not depend on
sandbox-manager packages. Defining the annotation key and value type in
`api/v1alpha1` keeps the lock directly readable and writable by the controller.

### Layered Architecture

```text
E2B API
  parse and validate E2B requests; authenticate; map responses
       |
       v
Sandbox Manager
  run every checkpoint through one entry point that holds the Sandbox
  checkpoint lock; create a source-owned running checkpoint; run bounded
  concurrent clones; collect item results
       |
       v
Sandbox Infra
  maintain the Sandbox checkpoint lock annotation; create source-owned
  checkpoints without locking; precisely clean completed fork batches and clean
  remaining artifacts on source deletion; execute Runtime operations; implement
  clone; operate Kubernetes resources
```

#### API layer

Add `fork.go`, request/result models, and route registration under
`pkg/servers/e2b`. The handler MUST:

1. Parse the optional JSON body and reject unknown fields.
2. Apply E2B defaults, validate timeout/count, and convert the accepted timeout
   from protocol seconds to `time.Duration`.
3. Resolve the authenticated user and namespace.
4. Call one Manager method.
5. Convert successful neutral Sandboxes through `convertToE2BSandbox`.
6. Map Manager request and item results to the public E2B contract.

The handler MUST NOT read or write Checkpoint CRs, call an Infra implementation
directly, or internally call the `CreateSnapshot` or `CreateSandbox` handlers.

The API-key middleware continues to establish ownership without exposing a
foreign Sandbox. The fork handler must query a sufficiently broad state set to
distinguish Paused from absent, mapping Paused to `409` rather than allowing a
live-only lookup to collapse it to `404`.

#### Manager layer

`SandboxManager.CreateCheckpoint` is the only sandbox-manager entry point that
creates a checkpoint; E2B Snapshot already calls it. Other Manager code MUST NOT
call the Infra `CreateCheckpoint` directly.

```go
type CreateCheckpointOptions struct {
    // Operation names the requester; it is written into the lock and reported
    // in conflict errors.
    Operation string
    Infra     infra.CreateCheckpointOptions
    // BeforeCheckpoint optionally runs inside the lock on the Sandbox returned
    // by acquisition; an error aborts before any checkpoint is created.
    BeforeCheckpoint func(ctx context.Context, sbx infra.Sandbox) error
}

func (m *SandboxManager) CreateCheckpoint(
    ctx context.Context,
    sbx infra.Sandbox,
    opts CreateCheckpointOptions,
) (string, error)
```

The entry point:

1. Derives the TTL from `Infra.WaitSuccessTimeout`, or 60 seconds when unset,
   and acquires the checkpoint lock with `Operation`.
2. Runs `BeforeCheckpoint`, when set, on the Sandbox returned by acquisition.
3. Calls the Infra `CreateCheckpoint` and waits for completion. Steps 2 and 3
   run under a deadline equal to the wait timeout, which leaves the cleanup
   allowance and the margin of the TTL for detached cleanup and release.
4. Releases the lock on a detached bounded context, whether the checkpoint
   succeeded or failed.

Snapshot calls it with operation `snapshot` and no `BeforeCheckpoint`; apart
from the concurrent-checkpoint `409`, its behavior is unchanged.

Add a protocol-neutral Manager use case for fork:

```go
type ForkSandboxOptions struct {
    Namespace string
    User      string
    SandboxID string
    Timeout   time.Duration
    Count     int
    Quota     *quotaspec.QuotaSpec
}

type ForkSandboxItemResult struct {
    Sandbox infra.Sandbox
    Err     error
}

func (m *SandboxManager) ForkSandboxes(
    ctx context.Context,
    opts ForkSandboxOptions,
) ([]ForkSandboxItemResult, error)
```

The Manager treats a positive `Timeout` as a business invariant and rejects a
zero or negative duration with `ErrorBadRequest`, even when called outside the
E2B handler. It never depends on a protocol-layer validation for this invariant.

The Manager is responsible for:

1. Looking up the source in tenant scope and evaluating its lifecycle state. A
   source that the informer view already shows as not Running is rejected
   before any lock write.
2. Creating the checkpoint through the Manager entry point with operation
   `fork` and the 60-second wait, which yields a 120-second lock.
   `BeforeCheckpoint` re-validates source identity, recycle state, and Running
   state on the Sandbox returned by the acquisition, then reads the source
   network policy. Only another checkpoint of the same Sandbox causes the lock
   conflict. The implementation MUST NOT use an API-process-local mutex or add a
   recoverable batch task for fork.
3. Creating or refreshing a source-owned running-state fork checkpoint with
   `KeepRunning=true`.
4. Fanning out child clones through a bounded worker pool. The public batch
   limit is 100; worker count is an internal Manager option protecting Runtime
   and Kubernetes create capacity. E2B Runtime currently launches up to 100
   children with unbounded `errgroup.Go`; bounded scheduling changes only
   internal start concurrency, not the request, response ordering, or partial
   success semantics.
5. Sending every child through the existing clone admission path so it receives
   an independent quota reservation, Sandbox ID, create-limit admission, route,
   and credentials.
6. Preserving result order through stable input indexes.
7. Waiting for all started workers and collecting item results.
8. Starting child clones only after the entry point returns, by which time the
   checkpoint lock is released. Source Sandbox delete/expiry cleans up its fork
   checkpoint.

A request canceled before its checkpoint starts takes no lock and creates no
checkpoint. Once the checkpoint starts, the system stops unstarted work when
safe, while source recovery continues in a detached bounded context.
Cancellation after checkpoint success does not prove that no child was created.
The request is not idempotent; callers that receive an uncertain result MUST
inspect existing resources before retrying.

The API layer owns E2B defaults and HTTP mapping. The Manager accepts only
validated neutral duration/count values and returns typed domain errors; it MUST
NOT import E2B request, response, or HTTP types.

#### Infra layer

Add a source-owned fork checkpoint contract on top of the existing checkpoint
and clone capabilities:

```go
type ForkCheckpointOptions struct {
    KeepRunning        bool
    PersistentContents []string
}

type ForkCheckpoint struct {
    ID        string
    Namespace string
    Name      string
}
```

Infra MUST:

- Implement the Sandbox checkpoint lock on the neutral Sandbox interface:

  ```go
  type CheckpointLockOptions struct {
      Operation string
      TTL       time.Duration
  }

  // Added to infra.Sandbox. A successful acquisition refreshes the Sandbox to
  // the object returned by the API server.
  AcquireCheckpointLock(ctx context.Context, opts CheckpointLockOptions) (v1alpha1.CheckpointLock, error)
  ReleaseCheckpointLock(ctx context.Context, lock v1alpha1.CheckpointLock) error
  ```

  A lock held by another holder returns a typed conflict error naming its
  operation; exhausted write-conflict retries return an internal error.
  The Infra `CreateCheckpoint` itself does not lock. Fork validates the source
  on the Sandbox returned by acquisition instead of a separate API-server read.
- Create and wait for a fork checkpoint for the source Sandbox using the
  existing running-state checkpoint implementation.
- Create children from that checkpoint through the existing neutral clone path.
- Delete a source's fork checkpoint when the source Sandbox is deleted or
  expires.

The implementation may reuse `Checkpoint` CRs and `SandboxTemplate`, but a
fork checkpoint MUST have an internal purpose label and an explicit ownership
relationship to its source Sandbox. It MUST NOT appear in E2B `GET /snapshots`,
be accepted as a user template, or be selected by unrelated pause or upgrade
flows. A public clone attempt against a fork checkpoint is an invalid request and
returns `ErrorBadRequest` immediately after checkpoint lookup, before reading
its SandboxTemplate; authorization failures continue to use `ErrorNotAllowed`.
The label is an implementation selector, not an authorization boundary;
namespace and owner checks remain mandatory.

When the fork flow ends — success, partial failure, or cancellation — the
Manager MUST delete only that batch's fork checkpoint, matched by both source
UID and checkpoint ID. Infra
finds it through the informer-backed checkpoint index and deletes it with UID
and resourceVersion preconditions; it does not read the SandboxTemplate or fall
back to the APIReader. The template is controlled by its checkpoint and is
reclaimed by garbage collection. A cache miss, failed deletion, or manager
crash between checkpoint creation and flow end leaves the artifact to its
one-hour `ttlAfterFinished` fallback. Source deletion or expiry MUST also delete
fork checkpoints and cascade deletion of their owned SandboxTemplates.

Recycle does not clean fork checkpoints in this proposal. The recycle
lifecycle's content reset is not yet complete (the recycler is currently a
no-op), and recycle is not enabled by the E2B create path, so the one-hour TTL
already bounds any checkpoint left behind by a recycled source. When recycle is
completed, it MUST batch-delete fork checkpoints for the source UID before the
Sandbox returns to the pool; until then the TTL fallback covers the gap.

### Operation Flow

```mermaid
sequenceDiagram
    participant C as E2B client
    participant A as E2B API
    participant M as Sandbox Manager
    participant I as Sandbox Infra
    participant R as Runtime/checkpoint backend

    C->>A: POST /sandboxes/{id}/fork
    A->>A: authenticate, validate timeout/count
    A->>M: ForkSandboxes(source, TTL, count, quota)
    M->>I: acquire source checkpoint lock (fork, 120s)
    M->>I: create source-owned fork checkpoint(keepRunning=true)
    I->>R: capture state, recover source
    R-->>I: immutable checkpoint ready
    M->>I: release source checkpoint lock
    par bounded workers
        M->>I: clone child from same checkpoint
        I-->>M: Sandbox or typed error
    end
    M-->>A: indexed item results
    A-->>C: 201 [Sandbox | Error]
```

### State and Failure Semantics

The source checkpoint lock has these conceptual states:

```text
Unlocked -> Held by one checkpoint (fork or Snapshot) -> Unlocked
                          \-> Expired after manager crash -> replaced by the next acquisition
```

On one Sandbox, fork and fork, fork and Snapshot, and Snapshot and Snapshot are
mutually exclusive; the later request returns `409`. Pause, resume, and delete
retain their existing semantics and can race with a checkpoint. The lock is
released when an in-lock check or the checkpoint fails. Fork releases it before
child clones begin, so child clones do not hold a source lock. The lock's
`resourceVersion` precondition gives atomic coordination across Manager
replicas without an API-process-local mutex. A manager crash leaves the lock to
expire; it does not create a recoverable or replayable `ForkOperation`.

| Phase | Failure outcome |
|---|---|
| Before checkpoint | Return a non-201 request error; do not start children. |
| Checkpoint / source recovery | Return a request error. Restore a healthy source; otherwise use existing terminal lifecycle cleanup. |
| Individual clone | Record a typed item error and continue other workers. |
| Source Sandbox deletion or expiry | Delete the source-owned fork checkpoint and its SandboxTemplate. |
| Request cancellation | Stop unstarted children; never abandon source recovery only because the HTTP client disconnected. |
| sandbox-manager restart | Do not resume or replay the old batch. Successfully created children remain; unfinished child results may be unknown. |

After checkpoint submission, source recovery uses a detached bounded context;
child clones keep the request context, so unstarted work stops on disconnect.
A restart neither resumes nor replays a batch: the lock expires, checkpoint TTL
handles abandoned artifacts, and created children remain.

A caller without the complete `201` result MUST treat the outcome as unknown and
inspect existing Sandboxes before retrying. Started-child failures remain item
errors in an ordered partial-success `201`, never an overall request error.

### Security Rules

Lookup and artifacts are tenant-scoped; a foreign source returns the same `404`
as an absent source. Children get new runtime credentials, traffic tokens, and
workload identity. Unsupported inheritance fails before checkpoint creation,
and external volumes keep their own sharing semantics rather than becoming
copy-on-write.

## Compatibility and Upgrade Strategy

The endpoint is additive. Existing E2B paths, public Snapshots, and create
behavior remain unchanged, except that a Snapshot concurrent with another
checkpoint of the same Sandbox returns `409`, matching upstream E2B.

The endpoint directly reuses the project's supported checkpoint/restore path.
Rollback is safe: disabling the fork route stops new requests without changing
existing Sandbox or Checkpoint data.

If fork-checkpoint labels or annotations are added, older controllers may ignore
them. Before rollout, all active controller/backend versions MUST understand the
source-owned checkpoint cleanup and isolation contract.

The checkpoint lock annotation is additive and is ignored by components that do
not acquire it. During a sandbox-manager rolling upgrade, old replicas create
checkpoints without the lock, so mutual exclusion takes full effect once every
replica is upgraded. A controller that predates recycle clearing of the key may
return a Sandbox to its pool with a stale lock; the lock expires, and the next
acquisition replaces it.

## Test Plan

Unit tests pin the HTTP contract and error mapping; Manager validation,
cancellation, ordered partial results, policy inheritance, and child cleanup;
lock acquisition/release races; private-checkpoint visibility and guarded
cleanup; credential rotation; and controller exclusion of fork checkpoints.

End-to-end coverage verifies one checkpoint per batch, source recovery,
independent child identity/credentials/quota/lifecycle, partial `201` results,
the 100-item bound, fork/Snapshot conflicts, private artifact visibility, root
filesystem isolation, and documented external-volume semantics.
