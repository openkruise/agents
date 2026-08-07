---
title: Short and Stable Sandbox IDs
authors:
  - "@AiRanthem"
reviewers: []
creation-date: 2026-07-11
last-updated: 2026-08-05
status: implemented
---

# Short and Stable Sandbox IDs

## Summary

OpenKruise Agents historically identified a Sandbox as:

```text
<namespace>--<sandbox-name>
```

This value is readable, but its length grows with both Kubernetes names. E2B-compatible traffic
addresses embed the Sandbox ID in a DNS name, so a valid namespace and Sandbox name can still
produce an address that exceeds DNS limits.

This proposal introduces an optional short Sandbox ID:

```text
<operator-prefix><13-character Snowflake encoding>
```

The suffix encodes a 63-bit Sonyflake value. Sandbox-manager obtains a non-reused 20-bit worker
incarnation from a prefix-scoped Kubernetes Lease during startup, then stores generated IDs in the
Sandbox label `agents.kruise.io/sandbox-id`. A non-empty label becomes that Sandbox CR's
authoritative identity for its current user delivery.

The change is deliberately incremental:

- an unlabeled Sandbox continues to use its legacy ID;
- enabling the feature assigns a new ID in each claim lock Update/Create or clone Create, before
  admission and heavyweight post-processing;
- claiming a recycled Sandbox replaces its prior delivery ID, while other operations within one
  delivery preserve the current non-empty label;
- one Sandbox has one active ID at a time; legacy and short IDs are not simultaneous aliases;
- client-provided IDs remain opaque and are never decoded to recover a Kubernetes object.

This trades a startup-time Lease allocation protocol for atomic delivery identity persistence and
a shorter ID. Delivery-scoped stability and version-ordered routing behavior remain the core of the
design.

## Motivation

Changing only the ID returned by the create API would leave caches, routing, peer synchronization,
Checkpoints, pagination, and E2B diagnostics with conflicting views of the same Sandbox. A safe
short-ID design therefore needs to answer four broader questions:

1. Where is the selected identity persisted?
2. How does an existing Sandbox move from its legacy ID without exposing two aliases?
3. How do independent manager and gateway processes reject delayed route events?
4. How can operators diagnose an opaque ID without weakening tenant isolation?

### Goals

- Keep Sandbox IDs short enough for normal E2B dynamic hostnames.
- Preserve legacy behavior for unlabeled Sandboxes without a background migration.
- Make the selected ID stable within one delivery and replace it when a recycled Sandbox is
  delivered again.
- Keep assignment policy in sandbox-manager and infrastructure concerns policy-neutral.
- Ensure cache and route transitions never expose both IDs in one current view.
- Fail closed when cache lookup observes duplicate IDs.
- Support a staged manager/gateway rollout before short-ID assignment is activated.
- Let operators locate a labeled Sandbox directly with `kubectl`.
- Restore namespace/name diagnostics only after authorization succeeds.
- Allocate manager worker incarnations without reusing a worker ID within one prefix domain.
- Provide an explicit prefix-switch recovery procedure for worker exhaustion and etcd restore.

### Non-Goals

- Giving one Sandbox permanent legacy and short aliases.
- Migrating all existing Sandboxes in the background.
- Rewriting IDs already stored on Checkpoints.
- Making short IDs reversible to namespace and name.
- Treating a Sandbox ID alone as proof of the current owner without the normal authorization path.
- Removing the `--` namespace restriction while legacy IDs remain supported.
- Repairing or normalizing a non-empty persisted label during reads.
- Supporting administrator-written, copied, or otherwise forged reserved labels and Routes.
- Rolling back to label-unaware binaries after short-ID assignment has begun.
- Reusing or resetting a prefix's worker counter after stopping old processes.
- Eliminating the duplicate-ID risk after restoring the cluster to an older etcd snapshot while
  continuing with the same prefix.

## Identity Model

### One authoritative value

Sandbox ID resolution has exactly two branches:

| Sandbox metadata | Resolved ID |
|---|---|
| `agents.kruise.io/sandbox-id` is non-empty | Return the label unchanged |
| The label is absent or empty | Return `<namespace>--<name>` |

A non-empty label is the persisted identity of the current delivery. Readers do not revalidate its
format, length, relationship to the UID, or origin. Revalidating on every read could make different
binaries disagree about an identity that has already been stored.

The assignment flag controls the identity written for a new delivery. It never changes how an
active Sandbox is read between delivery operations:

| Assignment | Unlabeled pooled Sandbox | Previously labeled pooled Sandbox |
|---|---|---|
| Disabled | Uses its legacy ID | Prior label is retired and the new delivery uses its legacy ID |
| Enabled | Receives a new short ID | Prior label is replaced with a new short ID |

### Delivery transition

The normal state transition is:

```text
pooled / unlabeled  ->  claimed / new short ID
claimed / short ID  ->  recycled / unlabeled
```

The ID identifies one user delivery, not the reusable Sandbox CR. Pause, resume, update, and
Checkpoint operations within that delivery preserve it. Successful recycle retires the ID before
the Sandbox returns to the pool, and a later Claim assigns the next delivery independently.
Authorization and external systems must still verify the current owner instead of treating ID
knowledge as authorization.

### Reserved metadata

The selected ID is stored as:

```yaml
metadata:
  labels:
    agents.kruise.io/sandbox-id: aae57hpxaaqac
```

This label is owned by sandbox-manager. Public inputs and internal extension callbacks cannot add,
change, or delete it. Pool and template materialization must not copy it into a new Sandbox, while
recycle clears it when the current delivery ends.
E2B-compatible requests reject any user-supplied label under the internal `agents.kruise.io/`
prefix space, not only the sandbox-id key, so no request can forge system-owned metadata.

The same qualified string is also used as a Checkpoint annotation. The two metadata kinds remain
strictly separate:

- the Sandbox label is the Sandbox's current authoritative ID;
- the Checkpoint annotation records the source Sandbox ID at Checkpoint creation time.

Readers never fall back from one metadata kind to the other.

Out-of-band writes to the reserved label are outside the supported protocol. Cache lookup still
detects duplicate resolved IDs and fails instead of choosing an arbitrary Sandbox. Routing assumes
that supported IDs were generated by the system and are unique within the cluster.

## ID Format and Assignment

### Encoding

The short suffix is produced by `github.com/sony/sonyflake/v2` with this fixed bit layout:

| Field | Bits | Contract |
|---|---:|---|
| Time | 41 | Milliseconds since `2025-01-01 00:00:00 UTC` |
| Worker incarnation | 20 | Allocated once at manager startup within the prefix domain |
| Sequence | 2 | Up to four IDs per millisecond before the generator waits |

The two-bit sequence field limits each manager generator to four ID assignments per millisecond,
or approximately 4,000 assignments per second under sustained load. After consuming all four
sequence values in one millisecond, the generator waits for the next millisecond instead of
rejecting or rate-limiting the request. This is a per-manager Sandbox ID assignment throughput
limit, not an aggregate API QPS limit: only operations that create a new ID consume this capacity,
and managers use independent worker incarnations and generators.

The positive 63-bit value is placed in an eight-byte big-endian buffer and encoded with unpadded,
lowercase RFC 4648 Base32. The result is always 13 characters from `[a-z2-7]`.

For example:

```text
aae57hpxaaqac
```

The generator guarantees uniqueness only while its worker incarnation is not reused. It does not
read Kubernetes or choose replica identity; cross-process uniqueness comes from the startup Lease
protocol below.

### Prefix-scoped worker allocation

The prefix is the worker allocation domain. Sandbox-manager derives one Lease name per prefix:

```text
sandbox-manager-sandbox-id-worker-<first 24 hexadecimal characters of sha256(prefix)>
```

The Lease lives in sandbox-manager's system namespace. The first process creates it with
`leaseTransitions=0`. Later processes use its `resourceVersion` as a CAS, replace
`holderIdentity`, and increment `leaseTransitions`. The holder is a full process-generated UUID and
remains stable for that allocation attempt. The counter value is the 20-bit Sonyflake worker ID.

The allocator uses live API-server `Get` operations because informer staleness cannot safely
confirm a CAS result. It retries `Conflict` and `AlreadyExists` after re-reading the Lease. A
timeout or other ambiguous Create/Update result is also re-read:

- if the holder is this process, the Lease counter is accepted;
- if another holder is present, the possibly consumed value is not reused and allocation advances
  from the latest counter;
- if confirmation fails or the parent context is cancelled, startup fails.

A missing holder, missing counter, negative counter, or counter at or above `2^20` fails closed.
The Lease is never renewed, released, reset, or deleted. These fields are used as a persistent CAS
record, following the [Kubernetes Lease API](https://kubernetes.io/docs/reference/kubernetes-api/coordination-resources/lease-v1/),
not as liveness evidence.

### Optional prefix

Operators may prepend a prefix with `--short-sandbox-id-prefix`. No separator is inserted
automatically, so an operator who wants `prod-` must include the hyphen in the configured value.
The prefix defaults to empty.

A non-empty prefix:

- starts with a lowercase letter or digit;
- otherwise contains only lowercase letters, digits, and hyphens;
- does not contain the legacy ID separator `--`, keeping short and legacy ID spaces disjoint;
- is at most 50 characters, keeping the complete ID within the 63-character Kubernetes label-value
  limit.

For Native E2B dynamic domains of the form `<port>-<sandbox-id>.<domain>`, operators should keep
the prefix at 44 characters or fewer so a five-digit port, separator, and ID fit in one DNS label.
During a mixed-version disabled rollout it must remain at 37 characters or fewer so older managers
still accept the configuration.

The prefix is validated when sandbox-manager starts, even when assignment is disabled, and must be
consistent across replicas. Prefix changes affect the allocator domain and only future
deliveries; active delivery labels are not regenerated. Fixed 13-character suffixes make full ID
spaces for two distinct prefixes disjoint: unequal prefix lengths produce unequal total lengths,
while equal lengths differ within the prefix bytes.

### Assignment boundary

Short-ID assignment is disabled by default. With `--enable-short-sandbox-id=true`, every Claim or
Clone delivery receives its ID through the Manager-decorated ordinary Modifier. The caller
Modifier runs first, but any reserved-label mutation it makes is discarded. The Manager generator
then assigns `<prefix><13-character-suffix>`, replacing any ID from a previous delivery. When
assignment is disabled, Claim retires any previous label and uses the legacy ID.

The generator runs whenever the Manager-decorated Modifier runs. Each Infra retry that reaches the
Modifier therefore consumes a new candidate ID, and failed attempts may leave gaps in the generated
sequence. Only the value persisted by the successful lock Update/Create becomes the delivery's
authoritative identity; there is no request-level memoization.

Claim merges the label into its existing optimistic lock Update/Create. Clone adds it to the first
Sandbox Create. Generation therefore happens before admission, lock/Create, readiness, runtime
initialization, token issuance, and CSI work. A generation failure performs no Sandbox write, and
there is no identity-specific trailing Update or retry loop. All later token, Checkpoint, route, and
response processing observes the same persisted ID. A clone creates a new ID and never inherits the
source Sandbox or Checkpoint identity.

The design intentionally keeps no legacy alias during cache propagation. Internal observers may
briefly converge at different times, but each observed version of a Sandbox resolves to exactly
one ID.

## Responsibility Boundaries

The identity decision crosses several components, but each layer keeps one kind of responsibility:

| Boundary | Responsibility |
|---|---|
| API and controllers | Reject reserved metadata at public inputs and present protocol-specific responses |
| Sandbox-manager | Own per-delivery assignment, prefix policy, prefix-scoped Lease worker allocation, generator initialization, and Modifier decoration |
| Infrastructure | Persist generic Sandbox changes and expose neutral backend observations |
| `pkg/sandboxid` | Implement the fixed bit layout and opaque encoding for a caller-supplied worker incarnation |
| Shared routing | Apply protocol-neutral projection, ordering, replacement, and deletion semantics |
| E2B compatibility | Enforce legacy namespace constraints and present authorized diagnostics |

Sandbox-manager owns worker allocation because the Lease coordinates manager process identity,
not Sandbox backend state. Infrastructure neither chooses the ID format nor owns assignment
enablement. `pkg/sandboxid` does not read Kubernetes, allocate workers, or own replica
configuration. E2B does not generate or migrate IDs. Controller code clears the current ID when
recycle ends a delivery but does not generate the next ID or depend on its format.

Manager and gateway keep separate in-memory route stores because they are separate processes. They
nevertheless use the same routing semantics so an identity transition cannot behave differently
between the two components.

## Lookup and Routing Contracts

### Opaque lookup

Every consumer treats a Sandbox ID as an opaque exact-match value. No cache, route store,
authorization path, or server adapter reverse-parses a legacy ID to recover namespace and name.

The claimed-Sandbox cache indexes exactly one resolved ID per Sandbox. When a label update is
observed, the entry moves from the legacy key to the short key rather than retaining both. Zero
matches remain not-found; multiple matches fail closed with a descriptive ambiguity error.

At the manager boundary, all underlying lookup failures retain the existing not-found error
category and include the underlying lookup error in the public message. Duplicate-ID ambiguity
does not create a new transport status.

### Atomic identity replacement

Routing is ordered by Kubernetes object identity and resource version, not by interpreting the
Sandbox ID:

- every current route is tied to an explicit namespace/name ObjectKey;
- a newer observation for the same ObjectKey atomically retires its previous ID and activates its
  new ID within each physical store;
- an older or equal resource version cannot replace current state;
- deletion is also ObjectKey- and resourceVersion-ordered;
- a deletion watermark is retained for a bounded window so a delayed update cannot
  resurrect a removed route while the watermark is still held;
- a recreated object with the same namespace/name crosses the old deletion watermark with its
  newer cluster resource version.

The ordering contract assumes every accepted route producer projects Kubernetes metadata from the
same cluster. Cross-cluster, forged, or misrouted payloads are unsupported.

Supported peer mutations carry an explicit namespace/name and a valid resource version. Malformed
or partial mutations are rejected without changing route state; valid stale events are
acknowledged as idempotent no-ops. During the pre-activation rollout only, legacy ID-only peer
messages are acknowledged and ignored rather than admitted into the new route model.

Route feeds preserve all informer-visible, non-terminating Sandboxes, regardless of lifecycle
state. Traffic admission remains a separate concern and continues to require a Running Sandbox.
For peer synchronization, only the exact `dead` state represents deletion; other states update
route knowledge without making them traffic-eligible.

Route projection also preserves the existing security and compatibility behavior: runtime access
tokens take precedence over the legacy token source, traffic authentication is enabled only when
its annotation is exactly `true`, and tokens are always redacted from route logs.

### Deletion fencing and informer truth

Informer List/Watch is the authoritative route-state source. Namespace and selector filtering are
applied at the informer boundary. Deletion timestamp updates and normal delete events preserve
their resource version before the object is discarded.

A synthetic tombstone without a trustworthy resource version is handled as a best-effort delete:
if the route is currently known, its current version becomes the deletion watermark. This closes
the common stale-event path but cannot prove the final deletion version if a newer peer update
arrived in between. The residual risk is accepted rather than adding API-server reads or a route
repair loop.

Deletion watermarks are retained for at least ten minutes from first establishment. Later deletes
on an existing watermark may advance its resource version but do not refresh the deadline, so
tombstone replays cannot extend retention indefinitely. Upsert and delete mutations check for
expired watermarks at one-minute intervals and remove them before evaluating the current mutation.
An idle Store may retain expired watermarks until its next mutation, but cannot keep growing while
idle. At 10,000 unique deletions per minute this bounds the normal watermark set to about 100,000
entries and the pre-cleanup peak to about 110,000 entries per process. This avoids a separate
cleanup lifecycle while accepting that observations delayed past the retention window are no
longer fenced.

## User-Visible Behavior

### E2B diagnostics

Short IDs intentionally omit namespace and name. After Sandbox lookup and ownership authorization
succeed, E2B restores that context without changing identity semantics:

- successful metadata includes
  `e2b.agents.kruise.io/sandbox-resource: <namespace>/<name>`;
- downstream runtime, gateway, Checkpoint, and lifecycle errors append
  `sandboxResource=<namespace>/<name>`;
- user metadata cannot spoof either the Sandbox-ID label or the response-only resource key.

Not-found and unauthorized responses do not disclose namespace or name.

Operators can locate a supported short-ID Sandbox directly:

```shell
kubectl get sbx -A -l agents.kruise.io/sandbox-id=<sandbox-id>
```

### Checkpoints and pagination

A Checkpoint records the resolved source Sandbox ID at creation time. If that Sandbox is later
recycled and delivered again, existing Checkpoints keep the prior delivery ID and later
Checkpoints record the new delivery ID. No historical rewrite is performed.

Pagination uses the resolved ID only as an opaque uniqueness component. An identity transition may
change that component between list requests, like other mutable list state; the system does not
retain a second identity to stabilize pagination.

## Rollout and Rollback

The rollout protocol is a correctness precondition:

1. Deploy label-aware manager and gateway binaries with short-ID assignment disabled.
2. The two components may be rolled out in either order while assignment remains disabled.
3. Keep the prefix at 37 characters or fewer while old managers remain in the rollout.
4. Drain old replicas and their in-flight or retry peer traffic.
5. Verify every relevant informer handler is synchronized.
6. Enable assignment on sandbox-manager. Each new manager first completes synchronous setup,
   peer startup, and cache startup; it then allocates one worker and initializes its generator
   before the proxy and E2B HTTP server can accept traffic.

During the initial disabled rollout, new receivers ignore old ID-only peer route messages and rely
on their own informer for convergence. A brief missing or stale route is acceptable during this
window. This compatibility behavior is only for reaching the activation point; old senders are not
supported after assignment begins.

Once any short label has been persisted, rolling back to a binary that ignores the label is unsafe.
Such a binary would reconstruct the legacy ID and disagree with persisted identity.

Turning `--enable-short-sandbox-id` off remains safe as a way to stop new short assignments, but it
is not a data rollback:

- active labeled Sandboxes remain short until their delivery ends;
- recycle clears their labels before returning them to the pool;
- later disabled Claims use the legacy ID.

Because a legacy ID is derived from the reusable namespace/name, disabled mode does not provide a
distinct ID for each reuse of the same Sandbox CR. Per-delivery uniqueness therefore requires
short-ID assignment to remain enabled after activation.

Removing legacy compatibility is a separate future change after operators confirm that no
supported unlabeled Sandboxes remain.

### Worker exhaustion and disaster recovery

One prefix supports 1,048,576 manager process incarnations. CrashLoop restarts consume worker IDs,
although allocating only after earlier startup prerequisites succeed avoids consumption by those
failures. Exhaustion recovery is deliberately a domain change rather than a counter reset:

1. Scale sandbox-manager to zero and confirm all old processes have exited.
2. Choose a prefix never used in this cluster's history.
3. Update configuration and scale sandbox-manager back up. The new prefix maps to a new Lease and
   starts at worker 0.
4. Retain the old Lease and every active Sandbox and Checkpoint ID. Do not reset or delete them;
   normal recycle will retire active Sandbox IDs when those deliveries end.

If the Lease derived from the candidate prefix already exists, choose another prefix. Stopping old
processes and resetting the old counter is unsupported because historical Sandbox/Checkpoint IDs
and clock rollback can still reproduce a complete ID.

An etcd snapshot restore rolls Kubernetes API objects back together, so it may also roll back the
allocator Lease. Continuing with the same prefix after restore is an accepted duplicate-ID risk.
The strict recovery procedure is the same: keep managers stopped and switch to a never-used prefix
before serving traffic. See the [Kubernetes etcd operations documentation](https://kubernetes.io/docs/tasks/administer-cluster/configure-upgrade-etcd/).

## Operational Decisions and Trade-offs

- No dedicated Prometheus series are added for legacy resolution, assignment, Store processing, or
  peer compatibility. Existing claim/clone errors, operation timings, informer health, and
  structured route diagnostics remain the observability surface.
- Persisted identity is preferred over a global response-format switch because a global switch
  could make one Sandbox alternate IDs during rollout or configuration changes.
- A startup Lease allocation protocol is accepted in exchange for a 13-character suffix and
  atomic persistence in the original claim/clone write.
- Late worker allocation reduces, but cannot eliminate, CrashLoop consumption. Prefix switching is
  the recovery boundary for the fixed 20-bit worker field.
- Ambiguous CAS writes are confirmed by a live re-read so process restarts do not blindly consume
  an additional incarnation.
- Prefix-scoped Leases keep recovery domains disjoint without deleting historical allocation state.
- Deployments must prevent `CLOCK_REALTIME` from stepping backward while sandbox-manager runs.
  Sonyflake's sequence-exhaustion wait cannot observe request cancellation, and a backward step can
  extend that wait until the wall clock catches up. If this invariant cannot be guaranteed, replace
  the generator with a context-aware scheduler.
- A single active ID is preferred over permanent aliases because aliases complicate authorization,
  cache uniqueness, route deletion, and eventual removal of the legacy format.
- Existing labels are trusted on read because read-time validation could split component behavior
  after identity has already been persisted.
- Informer convergence and deletion fencing are preferred over API-server repair loops to keep one
  route truth source and avoid replica-scaled repair traffic.
