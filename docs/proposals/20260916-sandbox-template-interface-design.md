---
title: Sandbox Template Resource API
authors:
  - "@ywExcellent"
reviewers: []
creation-date: 2026-09-16
last-updated: 2026-10-15
status: provisional
see-also:
  - "/docs/proposals/20260123-sandbox-template.md"
  - "/docs/proposals/20260106-sandboxset-autoscaler.md"
  - "/docs/proposals/20260916-sandbox-template-backend-support.md"
---

# Sandbox Template Resource API

## Summary

This proposal defines the public resource API for OpenKruise Sandbox Templates.
One SDK workflow covers the create-and-observe resource lifecycle: a
**Template** defines workload intent, a **SandboxSet** maintains warm capacity,
and a **PoolAutoscaler** adjusts capacity independently, while the community
**E2B SDK** keeps managing Sandbox instances unchanged. Update and list
operations for these resources are target-state (see API Contract). The API is
served by sandbox-manager, authorized by API key, and never exposes Kubernetes
credentials, backend resources, or backend configuration to callers.

Kubernetes is the default backend. Its first milestone — Template validation
plus ordinary SandboxSet warm pools, with no build artifact — is implemented
and validated end to end. On the Kubernetes backend, checkpoint, image-cache,
and command provisioning Build options are already part of the API contract
but are rejected at admission until the corresponding backend support lands
(see the companion backend proposal). The Template identity, SDK workflow,
and status contract do not change when they arrive.

## Background

### What exists today

OpenKruise Sandbox is wire-compatible with the community E2B Sandbox protocol:
users already create, connect, pause, resume, and delete Sandboxes with the
community E2B SDK. The declarative `SandboxTemplate` CR and `SandboxSet` warm
pools separate workload definition from capacity, and warm pools remove most
infrastructure preparation time from the request path.

Two things are still missing:

1. **Build semantics.** The existing SandboxTemplate is a declarative
   configuration template, not a build product. There is no way to
   pre-initialize a business environment — installed dependencies, warmed
   caches, started processes, memory state — and reuse it across Sandboxes.
   E2B's Template Build expresses exactly this "build once, restore many"
   model, and warm pools cannot substitute for it: a warm pool shortens
   infrastructure readiness, not business-process startup.
2. **A resource API.** Managing Templates, pools, and autoscaling today
   requires `kubectl` and Kubernetes credentials. Users of the E2B-compatible
   API hold API keys, not cluster access, and resource management needs the
   same authorization model.

The platform also runs two execution backends. Kubernetes materializes a
Template as an ordinary PodTemplate-based pool; Substrate materializes it as
an ActorTemplate with a Golden Snapshot and a WorkerPool. The internal
resources differ completely, but users must see one Template identity, one
status contract, and one SDK workflow on both.

### What a Build must mean here

E2B's reference model is: a Template DSL describes a single-container
environment, an asynchronous Build produces an immutable snapshot (guest
memory, root filesystem, device state), and Create restores a microVM from
that snapshot — shared read-only template data, lazy memory load, and
copy-on-write rootfs keep restore in the tens of milliseconds. Create
references a template tag or build ID, and pools are invisible to callers.

Adopting that model in OpenKruise runs into two constraints:

1. A Template must express a full `PodTemplateSpec` — multiple containers,
   volumes, scheduling, security, and networking. The E2B DSL cannot.
2. Create must preserve claim-time dynamics — request-level environment
   injection, dynamic mounts, and in-place image/resource updates. Values
   that differ per request cannot be baked into a restored snapshot.

The design therefore separates concerns: **Build** validates the Template and
optionally produces a checkpoint artifact; **Create** claims from a warm pool
and applies claim-time dynamics on top; when the business process must see
those dynamics, its start is deferred until claim initialization completes.

## Glossary

- **Logical Template**: namespaced SandboxTemplate named by users and referenced
  by SandboxSet.spec.templateRef.
- **Materialized Template**: a validated Template that can be used by a pool.
- **Artifact**: optional reusable backend output, such as a checkpoint or an
  ActorTemplate.
- **Warm pool**: capacity maintained by a SandboxSet.

## Motivation

### Goals

- One SDK workflow regardless of execution backend: define a Template, create
  warm capacity, optionally configure autoscaling — without `kubectl` or
  Kubernetes credentials.
- `SandboxTemplate.metadata.name` is the stable public identity, used both by
  the resource API and by `Sandbox.create(template=...)`.
- One unified Template model: full PodTemplate workload fields plus Sandbox
  fields in a single spec, with backend capability validation instead of
  per-backend SDK models. The SDK model stays one shape; whether a given
  field is accepted depends on the selected backend, and unsupported input
  is rejected with a field-path error rather than accepted and ignored.
- A backend-neutral status contract covering validation, readiness, and
  optional artifacts.
- A no-artifact Kubernetes path first, usable before checkpoint Build
  infrastructure exists; Build options are part of the contract now and are
  rejected until implemented, rather than silently ignored.
- Preserve claim-time dynamics — environment injection, dynamic mounts, and
  in-place image/resource updates — and make them effective before the
  business process starts when the user opts into deferred start.
- Serve both Serving and RL workloads: Serving uses checkpoint and image
  cache for fast startup; RL uses a base Template with claim-time image
  replacement and skips checkpoints.
- Keep Template, warm pool, and autoscaling as three independent resources
  with independent state machines.

### Non-Goals

These are design boundaries rather than deferred work — there is no current
plan to support them:

- Changes to the community E2B Template Build protocol. Template Build is
  defined by the SandboxTemplate resource API; the community protocol
  surface stays untouched.
- Substrate PoolAutoscaler support. Requests keep returning an explicit
  Unsupported error instead of creating an ineffective policy.
- One Build call that also creates pools or autoscalers. Template, pool,
  and autoscaler lifecycles are independent by design.
- Build history, tags, rebuild, rollback, or selecting a historical build.
  A Template has exactly one Build at a time.

### Future Work

- Implementing the Kubernetes checkpoint provider, Build Pods, envd
  provisioning, run_cmd, and image-cache storage — owned by the companion
  backend proposal.

## Design Overview

### Two SDKs, one workflow

Resource management and instance lifecycle use different SDKs by design. The
Kruise SDK owns declarative resources; the community E2B SDK owns the running
Sandbox and is not modified.

```text
Resource management (Kruise SDK)          Instance lifecycle (community E2B SDK)
──────────────────────────────────        ─────────────────────────────────────────
Template.build/get/delete → SandboxTemplate  Sandbox.create(template=<name>)
SandboxSet.create/get     → SandboxSet   ──▶  claim a warm Sandbox, claim-time init
PoolAutoscaler.create/get  → PoolAutoscaler   pause / connect / kill
                                           snapshot / clone (existing APIs)
        │                                          │
        ▼                                          ▼
sandbox-manager resource API               sandbox-manager lifecycle API
(/agents.kruise.io/v1alpha1)               (E2B-compatible, unchanged)
```

The Kruise SDK does not flatten the three resources into one `build()` call.
Capacity and scaling change on different cadences than the environment
definition, and bundling them would force rebuilds for pool-only changes.

### Resource relationships

```text
Kruise SDK
  │
  ▼
sandbox-manager resource API
  │  API-key → namespace mapping
  ▼
SandboxTemplate ──templateRef──▶ SandboxSet ──▶ warm Sandboxes
                                      │
                                      └──scaleTargetRef──▶ PoolAutoscaler

E2B SDK ──▶ sandbox-manager lifecycle API ──▶ claimed Sandbox
```

A Template is referenced by pool resources; the reverse never happens. The
SDK exposes no backend-specific worker configuration anywhere.

### Three independent state machines

`SandboxTemplate Ready` means the environment definition (and, later, its
artifact and image cache) is prepared. `SandboxSet Ready` means the warm pool
has usable capacity. `PoolAutoscaler Active` means the scaling policy took
effect. A Ready Template does not imply ready warm capacity, and none of the
three implies that a created Sandbox's business process has started. SDK
wait helpers (`wait_until_ready`, `wait_until_active`) poll each resource
independently.

## User Journeys

### Journey 1: build a Template

```python
from openkruise_agents import Template
from openkruise_agents.models import (
    Container,
    ResourceRequirements,
    RuntimeConfig,
    SandboxTemplateSpec,
    TemplateBuildOptions,
)

build = Template.build(
    name="code-interpreter",
    spec=SandboxTemplateSpec(
        containers=[Container(
            name="runtime",
            image="example/code-interpreter@sha256:<digest>",
            command=["/usr/local/bin/start"],
            resources=ResourceRequirements(
                requests={"cpu": "1", "memory": "2Gi"},
            ),
        )],
        runtimes=[RuntimeConfig(name="agent-runtime")],
    ),
    options=TemplateBuildOptions(checkpoint=True, image_cache=True),
)
build.wait_until_ready(timeout=300)
```

The call omits `backend`, so the Template builds on Kubernetes — the default;
"Selecting a backend" below shows the explicit Substrate form.

`SandboxTemplateSpec` is the single public Template model. It carries the
complete PodTemplate workload fields — containers, volumes, scheduling,
security, networking, and template metadata — directly, without nesting a
`pod_template` or `actor_template`, plus the Sandbox fields
(`persistent_contents`, `runtimes`, `pause_strategy`). The SDK derives these
fields from the upstream Kubernetes OpenAPI schema (generated from a pinned
swagger, never a hand-written subset), so container, volume, and security
fields stay identical to Kubernetes. sandbox-manager maps the model onto
`SandboxTemplate.spec.template` when it persists the CR.

`TemplateBuildOptions` describes Build behavior and is persisted as
`spec.templateBuildOptions`:

| Option | Semantics |
| --- | --- |
| `checkpoint` | Produce a reusable build artifact. Captured content derives from `spec.persistent_contents`; pod info is always implicitly included. Defaults to false; setting it is rejected at admission until backend support lands. |
| `image_cache` | Create or reuse a per-image cache for every container image in the Template, using sandbox-manager's own region, network, and credentials. When enabled, Build becomes Ready only after the cache is ready, and cache failure fails the Build. |
| `business_process.start_policy` | `after_claim_initialization` defers the business command until claim-time initialization completes (see the Serving scenario). |
| `run_commands` | Queued provisioning commands executed sequentially via envd in the business container during a Kubernetes Build; any non-zero exit fails the Build. Kubernetes backend only. |

The Kubernetes backend deliberately does not reimplement the E2B Template
Builder surface. There are no `from_image()`, `copy()`, `set_workdir()`,
`apt_install()`, `pip_install()`, or `set_start_cmd()` helpers: files and
packages belong in the image, and everything else is expressed with native
Kubernetes fields. The only Builder-style provision is `run_cmd()`, which
keeps the E2B name and parameter shape:

```python
template = (
    Template(
        spec=SandboxTemplateSpec(...),
        options=TemplateBuildOptions(checkpoint=True),
    )
    .run_cmd("python -m pip install -r /app/requirements.txt")
    .run_cmd(["python -m compileall /app"])
)
build = Template.build(template, "python-agent", on_build_logs=default_build_logger())
```

`run_cmd()` requires `checkpoint=True` and filesystem persistence — the
client validates this before any request, and the server validates it again
— because command effects are only reusable if they are captured in the
artifact. Build logs stream to the `on_build_logs` callback.

A Template has exactly one Build and is immutable in this API version: the
spec, once accepted, cannot be updated, and there is no public build
identifier. Changing the environment means deleting the Template and building
a new one under a new name; this matches the delivered API, which registers
create, read, and delete for Templates and no update route. A generation-based
rebuild (update the spec, the new generation re-enters Building and replaces
the artifact) is target-state: when the update route lands, it must also
specify concurrent-update behavior, cancellation of an in-progress Build,
artifact replacement ordering, failure rollback, and retention of the old
artifact while dependent pools still use it.

The rest of the Template lifecycle reuses the same name. `Template.get(name)`
returns the same handle shape as `build()` — status, conditions, and artifact
reflect the persisted CR — so a separate process can re-attach to a Build.
`Template.delete(name)` removes the Template and its artifact; it is refused
with 409 while a SandboxSet references the Template and succeeds idempotently
for a missing name.

#### Selecting a backend

`Template.build()` accepts a `backend` parameter. Kubernetes is the default
and is omitted from the wire request; Substrate is sent explicitly:

```python
from openkruise_agents import Template
from openkruise_agents.models import (
    Container,
    PersistentContent,
    ResourceRequirements,
    SandboxTemplateSpec,
    TemplateBackend,
    TemplateBuildOptions,
)

build = Template.build(
    name="stateful-agent",
    backend=TemplateBackend.SUBSTRATE,
    spec=SandboxTemplateSpec(
        containers=[Container(
            name="agent",
            image="registry.example.com/stateful-agent@sha256:<digest>",
            resources=ResourceRequirements(limits={"cpu": "2", "memory": "4Gi"}),
        )],
        persistent_contents=[PersistentContent.FILESYSTEM, PersistentContent.MEMORY],
    ),
    options=TemplateBuildOptions(checkpoint=True),
)
build.wait_until_ready()
```

The backend is written to `spec.backend` at creation time and is immutable
afterwards — clearing the field cannot bypass the rule, because the default
is materialized on every write. `backend` is a build-level selector, not a
build option: `TemplateBuildOptions` describes what a Build produces, while
`backend` selects which realization produces it.

Substrate is opt-in in every phase. It is specified in the companion backend
proposal with prototype validation, while the delivered milestone implements
Kubernetes only: a Template selecting `substrate` converges to Failed with
an explicit message instead of pretending to build, and the PoolAutoscaler
call for a Substrate-backed pool returns Unsupported. Substrate PodTemplate
constraints — digest-pinned images, rejected volumes/initContainers/`valueFrom`
— are enforced by that backend and specified in the companion proposal.

### Journey 2: create a warm pool

```python
from openkruise_agents import SandboxSet
from openkruise_agents.models import SandboxSetScaleStrategy, SandboxSetSpec

pool = SandboxSet.create(
    name="code-interpreter",
    template_ref=build.template_id,
    spec=SandboxSetSpec(
        replicas=5,
        scale_strategy=SandboxSetScaleStrategy(max_unavailable=1),
    ),
)
pool.wait_until_ready(timeout=300)
```

In the target model a SandboxSet expresses only pool fields — replicas and
scaling policy — and `template_ref` is its single link to the Template. The
SDK never exposes an inline `template`, `volumeClaimTemplates`, or a repeat
of Template fields such as `runtimes` or `pause_strategy`; the existing
v1alpha1 `EmbeddedSandboxTemplate` form is legacy compatibility and must not
spread to new interfaces.

Naming the pool after the Template is the recommended default: the E2B claim
path resolves the `template` argument of `Sandbox.create()` to the
same-named SandboxSet, so the default pool is discoverable without any
extra mapping. Non-default pools remain addressable through the
`e2b.agents.kruise.io/sandboxset` metadata extension; if multiple pools per
Template are allowed later, the default-pool rule stays the same-name
lookup. This also bounds the correspondence between the two API views: the
E2B surface and the resource API describe the same logical entity when the
pool name matches its `templateRef`, which the recommended naming guarantees.
A namespace can still hold a same-named pool that points elsewhere — the
resource API does not enforce the match — but the mismatch is contained
inside that caller's own namespace, makes the E2B pool view follow the pool's
actual workload rather than the same-named Template, and is a caller-side
configuration error. Adjusting fixed capacity through the resource API
(`PATCH /sandboxsets`) is target-state — the delivered milestone serves
create and read, so pools keep the replicas they were created with until
the PATCH route lands; when an autoscaler manages the pool, capacity
changes go through the autoscaler instead.

### Journey 3: optional autoscaling

```python
from openkruise_agents import PoolAutoscaler
from openkruise_agents.models import (
    CapacityPolicy,
    CapacityScalingRules,
    PoolAutoscalerSpec,
)

autoscaler = PoolAutoscaler.create(
    name="code-interpreter-autoscaler",
    scale_target_ref=pool.name,
    spec=PoolAutoscalerSpec(
        min_replicas=1,
        max_replicas=20,
        capacity_policy=CapacityPolicy(
            target_available=3,
            tolerance="10%",
            scale_up=CapacityScalingRules(stabilization_window_seconds=60),
            scale_down=CapacityScalingRules(stabilization_window_seconds=300),
        ),
    ),
)
autoscaler.wait_until_active()
```

The SDK exposes the full PoolAutoscaler parameter surface — min/max
replicas, capacity policy with tolerance and stabilization windows, cron
policies, and suspend. Each SandboxSet is managed by at most one
PoolAutoscaler. Most fixed-capacity users never create one. PoolAutoscaler
supports only Kubernetes-backed pools in this phase; creating one for a
Substrate-backed target returns an explicit Unsupported error instead of an
ineffective policy.

### Journey 4: create Sandboxes with the community E2B SDK

```python
from e2b_code_interpreter import Sandbox

sandbox = Sandbox.create(template="code-interpreter", timeout=900)
print(sandbox.commands.run("python -V").stdout)
sandbox.pause()
sandbox = sandbox.connect()   # connecting resumes a paused Sandbox
sandbox.kill()
```

The claim path stays on the existing E2B-compatible API:

```text
Sandbox.create(template="code-interpreter")
  → sandbox-manager resolves the Template name to the same-named SandboxSet
  → claims an available warm Sandbox from the pool
  → claim-time initialization: environment injection, identity and
    credentials, dynamic CSI mounts, optional in-place image/resource update
  → runtime (envd) ready → Sandbox connected
```

The Template name is not remapped to community E2B Template Build routes.
Concretely, no E2B Template Build route exists in this codebase and none is
added: the E2B-compatible surface registers only GET `/templates`, GET
`/templates/{templateID}`, and DELETE `/templates/{templateID}`. Those
endpoints continue to resolve the template name to the same-named
SandboxSet — GET returns a pool projection of it, DELETE only accepts
Checkpoint-backed templates and refuses SandboxSet-backed ones — while
Template creation and Build live exclusively on the Kruise resource API.
Both surfaces share the name and describe the same logical entity from two
sides — the E2B surface exposes the pool view, the resource API exposes the
build view — under the recommended same-name pool naming, as bounded in
Journey 2. `Sandbox.create()` therefore continues to receive a logical
Template name, exactly as before; existing E2B SDK calls, snapshots, and
clones are unchanged.

## Scenario Walkthroughs

The three scenarios below show the full target-state API. In the delivered
first milestone, Template validation, templateRef warm pools, and the E2B
claim path are live; checkpoint, image cache, `run_cmd`, and deferred start
are accepted by the SDK and rejected by the server until backend support
lands. Backend mechanics for each scenario are specified in the companion
backend proposal.

### Serving: checkpoint, image cache, deferred business start

For relatively stable business images, Build prepares an image cache and a
checkpoint, and the warm pool restores from them. The user still writes the
business command, but Build and warm-up do not run it — only platform
daemons (for example the agent runtime) and a supervisor-like launcher that
waits for a start signal:

```python
from openkruise_agents import Template
from openkruise_agents.models import (
    BusinessProcessOptions,
    BusinessStartPolicy,
    Container,
    PersistentContent,
    ResourceRequirements,
    RuntimeConfig,
    SandboxTemplateSpec,
    TemplateBuildOptions,
)

build = Template.build(
    name="code-interpreter-v2",
    spec=SandboxTemplateSpec(
        containers=[
            Container(
                name="business",
                image="registry.example.com/code-interpreter@sha256:<digest>",
                command=["/opt/code-interpreter/start.sh"],
                resources=ResourceRequirements(requests={"cpu": "1", "memory": "1Gi"}),
            ),
            Container(
                name="traffic-proxy",
                image="registry.example.com/traffic-proxy@sha256:<digest>",
            ),
        ],
        runtimes=[RuntimeConfig(name="agent-runtime")],
        persistent_contents=[
            PersistentContent.FILESYSTEM,
            PersistentContent.MEMORY,
        ],
    ),
    options=TemplateBuildOptions(
        checkpoint=True,
        image_cache=True,
        business_process=BusinessProcessOptions(
            start_policy=BusinessStartPolicy.AFTER_CLAIM_INITIALIZATION,
        ),
    ),
)
build.wait_until_ready()
assert build.artifact is not None
```

At claim time, in-place image/resource updates, environment injection,
identity initialization, and dynamic mounts complete **before** the start
signal is written; the launcher then execs the original business command,
which sees the claim-level configuration. Deferred start is optional: a
workload that needs no claim-level dynamics can start its business process
during Build and checkpoint the fuller running state instead.

### Custom environment provisioning: run_cmd

For code interpreters and per-project environments, the Template declares
the full PodTemplate (image, files, env, command, resources), and Build runs
a few provisioning commands inside the running business container through
envd. Command effects on the filesystem and process state are frozen into
the checkpoint:

```python
from openkruise_agents import Template, default_build_logger
from openkruise_agents.models import (
    Container,
    PersistentContent,
    RuntimeConfig,
    SandboxTemplateSpec,
    TemplateBuildOptions,
)

template = (
    Template(
        spec=SandboxTemplateSpec(
            containers=[Container(
                name="business",
                image="registry.example.com/python-agent-source:v1",
            )],
            runtimes=[RuntimeConfig(name="agent-runtime")],
            persistent_contents=[PersistentContent.FILESYSTEM],
        ),
        options=TemplateBuildOptions(checkpoint=True),
    )
    .run_cmd("python -m pip install -r /app/requirements.txt")
    .run_cmd(["python -m compileall /app"])
)
build = Template.build(template, "python-agent", on_build_logs=default_build_logger())
build.wait_until_ready()
```

Commands run sequentially (`&&` semantics; the SDK joins list commands and
the server executes them in order), preserve the optional E2B `user`
parameter, and execute only in the business container — multi-container
PodTemplates remain available for sidecars, but the Build command target is
singular.

### RL: base Template, claim-time image replacement

RL fleets can run tens of thousands of distinct task images with low
per-image reuse. A memory checkpoint binds to the platform component
versions captured at build time, so per-image checkpoints would demand a
rebuild storm after every platform upgrade. The RL pattern therefore uses a
base Template without checkpoint or image cache, and replaces the business
image at claim time through in-place update:

```python
from e2b import Sandbox
from openkruise_agents import Template
from openkruise_agents.models import (
    Container,
    RuntimeConfig,
    SandboxTemplateSpec,
    TemplateBuildOptions,
)

build = Template.build(
    name="rl-sandbox",
    spec=SandboxTemplateSpec(
        containers=[Container(
            name="business",
            image="registry.example.com/sandbox-base:latest",
        )],
        runtimes=[RuntimeConfig(name="agent-runtime")],
    ),
    options=TemplateBuildOptions(checkpoint=False, image_cache=False),
)
build.wait_until_ready()

sandbox = Sandbox.create(
    template="rl-sandbox",
    timeout=900,
    envs={"REQUEST_ID": "req-123"},
    metadata={
        "e2b.agents.kruise.io/image": "registry.example.com/rl-task:v12345",
        "e2b.agents.kruise.io/cpu-request": "2",
        "e2b.agents.kruise.io/memory-request": "4Gi",
    },
)
```

The base Template defines the runtime and pool shape; task images are
claimed per request via the `e2b.agents.kruise.io/*` metadata extensions,
and image replacement restarts the business process, so no deferred start
policy is used.

## API Contract

The resource API is served by sandbox-manager at
`/agents.kruise.io/v1alpha1`; it is not a Kubernetes API proxy. API-key
authentication derives the namespace from the caller's team, and request
bodies cannot specify another namespace.

| Method | Path | Semantics |
| --- | --- | --- |
| POST | `/templates` | Persist a Template and start convergence; 201, asynchronous. |
| GET | `/templates/{name}` | Return spec, phase, conditions, and optional artifact. |
| DELETE | `/templates/{name}` | Refuse referenced Templates; otherwise delete Template and artifact. Deleting a missing Template is idempotent. |
| POST/GET | `/sandboxsets`, `/sandboxsets/{name}` | Create and read warm capacity by templateRef. |
| POST/GET | `/poolautoscalers`, `/poolautoscalers/{name}` | Create and read Kubernetes autoscaling. |

PATCH for Templates, PATCH for pools, and PATCH/DELETE for autoscalers are
target-state: the delivered milestone serves creation, reads, and Template
deletion, and the SDK grows those operations when the server routes land. The
Template update route in particular implies generation-based rebuild, whose
semantics are defined above under the immutability model. List operations are
also target-state.

| Status | Meaning |
| --- | --- |
| 400 | Validation failed: unsupported PodTemplate field for the backend, or a Build option the current milestone does not implement. |
| 401/403 | 401: missing or invalid API key. 403: operation not allowed for the caller, or quota exhausted (the established create-path mapping). |
| 404 | Resource not found in the caller's namespace; template deletion is idempotent. |
| 409 | Duplicate creation, or deleting a Template still referenced by a SandboxSet. |
| 501 | The server does not implement this operation for the selected backend (for example PoolAutoscaler with a Substrate target); the request is rejected explicitly instead of silently ignored. |
| 503 | Resource manager unavailable. |

## Data Model

SandboxTemplate already exists with a workload `spec.template` and the
pool-mirroring fields (`persistentContents`, `runtimes`, `pauseStrategy`,
`probes`, `autoPausePolicy`, `volumeClaimTemplates`); those are unchanged.
This proposal adds the following.

**New spec fields:**

```go
type SandboxTemplateSpec struct {
    // ... existing fields unchanged ...

    // Backend selects the build backend ("kubernetes" or "substrate").
    // Defaults to kubernetes; immutable after creation.
    Backend string `json:"backend,omitempty"`

    // TemplateBuildOptions is the declarative snapshot of the Build request.
    TemplateBuildOptions *SandboxTemplateBuildOptions `json:"templateBuildOptions,omitempty"`
}

type SandboxTemplateBuildOptions struct {
    Checkpoint      bool                            `json:"checkpoint,omitempty"`
    ImageCache      bool                            `json:"imageCache,omitempty"`
    BusinessProcess *TemplateBusinessProcessOptions `json:"businessProcess,omitempty"`
    RunCommands     []TemplateBuildRunCommand       `json:"runCommands,omitempty"`
}
```

**New status** — the existing type had no status contract; this defines it:

```go
type SandboxTemplate struct {
    metav1.TypeMeta
    metav1.ObjectMeta

    Spec SandboxTemplateSpec `json:"spec"`

    // Status defines the observed state of SandboxTemplate.
    // +optional
    Status SandboxTemplateStatus `json:"status,omitempty"`
}

type SandboxTemplateStatus struct {
    ObservedGeneration int64                       `json:"observedGeneration,omitempty"`
    Phase              SandboxTemplateBuildPhase   `json:"phase,omitempty"`
    ArtifactRef        *SandboxTemplateArtifactRef `json:"artifactRef,omitempty"`
    Message            string                      `json:"message,omitempty"`
    StartTime          *metav1.Time                `json:"startTime,omitempty"`
    ReadyTime          *metav1.Time                `json:"readyTime,omitempty"`
    Conditions         []metav1.Condition          `json:"conditions,omitempty"`
}
```

The status is written by the SandboxTemplate controller, served through the
status subresource, and projected verbatim into resource API responses. Both
new blocks are optional, so existing objects and clients are unaffected.

Key rules:

- `backend` defaults to `kubernetes` and is immutable. Defaulting runs
  before validation, so clearing the field cannot bypass immutability and
  flip the controller branch.
- `templateBuildOptions` is a declarative snapshot of the Build request
  written by sandbox-manager at creation time and consumed directly by the
  controller reconcile.
- The SandboxTemplate controller is the exclusive Status writer;
  sandbox-manager writes only spec.
- `artifactRef` is empty in the first Kubernetes milestone. Future
  Kubernetes Builds write a checkpoint reference; Substrate writes an
  ActorTemplate reference. It records the backend that produced the
  artifact.
- Template runtime fields (`persistentContents`, `runtimes`,
  `pauseStrategy`, ...) drive SandboxSet revision hashes, so
  `SandboxTemplateSpec` mirrors every pool-level field from
  `SandboxSetSpec` that participates in rolling updates.

## Resource Placement

Resources are declared per container (`containers[].resources`), consistent
with the existing v1alpha1 CRD and the SandboxSet/Sandbox surface, and this
stays the model: per-container requests and limits serve business containers
and sidecars, the E2B compatibility layer derives `cpuCount`/`memoryMB` from
container resources as a read-only projection, and claim-time dynamic
adjustment targets container resources. A Template top-level entry is not
added: it has no Kubernetes precedent for how it would apply across multiple
containers, duplicates the same fields through a second entry point, and
would move claim-side semantics. The Substrate backend is unaffected either
way — it ignores resources at Template stage and consumes them at pool stage
through the WorkerPool (companion backend proposal). If single-container
users report the container wrapper as friction before the SDK reaches a
stable release, an SDK-level convenience parameter can expand to the business
container without adding a CRD field.

## Implementation Details and Constraints

- The resource API follows API → Manager → Infra layering; the controller
  must not import sandbox-manager packages.
- `templateRef` is the only public Template-to-pool linkage; the SDK does
  not expose backend-specific worker configuration.
- The first Kubernetes milestone rejects checkpoint, image-cache, and
  command Build options instead of accepting behavior it cannot provide.
- Private image pulls reuse `imagePullSecrets` from the Template; secret
  contents never enter the SandboxTemplate, Build status, or logs.
- The SDK client falls back to `E2B_API_KEY` and `E2B_API_URL`, so a
  deployment that already points the E2B SDK at sandbox-manager needs no
  extra configuration.
- E2B `Sandbox.create()` continues to receive a logical Template name; it is
  not remapped to community E2B Template Build routes.

## Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Ready is mistaken for a checkpointed image | Document no-artifact Ready semantics and leave ArtifactRef empty in the first milestone. |
| TemplateRef drops workload settings | Test revision construction and fields copied to created Sandboxes. |
| Backend-specific fields are silently ignored | Validate by selected backend with field-level errors; reject unsupported input at admission. |
| Template deletion breaks an active pool | Refuse deletion while SandboxSet references exist. |
| Container-level resources add friction for single-container users | Watch for feedback before the stable SDK release; an SDK convenience parameter can expand to the business container without a CRD change. |
| SDK Kubernetes models drift from upstream CRDs | Generate SDK models from a pinned upstream Kubernetes swagger instead of hand-writing a subset. |

## Alternatives

Requiring checkpoint Build before a Template is usable delays basic warm-pool
use until an unimplemented provider exists. Letting the SDK write CRs
directly would leak cluster credentials. Repeating PodTemplate in SandboxSet
duplicates workload intent. Flattening Template, pool, and autoscaler into
one `build()` call couples unrelated lifecycles. This proposal avoids all
four.

## Upgrade Strategy

New fields are optional. Templates without `backend` mean Kubernetes.
Existing inline SandboxSets remain supported; new SDK clients always
generate `templateRef`. No E2B lifecycle route is removed.

One behavioral change is deliberate: templateRef pools inherit unset pool
fields from the referenced Template instead of treating Template runtime
fields as informational. Pools that already reference a Template will see
their revision hash change once on upgrade, causing a single rolling update
of their warm Sandboxes.

## Test Plan

- Resource API authentication, namespace isolation, conflict, and delete
  tests.
- Template controller validation and Ready/Failed status tests.
- SandboxSet templateRef inheritance and revision tests.
- Kubernetes autoscaler tests and unsupported-backend tests.
- SDK Template → SandboxSet → E2B claim integration test.
- End-to-end stages delivered with the first milestone: admission and
  reconcile validation, backend immutability including
  field-removal bypass attempts, templateRef inheritance with revision
  rolling, and the E2B claim path against a live cluster.

## Implementation History

- 2026-09-16: Initial draft.
- 2026-10-12: Reworked for upstream: no pre-existing Substrate POC is
  assumed, and the initial Kubernetes path does not require a Build
  artifact.
- 2026-10-14: First Kubernetes milestone implemented and validated end to
  end (SDK, resource API, controller validation, templateRef inheritance,
  E2B claim path). Expanded for design review with scenario walkthroughs,
  Build-option semantics, and the resource-placement decision (per
  container, settled).
- 2026-10-15: Section-by-section review pass. Non-Goals split into
  permanent design boundaries versus Future Work; the API contract table
  aligned with the delivered routes (pool PATCH and autoscaler PATCH/DELETE
  documented as target-state); Template get/delete documented as part of
  the SDK lifecycle. Applied the external review decisions: Templates are
  immutable in this API version (generation-based rebuild and the update
  route are target-state), the Summary claims a create-and-observe
  lifecycle, backend neutrality is stated as one API shape with capability
  validation, the same-name pool correspondence is bounded to recommended
  naming (mismatches stay inside the caller's namespace), and the
  401/403/501 status rows are disambiguated.
