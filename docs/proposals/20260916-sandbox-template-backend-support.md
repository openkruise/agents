---
title: Sandbox Template Backend Support
authors:
  - "@ywExcellent"
reviewers: []
creation-date: 2026-09-16
last-updated: 2026-10-12
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
ordinary Kubernetes SandboxSet without producing an artifact. The Substrate
backend creates an internal ActorTemplate and waits for its Golden Snapshot.
Capabilities not delivered by a backend are explicitly temporarily unsupported
or future work; they must not be silently accepted by the API.

## Glossary

- **Logical Template**: the public, namespaced SandboxTemplate identified by
  `metadata.name`.
- **Materialized Template**: a validated Template usable by a SandboxSet.
- **Artifact**: an optional reusable backend output recorded in Template Status.
- **Warm pool**: capacity maintained by a SandboxSet.
- **ActorTemplate**: the internal Substrate template for one logical Template.
- **Golden Snapshot**: the initial Substrate state that makes an ActorTemplate
  usable.
- **WorkerPool**: the internal Substrate capacity resource backing a SandboxSet.
- **Actor**: one Substrate-backed Sandbox instance.
- **Instance snapshot**: runtime Sandbox state used by E2B snapshot and clone;
  it is distinct from every Template artifact.

## Motivation

Users need one SDK workflow regardless of execution backend: define a Template,
create warm capacity through SandboxSet, and create Sandboxes with the existing
E2B lifecycle API. The platform must preserve that stable public contract while
allowing Kubernetes and Substrate to use different internal resources.

The Kubernetes path must be useful before checkpoint Build infrastructure
exists. Substrate must remain opt-in and must not expose ActorTemplate,
WorkerPool, or control-plane credentials through the SDK.

### Goals

- Define common backend selection, validation, status, and lifecycle rules.
- Deliver a no-artifact Kubernetes Template to SandboxSet path first.
- Define the Substrate ActorTemplate, Golden Snapshot, and WorkerPool mappings.
- Make unsupported fields and lifecycle features explicit at admission time.
- Keep Template artifacts independent from E2B instance snapshots.

### Non-Goals/Future Work

- Multi-infrastructure routing and cross-backend pool aggregation.
- Changes to the community E2B Template Build protocol.
- Build history, rebuild, tags, rollback, and historical artifact selection.
- Kubernetes checkpoint implementation, storage format, or restore optimization.
- Substrate PoolAutoscaler and Substrate Fork support.
- A production metadata store schema or HA recovery implementation for
  Substrate actors.

## Proposal

### Common Backend Contract

`SandboxTemplate.spec.backend` selects an immutable backend. It defaults to
`kubernetes`; users select `substrate` explicitly. The controller is the only
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
required for that backend's current materialization model. Ready does not imply
that every future Build feature or every E2B operation is supported.

| Status field | Common meaning |
| --- | --- |
| `observedGeneration` | Generation evaluated by the controller. |
| `phase` | `Pending`, `Building`, `Ready`, or `Failed`. |
| `conditions` | Machine-readable validation and readiness results. |
| `artifactRef` | Optional backend artifact; absent when the backend requires none. |

The backend never drops unsupported workload fields. It rejects them with a
field-path validation error before marking a Template Ready.

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
| Template Ready without reusable artifact | Supported in first milestone | Not applicable; Golden Snapshot required |
| Ordinary warm pool materialization | Supported | Supported through WorkerPool |
| Container image, command, args, env, and standard PodTemplate fields | Supported by existing Kubernetes SandboxSet semantics | Supported only when representable; unsupported fields rejected |
| E2B create, pause, resume, delete | Supported by existing Kubernetes lifecycle | Supported after Actor lifecycle implementation |
| E2B snapshot and clone | Existing Kubernetes behavior | Supported through Actor snapshot tags |
| PoolAutoscaler | Supported for Kubernetes SandboxSets | Temporarily unsupported |
| Checkpoint Template artifact | Future | Golden Snapshot supported |
| ImageCache | Future | Temporarily unsupported |
| `run_cmd()` and envd provisioning | Future | Temporarily unsupported |
| `after_claim_initialization` | Future | Temporarily unsupported |
| Fork | Existing Kubernetes behavior is unchanged | Temporarily unsupported |
| Persistent actor metadata and restart recovery | Not a new backend requirement | Future production prerequisite |

“Supported” in this table means the feature has an implementation target in this
proposal. “Future” requires further design or a missing platform capability.
“Temporarily unsupported” means the API must reject the request instead of
claiming equivalent behavior.

### Kubernetes Backend

#### First Milestone

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

For this milestone, Ready means “the Template can be used for a normal
SandboxSet-based Kubernetes pool.” It does not mean a checkpoint, prebuilt
filesystem, image cache entry, or Build command result exists.

#### Future Kubernetes Build

Checkpoint artifacts, Build Pods, envd, ImageCache, `run_cmd()`, and deferred
business process startup require separate implementation and API validation.
When available, the controller will write a Kubernetes-specific artifact
reference only after the requested artifact is ready. Their absence must not
block the no-artifact first milestone.

### Substrate Backend

#### Template Materialization

A logical Substrate Template maps one-to-one to a same-named ActorTemplate. The
controller validates supported fields, creates the ActorTemplate idempotently,
and polls its Golden Snapshot phase.

```text
SandboxTemplate controller ──▶ Ensure namespace/CreateActorTemplate
                              ──▶ GetActorTemplate phase

Golden Snapshot Ready
  → status.phase=Ready
  → status.artifactRef.backend=substrate
  → status.artifactRef.actorTemplateName=<Template name>
```

A failed Golden Snapshot writes Failed with a sanitized message. Reconciliation
repeats the same-name ensure operation after controller restart. Runtime
resolution uses `status.artifactRef.actorTemplateName`; it must not select a
recent ActorTemplate by list ordering.

| Template input | Substrate rule |
| --- | --- |
| image, command, args | Supported; image must include a digest. |
| multiple business containers | Supported when the selected SandboxClass supports them. |
| literal env and HTTP readiness | Supported when representable by Substrate. |
| cpu/memory limits | Supported; requests must be omitted or equal limits. |
| volumes, initContainers, `valueFrom` | Temporarily unsupported; reject. |
| request-time Env, CSI mounts, in-place updates | Temporarily unsupported; reject. |
| `run_cmd`, ImageCache, deferred business start | Temporarily unsupported; reject. |

#### SandboxSet Materialization

The resource API resolves the referenced Template. For a Ready Substrate
Template, it reads a deployment-level WorkerPool profile and fills managed
SandboxSet fields plus the backend marker. The profile contains the ateom image,
resources, scheduling rules, SandboxClass, and SandboxConfigName. It is neither
a Template field nor an SDK field.

```text
SandboxSet(templateRef, replicas)
  → verify Ready Substrate Template
  → fill managed WorkerPool profile
  → persist SandboxSet
  → controller projects it to same-named WorkerPool
```

The WorkerPool is Ready only when it has usable capacity. Users adjust capacity
through SandboxSet replicas. Substrate PoolAutoscaler is temporarily unsupported
and must return an explicit Unsupported error.

#### Actor Lifecycle and Recovery

```text
E2B create → resolve Ready artifact → verify WorkerPool
           → CreateActor → ResumeActor(boot=false) → persist metadata → publish route
E2B pause  → PauseActor or SuspendActor → withdraw route
E2B resume → ResumeActor(boot=false) → publish route
E2B delete → DeleteActor → remove metadata and route
```

An E2B checkpoint suspends an Actor, creates an instance snapshot tag, and
resumes the source Actor. Clone creates a new Actor from that tag. Neither
operation modifies an ActorTemplate Golden Snapshot.

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
| Substrate Golden state differs from requested image | Require immutable image digests. |
| Restart creates duplicate Substrate artifacts | Use same-name idempotency and verify existing fields. |
| Actor is created but unmanaged | Persist metadata before success or compensate. |
| Lifecycle is mistaken for data-plane readiness | Verify routing, credential, and port contracts separately. |

## Alternatives

Using E2B Template Build endpoints would couple the new SDK API to a community
compatibility protocol and expose build IDs outside the logical Template model.
Exposing WorkerPool configuration through SandboxSet would make a Substrate
implementation detail part of the SDK. Both alternatives are rejected.

## Upgrade Strategy

`spec.backend` defaults to Kubernetes, so existing Kubernetes resources retain
their behavior. Substrate is opt-in. The CRD additions are optional, and the
controller ignores Templates that do not select a backend it implements.

## Test Plan

- Unit tests for backend selection, validation, Status projection, and rejected
  unsupported fields.
- Kubernetes controller tests for no-artifact Ready and SandboxSet
  materialization.
- Substrate controller tests for Ready, Failed, same-name idempotency, and
  finalizer cleanup.
- SandboxSet-to-WorkerPool projection tests, including invalid platform profile.
- Lifecycle tests for create, pause, resume, delete, snapshot, and clone.
- Restart and metadata recovery tests before enabling Substrate for production.

## Implementation History

- 2026-09-16: Initial backend design draft.
- 2026-10-12: Expanded to cover Kubernetes and Substrate backend support with
  explicit temporary and future capability states.
