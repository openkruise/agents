---
title: Sandbox Template Backend Support
authors:
  - "@ywExcellent"
reviewers: []
creation-date: 2026-09-16
last-updated: 2026-10-15
status: provisional
see-also:
  - "/docs/proposals/20260916-sandbox-template-interface-design.md"
  - "/docs/proposals/20260123-sandbox-template.md"
  - "/docs/proposals/20260106-sandboxset-autoscaler.md"
---

# Sandbox Template Backend Support

## Summary

This proposal defines how the SandboxTemplate resource API is realized by the
Kubernetes and Substrate backends. Both backends retain the same public
Template identity, SandboxSet reference model, and E2B Sandbox lifecycle API.
They differ only in their internal materialization, capacity resources, and
optional artifact forms.

The first Kubernetes milestone validates a Template and makes it usable by an
ordinary Kubernetes SandboxSet without producing an artifact; it is
implemented and validated end to end. Target-state Kubernetes Build support
(checkpoint, run_cmd, image cache, deferred business start) is specified here
and lands behind the same contract. The Substrate backend creates an internal
ActorTemplate and waits for its Golden Snapshot; prototype validation of the
full mapping exists. Capabilities not delivered by a backend are explicitly
temporarily unsupported or future work; they must not be silently accepted by
the API.

## Background

OpenKruise Sandbox executes Sandboxes on two backends. The Kubernetes backend
treats a PodTemplate as the workload definition and manages template,
capacity, and instances through SandboxTemplate, SandboxSet, and Sandbox. The
Substrate backend uses ActorTemplate and Golden Snapshot as its template and
build artifact, materializes capacity as a WorkerPool, and runs each instance
as an Actor.

The two chains differ in template definition, build artifact, and capacity
model, but the public contract — Template identity, the SandboxSet reference
model, and the E2B Sandbox lifecycle — must stay uniform. This proposal
specifies both realizations and the rules that keep them behind one contract.
The public surface each realization must preserve is defined by the companion
interface proposal; everything below it is internal.

## Glossary

- **Logical Template**: the public, namespaced SandboxTemplate identified by
  `metadata.name`.
- **Materialized Template**: a validated Template usable by a SandboxSet.
- **Artifact**: an optional reusable backend output recorded in Template
  Status.
- **Warm pool**: capacity maintained by a SandboxSet.
- **ActorTemplate**: the internal Substrate template for one logical Template.
- **Golden Snapshot**: the initial Substrate state that makes an ActorTemplate
  usable.
- **WorkerPool**: the internal Substrate capacity resource backing a
  SandboxSet.
- **Actor**: one Substrate-backed Sandbox instance.
- **Instance snapshot**: runtime Sandbox state used by E2B snapshot and clone;
  it is distinct from every Template artifact.

## Motivation

Users need one SDK workflow regardless of execution backend: define a
Template, create warm capacity through SandboxSet, and create Sandboxes with
the existing E2B lifecycle API. The platform must preserve that stable public
contract while allowing Kubernetes and Substrate to use different internal
resources.

The Kubernetes path must be useful before checkpoint Build infrastructure
exists. Substrate must remain opt-in and must not expose ActorTemplate,
WorkerPool, or control-plane credentials through the SDK.

### Goals

- Define common backend selection, validation, status, and lifecycle rules.
- Deliver a no-artifact Kubernetes Template to SandboxSet path first.
- Specify target-state Kubernetes Build support: checkpoint artifacts, envd
  run_cmd provisioning, image cache, and deferred business process start.
- Define the Substrate ActorTemplate, Golden Snapshot, and WorkerPool
  mappings, including prototype-validated rules.
- Make unsupported fields and lifecycle features explicit at admission time.
- Keep Template artifacts independent from E2B instance snapshots.

### Non-Goals

These are design boundaries rather than deferred work — there is no current
plan to support them:

- Multi-infrastructure routing and cross-backend pool aggregation.
- Changes to the community E2B Template Build protocol. Template Build is
  defined by the SandboxTemplate resource API; the community protocol
  surface stays untouched.
- Build history, rebuild, tags, rollback, and historical artifact selection.
  A Template has exactly one Build at a time.
- Substrate PoolAutoscaler support. Requests keep returning an explicit
  Unsupported error instead of creating an ineffective policy.

### Future Work

- Kubernetes checkpoint storage format and restore optimization internals —
  owned by the checkpoint provider design.
- Substrate Fork support.
- A production metadata store schema or HA recovery implementation for
  Substrate actors.

## Proposal

### Common Backend Contract

`SandboxTemplate.spec.backend` selects an immutable backend. It defaults to
`kubernetes`; users select `substrate` explicitly. The default is
materialized on every write and validated against the previous value, so
clearing the field cannot bypass immutability. The controller is the only
writer of SandboxTemplate Status. sandbox-manager creates and reads CRs,
validates user-facing requests, and orchestrates E2B instance lifecycle.

```text
Kruise SDK ──▶ sandbox-manager resource API ──▶ SandboxTemplate
                                                   │
                                                   ▼
                                      SandboxTemplate controller
                                          │                    │
                                          ▼                    ▼
                                Kubernetes resources   Substrate control API

Kruise SDK ──▶ SandboxSet(templateRef, replicas) ──▶ backend capacity
E2B SDK ──▶ sandbox-manager lifecycle API ──▶ backend Sandbox instance
```

A Template is Ready only when its selected backend has completed the work
required for that backend's current materialization model. Ready does not
imply that every future Build feature or every E2B operation is supported.

| Status field | Common meaning |
| --- | --- |
| `observedGeneration` | Generation evaluated by the controller. |
| `phase` | `Pending`, `Building`, `Ready`, or `Failed`. |
| `conditions` | Machine-readable validation and readiness results. |
| `artifactRef` | Optional backend artifact; absent when the backend requires none. |

The backend never drops unsupported workload fields. It rejects them with a
field-path validation error before marking a Template Ready — at admission
when the request can be refused before persistence, or as terminal Failed
status when only the backend can judge.

### Common Resource Interaction

```text
SDK              Resource API       Controller             Selected backend
 │ POST Template       │                  │                       │
 │────────────────────▶│ create CR        │                       │
 │◀────────────────────│ 201              │                       │
 │                     │─────────────────▶│ reconcile spec        │
 │                     │                  │──────────────────────▶ materialize
 │ GET Template ──────▶│                  │                       │
 │◀────────────────────│ phase/conditions │                       │

SDK POST SandboxSet(templateRef, replicas)
  → resource API reads Template Status
  → rejects a non-Ready Template
  → persists backend-specific internal configuration only when required
  → SandboxSet controller materializes backend capacity
```

SandboxSet publicly expresses a Template reference, replica count, and pool
policy. It does not expose backend worker configuration. A backend may add
managed fields to the persisted SandboxSet object, but SDK request models must
remain backend-neutral.

### Capability Matrix

| Capability | Kubernetes | Substrate |
| --- | --- | --- |
| Template and SandboxSet public API | Supported | Supported |
| Template Ready without reusable artifact | First milestone (delivered) | Not applicable; Golden Snapshot required |
| Ordinary warm pool materialization | Supported | Supported through WorkerPool |
| Container image, command, args, env, and standard PodTemplate fields | Supported by existing Kubernetes SandboxSet semantics | Supported only when representable; unsupported fields rejected |
| E2B create, pause, resume, delete | Supported by existing Kubernetes lifecycle | Supported after Actor lifecycle implementation |
| E2B snapshot and clone | Existing Kubernetes behavior | Supported through Actor snapshot tags |
| PoolAutoscaler | Supported for Kubernetes SandboxSets | Temporarily unsupported |
| Checkpoint Template artifact | Future (specified below) | Golden Snapshot supported |
| ImageCache | Future (specified below) | Temporarily unsupported |
| `run_cmd()` and envd provisioning | Future (specified below) | Temporarily unsupported |
| `after_claim_initialization` | Future (specified below) | Temporarily unsupported |
| Fork | Existing Kubernetes behavior is unchanged | Temporarily unsupported |
| Persistent actor metadata and restart recovery | Not a new backend requirement | Future production prerequisite |

“Supported” in this table means the feature has an implementation target in
this proposal. “Future” requires further implementation of an already
specified design or a missing platform capability. “Temporarily unsupported”
means the API must reject the request instead of claiming equivalent
behavior.

### Kubernetes Backend

#### First Milestone (delivered)

The controller validates the Template PodTemplate against existing Kubernetes
SandboxSet requirements. It then writes Ready with no `artifactRef`. A
SandboxSet referencing that Template materializes ordinary Kubernetes warm
Sandboxes using the existing controller path.

```text
SandboxTemplate controller
  → validate PodTemplate
  → status.phase=Ready, status.artifactRef omitted

SandboxSet controller
  → resolve templateRef
  → create and maintain ordinary Kubernetes Sandboxes
```

Validation is a dual gate: the resource API rejects malformed or unsupported
requests at admission (400), and the controller independently validates the
persisted PodTemplate, writing Failed with a sanitized field-path message
when it cannot be materialized.

For this milestone, Ready means “the Template can be used for a normal
SandboxSet-based Kubernetes pool.” It does not mean a checkpoint, prebuilt
filesystem, image cache entry, or Build command result exists.

The milestone is validated end to end on a Kubernetes cluster:

- Template validation through both gates, including rejection of
  unsupported Build options at admission.
- Backend immutability at the CRD level, including attempts to bypass it by
  removing the field (defaulting re-materializes `kubernetes` first).
- templateRef inheritance: unset pool fields are taken from the referenced
  Template, and a Template change rolls pool revisions exactly like an
  inline template change.
- The E2B claim path: `Sandbox.create(template=...)` resolves the
  same-named SandboxSet, claims a warm Sandbox, and completes claim-time
  initialization.

#### Target State: Build Artifacts

The following capabilities land behind the same Template contract. Each is
independent: the no-artifact milestone is not blocked by any of them, and
none of them changes the SDK workflow.

**Checkpoint Build.** When `templateBuildOptions.checkpoint` is set, the
controller launches a Build Pod from `spec.template` (with runtime and envd
injection), waits for the business container and envd to become ready,
executes any queued provisioning commands, and captures a checkpoint of the
requested persistent contents:

```text
Template accepted (checkpoint=true)
  → launch Build Pod from spec.template (+ runtime/envd injection)
  → wait for business container and envd readiness
  → execute queued run_commands sequentially via envd (optional)
  → capture checkpoint of spec.persistentContents (pod info implicit)
  → status.phase=Ready
  → status.artifactRef = { backend: kubernetes, checkpointRef: {...} }
```

A Template has one Build per generation: a spec change re-enters Building
and replaces the artifact; there is no build history. A Build failure writes
Failed with a sanitized message. Warm Sandboxes then restore from the
checkpoint instead of cold-starting from the image, which removes
business-process startup (dependency installation, cache warming, process
initialization) from the request path.

**run_cmd provisioning.** `run_cmd()` is the only E2B Builder-style provision.
Commands execute sequentially (`&&` semantics; a non-zero exit fails the
Build) through envd inside the business container, with the optional E2B
`user` parameter preserved. Command effects on the filesystem and process
state are frozen into the checkpoint, so `run_cmd` requires
`checkpoint=true` and filesystem persistence — validated by the SDK before
the request and by the server again. The command target stays singular: a
multi-container PodTemplate may carry sidecars, but Build commands run only
in the business container.

The deliberate difference from the E2B Builder surface:

| Category | E2B Builder | Kruise Template Build |
| --- | --- | --- |
| Base image | `from_image()`, `from_template()`, `from_dockerfile()`, ... | Container images declared in the unified Template spec |
| Files | `copy()`, `git_clone()`, `make_dir()`, `rename()`, ... | Pre-baked into the image; some operations expressible as `run_cmd`; upload-based `copy()` unsupported |
| Commands | `run_cmd(command, user)` | Same shape, executed via envd |
| Packages | `apt_install()`, `pip_install()`, `npm_install()`, ... | Explicit `run_cmd("pip install ...")` |
| Build context | `set_workdir()`, `set_user()`, `set_envs()` persist across steps | No persistent builder context |
| Start/readiness | `set_start_cmd()`, `set_ready_cmd()`, `wait_for_port()`/`url()`/`process()`/`file()`/`timeout()` | Container command and Kubernetes-native probes |
| Build control | `skip_cache()`, per-step build cache | ImageCache and checkpoint are separate layers; no step cache |
| Advanced | Dev Container prebuilds, Dockerfile conversion, template stacking | Out of scope |

**ImageCache.** When `templateBuildOptions.imageCache` is set, the platform
resolves every container image in `spec.template` and creates or reuses one
image cache per image (a multi-container Template can map to several
caches), using its own region, network, and cloud credentials. The user
provides no registry credentials for this: private pulls reuse the
Template's `imagePullSecrets`, and secret contents never enter the
SandboxTemplate, Build status, or logs. When the user enables the cache, the
Build becomes Ready only after the cache is ready, and cache failure fails
the Build.

**Deferred business process start.** For Serving workloads that need
claim-time dynamics, `businessProcess.startPolicy=after_claim_initialization`
keeps the business command out of the restored state:

1. The user's business command is preserved and the business container's
   command is replaced by a supervisor-like deferred launcher:

   ```text
   configured:  /opt/code-interpreter/start.sh
   launched:    /var/sandbox/run_with_envs.sh -- /opt/code-interpreter/start.sh
   ```

2. Build and warm-up run only platform daemons (for example the agent
   runtime) and the waiting launcher. A checkpoint captures that pre-start
   state, not a running business process.
3. At claim time, in-place image/resource updates complete and the runtime
   becomes ready; then environment injection, identity and credential
   initialization, and dynamic CSI mounts are applied. The launcher is still
   waiting.
4. Once initialization completes, the start signal is written. The launcher
   loads the claim-level environment and execs the original command, which
   then runs under the updated image, resources, environment, and mounts.

Deferred start is optional. A workload that needs no claim-level dynamics
starts its business process during Build and checkpoints the fuller running
state — a richer restored state and an even faster path — and only
claim-configured workloads opt into `after_claim_initialization`.

**RL without checkpoints.** RL fleets can run tens of thousands of distinct
task images with low per-image reuse. A memory checkpoint binds to the
platform component versions captured at build time, so per-image checkpoints
would demand a rebuild storm after every platform upgrade. The RL pattern
therefore uses `checkpoint=false` and `imageCache=false` on a base Template
whose pool provides warm capacity; the real task image is selected per
request through `e2b.agents.kruise.io/*` claim metadata and applied by
in-place update. Image replacement restarts the business process, so no
deferred start policy is used. Batch invalidation of stale snapshots after
platform upgrades follows a scan-and-rebuild model rather than in-place
upgrade.

### Substrate Backend

#### Template Materialization

A logical Substrate Template maps one-to-one to a same-named ActorTemplate.
The backend Adapter converts the supported subset of the unified
SandboxTemplateSpec into the internal ActorTemplate representation; fields it
cannot express are rejected with a field-path error, never silently dropped.
The controller creates the ActorTemplate idempotently by name and polls its
Golden Snapshot phase.

```text
SandboxTemplate controller ──▶ Ensure namespace/CreateActorTemplate
                              ──▶ GetActorTemplate phase

Golden Snapshot Ready
  → status.phase=Ready
  → status.artifactRef.backend=substrate
  → status.artifactRef.actorTemplateName=<Template name>
```

A failed Golden Snapshot writes Failed with a sanitized message.
Reconciliation repeats the same-name ensure operation after controller
restart. Runtime resolution uses `status.artifactRef.actorTemplateName`; it
must not select a recent ActorTemplate by list ordering.

| Template input | Substrate rule |
| --- | --- |
| image, command, args | Supported; image must include a digest. |
| multiple business containers | Supported when the selected SandboxClass supports them. |
| literal env and HTTP readiness | Supported when representable by Substrate. |
| cpu/memory limits | Accepted and validated (requests omitted or equal limits); capacity semantics — not bound to the ActorTemplate, consumed by the WorkerPool at pool stage. |
| volumes, initContainers, `valueFrom` | Temporarily unsupported; reject. |
| request-time Env, CSI mounts, in-place updates | Temporarily unsupported; reject. |
| `run_cmd`, ImageCache, deferred business start | Temporarily unsupported; reject. |

#### Unified Spec to ActorTemplate Conversion

The Adapter converts the unified PodTemplate into the neutral container form
the Substrate control plane accepts. Every field is either converted
faithfully or rejected with its field path; nothing is dropped silently.

| Unified spec field | ActorTemplate | Rule |
| --- | --- | --- |
| `containers[].name`, `image` | container identity | Direct; image must be digest-pinned. |
| `containers[].command`, `args` | entrypoint | Direct; sandbox classes that isolate each business container into its own rootfs tree require an explicit command or args on every container of a multi-container Template (there is no unambiguous default process); single-container Templates keep the image entrypoint. |
| `containers[].env` | env | Literal values only; `valueFrom` and duplicate names rejected. |
| `containers[].securityContext.capabilities` | capabilities (add/drop) | The only securityContext subfield that converts; `runAsUser`, `privileged`, `seccompProfile`, and the rest are rejected, and `ALL` must be expanded into individual capabilities. |
| `containers[].readinessProbe` | readyz | Only `httpGet` probes map; path and port are required. |
| `volumes`, `volumeMounts`, `initContainers` | — | Rejected. |
| `resources` | — | Validated only; see resource placement below. |

**Resource placement.** Resources are capacity semantics, not workload
semantics. They are validated at admission — only cpu and memory limits are
expressible, and requests must be empty or exactly mirror limits, because
Substrate enforces limits and cannot honor a different reservation — but in
the target model they are not bound into the ActorTemplate or the Golden
Snapshot: the same values size the WorkerPool at pool stage, so users
declare them once. The prototype attached cpu/memory limits to the
ActorTemplate; the target model moves that binding to the pool stage, which
keeps the Golden Snapshot free of capacity state and lets capacity change
without rebuilding the Template.

**Identity and idempotency.** Three rules protect the 1:1 mapping:

1. *Terminal validation.* Spec validation runs before anything is created: a
   rejected Build never creates an ActorTemplate, never adds a finalizer,
   and stays Failed until the spec changes.
2. *Collision-checked idempotency.* An existing same-named ActorTemplate is
   the idempotent path (a previous reconcile or a controller restart). The
   controller deep-compares every converted field — name, image, command,
   args, env, capabilities — against the existing object; a mismatch is a
   name collision with this Build's identity and fails loudly. The 1:1 model
   has no rebuild, so a foreign artifact is never claimed silently.
3. *Transient golden actor.* Substrate starts a golden actor from the
   ActorTemplate, waits for readyz, captures the Golden Snapshot, and
   releases the golden actor; only the snapshot is retained.

**Platform envelope.** Two families of settings deliberately never enter the
Template interface. The *execution envelope* — sandbox class and sandbox
config — is deployment-level platform configuration that decides the
ActorTemplate envelope together with the WorkerPool, not a Template or SDK
field. *Snapshot storage* — location, chunking, retention — is
platform-managed: the user declares only `checkpoint` and
`persistent_contents`, storage resolves per team namespace, and the Golden
Snapshot identity and build result land in `status.artifactRef` and
`status.conditions`.

#### SandboxSet Materialization

The resource API resolves the referenced Template. For a Ready Substrate
Template, it reads a deployment-level WorkerPool profile and fills managed
SandboxSet fields plus the backend marker. The profile contains the ateom
image, resources, scheduling rules, SandboxClass, and SandboxConfigName. It
is neither a Template field nor an SDK field.

```text
SandboxSet(templateRef, replicas)
  → verify Ready Substrate Template
  → fill managed WorkerPool profile
  → persist SandboxSet
  → controller projects it to same-named WorkerPool
```

The WorkerPool is Ready only when it has usable capacity. Users adjust
capacity through SandboxSet replicas. Substrate PoolAutoscaler is temporarily
unsupported and must return an explicit Unsupported error.

A prototype validated the SandboxSet-to-WorkerPool projection with the
following rules; the target state removes the inline worker template
entirely and resolves capacity fields through templateRef plus the
deployment-level profile, so users never declare resources twice:

1. SandboxSet name and namespace become the WorkerPool name and namespace.
2. `spec.replicas` maps one-to-one to `WorkerPool.spec.replicas`.
3. The worker container image maps to `WorkerPool.spec.ateomImage` and its
   resources map to the worker Pod resources.
4. `nodeSelector`, `tolerations`, `priorityClassName`, and `nodeAffinity`
   map to the WorkerPool PodTemplate; Pod (anti-)affinity is unsupported.
5. The SandboxClass selection (`gvisor` or `microvm`) is a backend/platform
   setting, not a Template field; the controller validates that the pool is
   compatible with the ActorTemplate.
6. The controller labels the WorkerPool with its owning SandboxSet, and E2B
   create metadata (`e2b.agents.kruise.io/sandboxset`) is translated into
   the Actor's worker selector, placing Actors on the chosen pool; omitting
   it lets the scheduler place on any compatible pool.

#### Actor Lifecycle and Recovery

```text
E2B create → resolve Ready artifact → verify WorkerPool
           → CreateActor → ResumeActor(boot=false) → persist metadata → publish route
E2B pause  → PauseActor or SuspendActor → withdraw route
E2B resume → ResumeActor(boot=false) → publish route
E2B delete → DeleteActor → remove metadata and route
```

With suspend-mode hibernation, Pause suspends the Actor — capturing a
snapshot and releasing the worker — and the subsequent resume restores the
Actor from that snapshot. With pause-mode hibernation, Pause pauses the Actor
in place and retains the worker. An E2B checkpoint suspends an Actor, creates
an instance snapshot tag, and resumes the source Actor. Clone creates a new
Actor from that tag. Neither operation modifies an ActorTemplate Golden
Snapshot: instance snapshots and Template artifacts stay separate, so
per-instance snapshots never invalidate the shared Template.

ActorTemplate is immutable and container env is captured in the Golden
Snapshot, so request-level env cannot be written back into the internal
ActorTemplate. Preserving the E2B dynamic env semantics requires a
post-restore injection mechanism, which is future work; until it exists,
request-level env for Substrate Sandboxes is rejected.

An early prototype measured Create at about 2.8 s, Pause at about 13.7 s,
Resume at about 0.6 s, and immediate Kill. These are prototype observations
on specific hardware that demonstrate the mapping is viable, not tuned
targets or SLAs.

Persistent metadata is required before production availability. It must record
the public sandbox ID, Actor identity, owner, timeout, WorkerPool, route,
hibernate state, and snapshot tags. Recovery must reconcile those records with
Actor state and repair routes. In-memory metadata is limited to initial
integration and is not production-safe.

### Deletion and Ownership

Template deletion is rejected while a SandboxSet references the Template or a
running instance still needs its artifact. Backend cleanup is guarded by a
Template finalizer: Kubernetes future artifact cleanup is backend-specific;
Substrate cleanup deletes the same-named ActorTemplate and Golden Snapshot
before releasing the logical Template.

Instance deletion never deletes a Template artifact. A failed backend create or
resume must compensate the created resource or retain enough durable metadata
for later recovery.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Backend differences leak into SDK models | Keep backend fields internal and reject unsupported input. |
| Kubernetes Ready is mistaken for checkpoint readiness | Omit artifactRef and document the no-artifact meaning. |
| Checkpoints bind platform component versions | RL pattern avoids checkpoints; platform upgrades use scan-and-rebuild invalidation. |
| Substrate Golden state differs from requested image | Require immutable image digests. |
| Restart creates duplicate Substrate artifacts | Use same-name idempotency and verify existing fields. |
| Actor is created but unmanaged | Persist metadata before success or compensate. |
| Lifecycle is mistaken for data-plane readiness | Verify routing, credential, and port contracts separately. |
| Deferred-start launcher fails to exec the business command | Signal/exec contract is covered by Build and claim integration tests. |

## Alternatives

Using E2B Template Build endpoints would couple the new SDK API to a community
compatibility protocol and expose build IDs outside the logical Template
model. Exposing WorkerPool configuration through SandboxSet would make a
Substrate implementation detail part of the SDK. Starting the business
process during every Build and checkpointing it unconditionally would break
claim-time dynamics for workloads that need them. All three are rejected.

## Upgrade Strategy

`spec.backend` defaults to Kubernetes, so existing Kubernetes resources retain
their behavior. Substrate is opt-in. The CRD additions are optional, and the
controller ignores Templates that do not select a backend it implements.
Build options arrive behind the existing contract: the first milestone
rejects them at admission, later milestones accept them, and the Template
identity, SDK workflow, and status fields do not change in between.

## Test Plan

- Unit tests for backend selection, validation, Status projection, and rejected
  unsupported fields.
- Kubernetes controller tests for no-artifact Ready and SandboxSet
  materialization.
- Kubernetes Build tests (per capability as it lands): checkpoint capture and
  artifactRef, run_cmd sequencing and failure, image-cache gating, deferred
  start signal/exec contract.
- Substrate controller tests for Ready, Failed, same-name idempotency, and
  finalizer cleanup.
- SandboxSet-to-WorkerPool projection tests, including invalid platform profile.
- Lifecycle tests for create, pause, resume, delete, snapshot, and clone.
- Restart and metadata recovery tests before enabling Substrate for production.
- Delivered end-to-end stages for the first milestone: admission and
  reconcile validation, CRD-level backend immutability (including
  field-removal bypass attempts), templateRef inheritance with revision
  rolling, and the E2B claim path against a live cluster.

## Implementation History

- 2026-09-16: Initial backend design draft.
- 2026-10-12: Expanded to cover Kubernetes and Substrate backend support with
  explicit temporary and future capability states.
- 2026-10-14: First Kubernetes milestone implemented and validated end to
  end. Expanded for design review with target-state Build mechanics
  (checkpoint, run_cmd, image cache, deferred start, RL pattern), the
  E2B Builder comparison, the unified-spec to ActorTemplate conversion rules
  and resource-placement decision, the prototype WorkerPool mapping rules
  and measurements, and the ActorTemplate env-immutability constraint.
- 2026-10-15: Section-by-section review pass. Non-Goals split into
  permanent design boundaries versus Future Work; Substrate Fork confirmed
  as future work.
