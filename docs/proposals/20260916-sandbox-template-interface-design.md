---
title: Sandbox Template Resource API
authors:
  - "@ywExcellent"
reviewers: []
creation-date: 2026-09-16
last-updated: 2026-10-12
status: provisional
see-also:
  - "/docs/proposals/20260123-sandbox-template.md"
  - "/docs/proposals/20260106-sandboxset-autoscaler.md"
  - "/docs/proposals/20260916-sandbox-template-backend-support.md"
---

# Sandbox Template Resource API

## Summary

This proposal adds a sandbox-manager resource API for logical SandboxTemplate
resources. A Template defines workload intent, SandboxSet defines warm capacity,
and PoolAutoscaler changes capacity independently. The SDK uses this API rather
than Kubernetes credentials.

Kubernetes is the default backend. The first Kubernetes milestone validates a
Template and lets SandboxSet materialize ordinary warm Sandboxes; it does not
produce a checkpoint artifact. Substrate is an opt-in backend described by the
companion proposal. Later checkpoint, image-cache, and command Build support
extend this contract without changing the Template identity or SDK workflow.

## Glossary

- **Logical Template**: namespaced SandboxTemplate named by users and referenced
  by SandboxSet.spec.templateRef.
- **Materialized Template**: a validated Template that can be used by a pool.
- **Artifact**: optional reusable backend output, such as a checkpoint or an
  ActorTemplate.
- **Warm pool**: capacity maintained by a SandboxSet.

## Motivation

SandboxTemplate and SandboxSet already separate workload definition from warm
capacity, but users lack a stable SDK API, backend-neutral status, and an
API-key-authorized way to create and observe these resources. The first delivery
must make the existing Kubernetes pool path available without waiting for a
checkpoint provider or a new Build runtime.

### Goals

- Define SDK and HTTP resource operations for Templates, SandboxSets, and
  PoolAutoscalers.
- Make SandboxTemplate.metadata.name the stable public identity.
- Persist backend choice and workload input in the Template CR.
- Define status for validation, readiness, and optional artifacts.
- Support a no-artifact Kubernetes path before checkpoint Build exists.
- Preserve existing E2B Sandbox and snapshot lifecycle APIs.

### Non-Goals/Future Work

- Kubernetes Build Pods, envd provisioning, run_cmd, ImageCache, and checkpoint
  artifact storage.
- Build history, tags, rebuild, rollback, or historical Build selection.
- Changes to the community E2B Template Build protocol.
- Substrate PoolAutoscaler support.

## Proposal

### Resource Relationships

```text
Kruise SDK
  │
  ▼
sandbox-manager resource API
  │  authenticated namespace mapping
  ▼
SandboxTemplate ──templateRef──▶ SandboxSet ──▶ warm Sandboxes
                                      │
                                      └──scaleTargetRef──▶ PoolAutoscaler

E2B SDK ──▶ sandbox-manager lifecycle API ──▶ claimed Sandbox
```

Template readiness, pool readiness, and autoscaler activity are separate state
machines. A Ready Template does not imply ready warm capacity.

### User Stories

#### Kubernetes Template and warm pool

```python
from kruise_agents import SandboxSet, Template
from kruise_agents.models import Container, SandboxSetSpec, SandboxTemplateSpec
from e2b_code_interpreter import Sandbox

template = Template.build(
    name="code-interpreter",
    spec=SandboxTemplateSpec(containers=[Container(
        name="runtime",
        image="example/code-interpreter@sha256:<digest>",
        command=["/usr/local/bin/start"],
    )]),
)
template.wait_until_ready(timeout=300)

pool = SandboxSet.create(
    name="code-interpreter-pool",
    template_ref=template.template_id,
    spec=SandboxSetSpec(replicas=5),
)
pool.wait_until_ready(timeout=300)

sandbox = Sandbox.create(template=template.template_id, timeout=300)
```

For the first Kubernetes milestone, Ready means the PodTemplate has passed
validation and can be materialized by SandboxSet. Every Sandbox starts from the
specified image; no checkpoint or ImageCache exists yet.

#### Optional autoscaling

```python
from kruise_agents import PoolAutoscaler
from kruise_agents.models import CapacityPolicy, PoolAutoscalerSpec

PoolAutoscaler.create(
    name="code-interpreter-autoscaler",
    scale_target_ref="code-interpreter-pool",
    spec=PoolAutoscalerSpec(
        min_replicas=1,
        max_replicas=20,
        capacity_policy=CapacityPolicy(target_available=3),
    ),
)
```

PoolAutoscaler supports only Kubernetes-backed pools. Unsupported backends must
return an explicit error rather than creating an ineffective policy.

### API Contract

The resource API is served by sandbox-manager at
`/agents.kruise.io/v1alpha1`; it is not a Kubernetes API proxy. API-key
authentication derives the namespace from the caller's team.

| Method | Path | Semantics |
| --- | --- | --- |
| POST | `/templates` | Persist a Template and return 201; convergence is asynchronous. |
| GET | `/templates/{name}` | Return spec, phase, conditions, and optional artifact. |
| DELETE | `/templates/{name}` | Refuse referenced Templates; otherwise delete Template and artifact. |
| POST/GET/PATCH | `/sandboxsets`, `/sandboxsets/{name}` | Manage warm capacity by templateRef. |
| POST/GET/PATCH/DELETE | `/poolautoscalers`, `/poolautoscalers/{name}` | Manage Kubernetes autoscaling. |

Duplicate creation returns 409. Deleting a missing Template is idempotent.
Users cannot specify another namespace in request bodies.

### Data Model

```go
type SandboxTemplateSpec struct {
    Backend string `json:"backend,omitempty"`
    TemplateBuildOptions *SandboxTemplateBuildOptions `json:"templateBuildOptions,omitempty"`
    Template *corev1.PodTemplateSpec `json:"template,omitempty"`
    PersistentContents []string `json:"persistentContents,omitempty"`
    Runtimes []RuntimeConfig `json:"runtimes,omitempty"`
    PauseStrategy *PauseStrategy `json:"pauseStrategy,omitempty"`
    Probes []Probe `json:"probes,omitempty"`
    AutoPausePolicy *AutoPausePolicy `json:"autoPausePolicy,omitempty"`
}

type SandboxTemplateStatus struct {
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
    Phase SandboxTemplateBuildPhase `json:"phase,omitempty"`
    ArtifactRef *SandboxTemplateArtifactRef `json:"artifactRef,omitempty"`
    Message string `json:"message,omitempty"`
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

`backend` defaults to `kubernetes` and is immutable. The SandboxTemplate
controller is the exclusive Status writer; sandbox-manager writes only spec.
ArtifactRef is empty in the first Kubernetes materialization mode. Future
Kubernetes Build writes a checkpoint reference; Substrate writes an
ActorTemplate reference.

### Kubernetes First-Milestone Interaction

```text
SDK                 Resource API       Template controller        SandboxSet controller
 │ POST Template          │                    │                        │
 │───────────────────────▶│ create CR          │                        │
 │◀───────────────────────│ 201                │                        │
 │                         │───────────────────▶ validate PodTemplate   │
 │                         │                    │ status=Ready          │
 │ POST SandboxSet ──────▶│ create CR with ref │                        │
 │                         │───────────────────────────────────────────▶ materialize warm pool
 │◀───────────────────────│ 201                │                        │
```

The SandboxSet controller remains responsible for revision construction,
Sandbox creation, rolling updates, and pool status. TemplateRef must inherit
its PodTemplate and relevant runtime settings; it must not become a name-only
label.

### Backend Capability Rules

| Capability | Kubernetes first milestone | Substrate |
| --- | --- | --- |
| PodTemplate validation and warm pool | Supported | Supported through WorkerPool mapping |
| Checkpoint artifact | Future work | Golden Snapshot |
| run_cmd and ImageCache | Unsupported | Unsupported |
| PoolAutoscaler | Supported | Unsupported |

Unsupported input is rejected before persistence or reported as terminal Failed
status with a field-path error.

### Implementation Details and Constraints

- The resource API follows API → Manager → Infra layering.
- The controller must not import sandbox-manager packages.
- templateRef is the only public Template-to-pool linkage; the SDK does not
  expose backend-specific worker configuration.
- The first Kubernetes milestone rejects checkpoint, image-cache, and command
  Build options instead of accepting behavior it cannot provide.
- E2B Sandbox.create continues to receive a logical Template name; it is not
  remapped to community E2B Template Build routes.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Ready is mistaken for a checkpointed image | Document no-artifact Ready semantics and leave ArtifactRef empty. |
| TemplateRef drops workload settings | Test revision construction and fields copied to created Sandboxes. |
| Backend-specific fields are silently ignored | Validate by selected backend with field-level errors. |
| Template deletion breaks an active pool | Refuse deletion while SandboxSet references exist. |

## Alternatives

Requiring checkpoint Build before a Template is usable delays basic warm-pool
use until an unimplemented provider exists. Letting the SDK write CRs directly
would leak cluster credentials. Repeating PodTemplate in SandboxSet duplicates
workload intent. This proposal avoids all three.

## Upgrade Strategy

New fields are optional. Templates without backend mean Kubernetes. Existing
inline SandboxSets remain supported; new SDK clients use templateRef. No E2B
lifecycle route is removed.

## Test Plan

- Resource API authentication, namespace isolation, conflict, and delete tests.
- Template controller validation and Ready/Failed status tests.
- SandboxSet TemplateRef inheritance and revision tests.
- Kubernetes autoscaler tests and unsupported-backend tests.
- SDK Template → SandboxSet → E2B claim integration test.

## Implementation History

- 2026-09-16: Initial draft.
- 2026-10-12: Reworked for upstream: no pre-existing Substrate POC is assumed,
  and the initial Kubernetes path does not require a Build artifact.
