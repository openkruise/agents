# SandboxServing: session routing over a replicated Sandbox pool

## Scope

`SandboxServing` manages a namespaced pool of Sandboxes behind one public
identity. Requests carry a Session ID; the Gateway authenticates the caller,
selects a member, persists the binding, and forwards traffic directly to that
Sandbox. The binding survives Gateway and Manager replacement while its
Sandbox resource exists.

This initial implementation supports template provisioning, fixed replicas,
binding-aware scale-down, Session inspection and Close, local traffic
authorization, and best-effort recovery from concurrent bindings. It does not
provide a global unique-allocation transaction or exactly-once execution.

Claim provisioning, template rolling updates, Session idle actions, automatic
resume, autoscaling integration, and a community JWT signing provider are
outside this implementation. Reserved API fields do not imply these features
are implemented.

## Resource and ownership

The [example](../../config/samples/agents_v1alpha1_sandboxserving.yaml) creates
two members with a capacity of four Sessions each. `spec.replicas` defaults to
one; zero is valid. `spec.ports` declares the ports authorized for Serving
traffic. `spec.sessionPolicy.affinity` defaults to `BestEffort`; an omitted
`maxSessionsPerSandbox` uses the platform record limit.

`spec.provision.sandboxTemplate` embeds the shared `SandboxTemplateSpec`.
Creation copies the Pod template, PVC templates, persistent contents, runtimes,
pause strategy, probes, AutoPause policy, and lifecycle options. Each member
receives a controller owner reference, Serving UID/name/revision labels, and an
empty binding annotation. Membership requires both the Serving UID label and
the matching owner reference within the Serving namespace.

The complete provision is immutable and stored in an initial
`ControllerRevision`. Replica, port, and supported Session policy changes do
not create new revisions. `sandboxSetRef` and `buildOptions` are rejected;
`pauseAfterIdle` and `expireAfterIdle` report `Supported=False` before member
changes. `maxSurge` is reserved and does not initiate rolling updates.

Inline templates share the existing Pod/PVC schema and validation boundary.
They do not invoke the SandboxTemplate defaulting webhook: PVC access modes
must be explicit, and ServiceAccount token mounting follows the supplied Pod
template. Probe, AutoPause, and PVC relationship validation is retained.
Lifecycle upgrade hooks are unsupported. PVCs belong to individual Sandboxes;
this design does not provide stable storage across replacement Sandbox UIDs.
Template AutoPause remains independent of Serving Session policy and may pause
a member even while it has bindings; this does not promise uninterrupted traffic.

## Controller reconciliation

Nonterminal, nondeleting members count toward replicas, including members that
are Pending, NotReady, Paused, or Resuming. Only Running, Ready members count
toward `readyReplicas`. Status also reports the observed generation, UID-based
scale selector, revisions, and per-revision replica counts.

Scale-up creates members in bounded batches. Stable names and cache-observation
expectations allow an unobserved creation to be retried under its original
name rather than allocating another name after an uncertain response.

Scale-down considers only effective members with an absent binding annotation
or a valid empty binding object. The selected member's UID and resource version
from that same observation become DELETE preconditions. A conflict requires a
new observation. Occupied or malformed bindings block reclamation and set
`ScaleDownBlocked=True`. Deleting the Serving uses normal owner-reference
garbage collection; no Session retention finalizer is added.

## Traffic admission and routing

Serving traffic uses `{port}-{servingID}.svc.{baseDomain}` and exactly one
`X-Kruise-Agent-Session-ID` header. Session IDs are case-sensitive and match
`[A-Za-z0-9_-]{1,64}`. The response echoes the validated Session ID, including
on upstream errors.

The request path is:

```text
Gateway Host/Session parsing
  -> resolve trusted Serving identity and policy from the local cache
  -> verify Serving JWT and authorize the port
  -> shared in-process allocator
  -> cached observations and conditional Kubernetes binding writes
  -> forward directly to the selected Sandbox
```

There is no per-request allocation RPC to Manager, and no authoritative
memory-only binding map. Manager assigns public Serving IDs using the existing
Sonyflake worker coordination. Existing identified Servings can continue to
allocate while Manager is unavailable; a newly created Serving still needs
Manager to assign its public ID.

The Gateway uses its existing Sandbox informer and an isolated Serving cache.
It preserves namespace and selector scope. When `--label-selector` is configured,
both the SandboxServing object's labels and its member template's labels must
match it; the generated Sandbox members must remain in that scope. A parent
outside the selector is not visible to the Gateway, even if its members match.
The example labels both the Serving and member template with `app: sample` for
use with `--label-selector=app=sample`. Missing Serving capability does not
intentionally disable ordinary Sandbox routing. Business reads use cached
clients; this feature adds no APIReader reads for Session allocation.

## Binding and allocation semantics

`agents.kruise.io/session-bindings` stores a JSON object on each member:

```json
{"session-a":{"state":"Active","lastRequestAt":"2026-11-13T00:00:00Z"}}
```

The implementation validates IDs, states, timestamps, duplicate JSON keys, and
storage budgets. Limits are 512 records per member, 128 KiB for the binding
annotation including its key, and 256 KiB for all annotation keys and values.
Malformed data is unavailable, not an empty binding set. Binding writes
preserve unrelated annotations and discard unknown record fields.

For an unbound Session, the allocator selects a Running, Ready, nondeleting
member with an available route and spare capacity. It minimizes durable binding
count and breaks ties by Sandbox UID. Lowering capacity never evicts existing
Sessions. `lastRequestAt` is recorded when a binding is created; request activity
refresh and idle enforcement are not implemented.

An available existing binding is reused. `Strict` retains an unavailable
binding and returns an unavailable error. `BestEffort` removes an unavailable
binding before creating a replacement, rereading after removal to reuse or
reconcile another writer's result. Paused and Resuming members retain their
bindings under both policies; requests do not automatically resume them.

The unbound interval during recovery is accepted. Capacity exhaustion,
unconfirmed writes, conflicts, cancellation, or timeouts can produce a 503.
An uncertain new write is verified at its original target before the request
can consider another target. A successful write is observed through the cache
before the allocator returns a route. Local write watermarks and shared resource
version expectations prevent an older captured observation from erasing that
confirmation requirement. The request's total admission budget is two seconds.

Once forwarding starts, business errors and slow responses do not trigger
automatic rebinding or replay. Close removes binding records without stopping
the Sandbox and is idempotent; it is not a fence against concurrent reopening.

Deleting a Sandbox CR removes its Session history, including under `Strict`.
A later request using the same Session ID may therefore allocate a new member.
There is no persistent tombstone or historical-binding lookup.

## Concurrency and convergence

Process-local Session locks serialize cooperating requests within one Gateway
or Manager. Before a write, the Store rechecks the visible parent and member
versions, then conditionally patches one member. This is **not** a transaction
over the member set: two Gateways can write different Sandbox CRs and both
writes can succeed. Even rapid sequential requests can encounter differing
cache observations.

When duplicate bindings are observed, the lowest Sandbox UID wins. Repair
conditionally confirms that winner without changing its record, rereads the
member set, and conditionally removes one loser at a time. It does not merge
the loser's `lastRequestAt`. Warning logs report the conflict, failures, and
subsequently observed convergence. Requests and periodic Manager maintenance
reuse the same repair implementation.

The design accepts temporary duplicate bindings and possible traffic to
different members before repair. It provides neither execution fencing nor an
unconditional convergence deadline. A final unique record is not evidence that
all earlier requests used the same backend.

## Authentication and management APIs

Gateway authenticates a JWT against the current cached Serving identity before
reading or mutating member bindings. In addition to the existing signature,
issuer, and time validation, the token must contain:

```json
{
  "sub": "serving:controlplane:<servingID>",
  "metadata": {"servingId": "<servingID>", "servingUid": "<Serving UID>"}
}
```

A token containing a `sandbox` claim is rejected. Namespace/name metadata does
not establish authority. Missing or invalid credentials produce 401; a validly
signed stale Serving UID produces 404; a denied port produces 403. Unavailable
verification or observation produces 503. Ordinary Sandbox authentication
settings do not bypass Serving authentication.

Gateway strips the Serving JWT before forwarding. Serving admission authorizes
only a selected member of that Serving through the Serving hostname. Direct
Sandbox hostnames retain existing Sandbox JWT/static-token checks. Runtime-port
forwarding injects the member's existing runtime static credential only after
matching its current registry identity; business ports receive no such secret.

Manager exposes these native APIs under
`/kruise/api/v1alpha1/sandboxservings/{servingID}`:

| Method | Suffix | Result |
| --- | --- | --- |
| GET | `/sessions/{sessionID}` | Durable binding or 404 |
| GET | `/sessions?limit=...&nextToken=...` | Sorted array and optional `x-next-token` header |
| POST | `/sessions/{sessionID}:close` | Idempotent binding removal acknowledgment |
| POST | `/traffic-access-token` | Token and expiration from the configured provider |

These endpoints use the existing API-key authorization. Non-admin callers are
scoped by Team name/namespace; different keys in that Team share visibility.
Cross-Team lookup returns 404. Pagination cursors are bound to the Serving UID;
they do not provide a point-in-time snapshot across pages.

The default community identity provider returns a random placeholder, not a
JWT. The signing-provider seam is available, but a provider trusted by Gateway
must be configured to complete authenticated Serving traffic. This proposal
does not introduce enterprise identity integrations or claim a complete default
community signing flow.

## Code ownership and validation

- `pkg/controller/sandboxserving`: replicas, membership, Revision, and status.
- `pkg/servingsession`: neutral contracts; `binding`: persisted format;
  `allocator`: affinity, selection, and repair; `kubernetes`: shared persistence.
- `pkg/sandbox-gateway/serving`: protocol admission and local allocation;
  `filter`: transport completion and forwarding.
- `pkg/sandbox-manager`: inspection, Close, identity assignment, and maintenance;
  its Infra adapter supplies the shared Store.
- `pkg/servers/e2b`: management transport, API-key checks, and response mapping.

The independent Controller does not import Manager, API, or the allocator.
Gateway does not initialize SandboxManager or copy its allocation algorithm.

Validation must distinguish local tests from deployment acceptance. Deterministic
tests cover conditional writes, stale observations, uncertain outcomes,
cancellation, conflict repair, policy changes, and authentication boundaries.
Runtime acceptance must additionally exercise actual informer/RBAC behavior,
replica changes, multi-Gateway traffic, member recovery, and component replacement.
No template rolling-update or production-readiness claim follows from those tests.
