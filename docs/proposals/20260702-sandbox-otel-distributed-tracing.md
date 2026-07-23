---
title: OpenTelemetry-based Distributed Tracing for Sandbox Lifecycle
authors:
  - "@zhaomingshan"
reviewers:
  - "@TBD"
creation-date: 2026-07-02
last-updated: 2026-07-02
status: provisional
see-also:
  - "/docs/proposals/20260422-sandbox-prometheus-metrics.md"
---

# OpenTelemetry-based Distributed Tracing for Sandbox Lifecycle

## Table of Contents

- [Overview](#overview)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals / Future Work](#non-goals--future-work)
- [Design](#design)
  - [Overall Architecture](#overall-architecture)
  - [Trace Boundaries and Context Propagation](#trace-boundaries-and-context-propagation)
  - [Multi-Reconcile Span Modeling](#multi-reconcile-span-modeling)
  - [Span Classification](#span-classification)
  - [End-to-End Trace Examples](#end-to-end-trace-examples)
  - [Package Structure](#package-structure)
  - [HTTP Middleware Design](#http-middleware-design)
  - [Annotation Injection Points](#annotation-injection-points)
  - [Reconcile Span Placement](#reconcile-span-placement)
  - [Controller Child Spans](#controller-child-spans)
  - [Visualization Backend](#visualization-backend)
  - [Configuration](#configuration)
  - [Feature Gate](#feature-gate)
  - [Cross-Operation Context Propagation](#cross-operation-context-propagation)
- [User Stories](#user-stories)
- [Implementation Details and Constraints](#implementation-details-and-constraints)
- [Risks and Mitigations](#risks-and-mitigations)
- [Alternative Approaches](#alternative-approaches)
- [Upgrade Strategy](#upgrade-strategy)
- [Implementation History](#implementation-history)

## Overview

This proposal introduces distributed tracing capabilities for sandbox lifecycle management,
using the **OpenTelemetry** (OTel) SDK. Tracing covers two core components:

- **sandbox-manager**: API-level tracing for all E2B-compatible HTTP endpoints. sandbox-manager's
  HTTP requests are synchronous (e.g., CreateSandbox waits for Pod Ready before returning), so the
  root Span's Duration covers the full end-to-end latency.

- **sandbox-controller**: Tracing for phase transitions, Pod operations, Checkpoint, and Reuse
  flows within the Reconcile loop. A single user operation may trigger multiple Reconcile passes,
  each producing a sibling Span.

Trace context is propagated from sandbox-manager to sandbox-controller via the Sandbox CRD
Annotation, enabling end-to-end sandbox lifecycle correlation across the two components.
**Each user operation (create, delete, pause, resume) is an independent Trace with its own TraceID.**

## Motivation

### Current Problems

1. **No context propagation**: Each operation log entry is isolated, with no parent-child Span
   relationship between operations
2. **No cross-component correlation**: sandbox-manager operations and sandbox-controller Reconcile
   loops cannot be correlated
3. **No standard tracing backend**: Incompatible with standard observability tools (Jaeger, Tempo, etc.)
4. **No bottleneck analysis**: When sandbox creation takes 30 seconds, cannot distinguish whether
   the bottleneck is in controller logic, K8s API calls, or Pod startup (image pull, container
   initialization)

### Key Design Insight

Using CR ID or Pod UID as TraceID is inappropriate. Each user operation (create, pause, resume,
delete) should be an independent Trace. Reasons:

- Each operation is triggered by an independent HTTP request with its own lifecycle
- "Why is creation slow?" and "Why is resume slow?" are different Traces, enabling targeted analysis
- sandbox-controller can clearly determine whether the current Reconcile is the start/end of work,
  and construct tracing based on the `agents.kruise.io/trace-context` annotation

### Goals

1. **sandbox-manager HTTP tracing**: Create a root Span for each API request, covering the full
   synchronous wait time
2. **sandbox-controller Reconcile tracing**: Create sibling Spans for each Reconcile that performs
   actual work
3. **Cross-component context propagation**: Propagate W3C Trace Context via Sandbox CRD Annotation
4. **OTLP gRPC export**: Standard protocol, backend-agnostic (Jaeger, Tempo, etc.)
5. **Feature Gate control**: Enable/disable tracing with zero overhead when disabled
6. **Rich Span attributes**: Support trace search and filtering in any OTLP-compatible backend

### Non-Goals / Future Work

- **Log correlation**: Embedding trace ID in klog/logr output (future via `otelslog`)
- **Metrics migration**: Existing Prometheus metrics remain unchanged
- **agent-runtime tracing**: Tracing for the sidecar inside sandbox Pods is out of scope
- **K8s client-go auto-instrumentation**: Only instrument our own code (future via otelhttp + k8s
  client)
- **Custom sampling strategies**: Default `ParentBased(AlwaysSample)`, runtime configuration deferred
- **DAG diagnostic views**: Standard span tree + waterfall suffices for the two-component
  architecture; DAG rendering handled by the backend

## Design

### Overall Architecture

```plaintext
┌──────────────────────────────────────────────────────────────────────────┐
│                         User / E2B SDK                                   │
│            (HTTP request, optionally carrying W3C traceparent)           │
└──────────────────────────────┬───────────────────────────────────────────┘
                               │
                               ▼
┌──────────────────────────────────────────────────────────────────────────┐
│                        sandbox-manager                                    │
│                                                                           │
│  ┌─────────────────────────────────────────────────────────────────────┐ │
│  │  otelhttp.NewHandler(mux)  ← HTTP middleware                       │ │
│  │  ┌───────────────────────────────────────────────────────────────┐  │ │
│  │  │  E2B Handler: CreateSandbox                                    │  │ │
│  │  │  Span: sandbox-manager.CreateSandbox (root Span)               │  │ │
│  │  │    ├─ Span: manager.ClaimSandbox                               │  │ │
│  │  │    │    └─ Span: infra.ClaimSandbox                             │  │ │
│  │  │    └─ Inject traceparent into Sandbox annotation               │  │ │
│  │  │  (root Span stays open, Duration = end-to-end latency)        │  │ │
│  │  └───────────────────────────────────────────────────────────────┘  │ │
│  └─────────────────────────────────────────────────────────────────────┘ │
│  TracerProvider ──► OTLP gRPC Exporter ──► OTel Collector               │
└──────────────────────────────┬───────────────────────────────────────────┘
                               │ K8s API: Create/Update Sandbox CR
                               │ (trace context in annotation)
                               ▼
┌──────────────────────────────────────────────────────────────────────────┐
│                      sandbox-controller                                   │
│  ┌─────────────────────────────────────────────────────────────────────┐ │
│  │  Reconcile #1: Pod not found → create Pod                          │ │
│  │  Span: controller.Reconcile (child of root Span)                   │ │
│  │    └─ Span: controller.CreatePod                                    │ │
│  │  Reconcile #2~#6: Pod Pending → check status (sibling Spans)      │ │
│  │  Reconcile #7: Pod Ready → update status                           │ │
│  │    └─ Span: controller.updateSandboxStatus                          │ │
│  │  Reconcile #8: Already Running, no work → no Span created          │ │
│  └─────────────────────────────────────────────────────────────────────┘ │
│  TracerProvider ──► OTLP gRPC Exporter ──► OTel Collector               │
└──────────────────────────────────────────────────────────────────────────┘
```

### Trace Boundaries and Context Propagation

#### Trace Boundary: Independent Trace per User Operation

Each user operation (create, delete, pause, resume) is an independent Trace with its own TraceID:

```plaintext
Sandbox lifecycle:
  ├─ Trace #1: CreateSandbox  (TraceID: aaa)  create → wait for Pod Ready
  ├─ Trace #2: PauseSandbox   (TraceID: bbb)  pause → wait for Checkpoint + Pod deletion
  ├─ Trace #3: ResumeSandbox  (TraceID: ccc)  resume → wait for Pod to become Ready again
  └─ Trace #4: DeleteSandbox  (TraceID: ddd)  delete → wait for Pod termination or reuse completion
```

#### Annotation Propagation Mechanism

sandbox-manager and sandbox-controller communicate via the K8s API Server (Sandbox CRD), requiring
a mechanism to propagate W3C Trace Context across this boundary.

**Annotation key**: `agents.kruise.io/trace-context`

**Value format**: W3C `traceparent` header (e.g.,
`00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01`)

sandbox-manager injects `traceparent` when creating or updating Sandbox resources; the controller
extracts the trace context at the beginning of each Reconcile.

#### Annotation Update Contract

Each user operation **overwrites** the traceparent in the annotation:

```plaintext
CreateSandbox  → inject traceparent-A → controller uses traceparent-A
PauseSandbox   → inject traceparent-B (overwrites A) → controller uses traceparent-B
ResumeSandbox  → inject traceparent-C (overwrites B) → controller uses traceparent-C
DeleteSandbox  → inject traceparent-D (overwrites C) → controller uses traceparent-D
```

#### Degradation Handling

If the annotation does not exist (e.g., sandbox created directly via `kubectl`), the controller
starts a new root Span. The Trace can still be correlated via sandbox UID as a Span attribute,
supporting manual search.

### Multi-Reconcile Span Modeling

#### Core Challenge

sandbox-manager's HTTP request is synchronous (CreateSandbox waits for Pod Ready before returning),
during which sandbox-controller is triggered for Reconcile multiple times. How to model the Spans
of these Reconcile passes is the core design question.

#### Sibling Span Model

Multiple Reconcile passes produce **sibling Spans** (same TraceID, same ParentSpanID), not
parent-child Spans. Each Reconcile creates its own Span and ends it before returning.

```plaintext
sandbox-manager.CreateSandbox  ████████████████████████████████████████  (30s, root Span)
  ├─ controller.Reconcile #1   ████████                                     (200ms)
  │    └─ controller.CreatePod  ██████                                       (180ms)
  │           ↑ 5s interval (requeue wait)
  ├─ controller.Reconcile #2   █                                            (10ms)
  │           ↑ 5s interval
  ├─ controller.Reconcile #3~#6  █ (10ms each) ← check Pod status
  │           ↑ 5s interval
  └─ controller.Reconcile #7   ███                                          (30ms)
       └─ controller.updateSandboxStatus  ██                                  (20ms)
```

**The gaps between Spans are more valuable than the Spans themselves**: All controller Reconcile
passes combined take only ~280ms, but the root Span is 30 seconds. Those 29.7 seconds are Pod
startup time — the bottleneck is immediately visible.

#### Check Before Creating Span

The controller checks whether work is needed before creating a Span:

```plaintext
Reconcile(ctx, req):
    1. Get Sandbox object
    2. Check if work is needed?
       └─ Sandbox is in terminal state and spec unchanged → return immediately, no Span
    3. Confirmed work is needed
       → Extract trace context from annotation
       → Create Reconcile Span
       → Execute operations (create child Spans for heavy IO)
       → Span.End()
```

#### Child Span Granularity Guidelines

| Operation | Typical Duration | Create Child Span | Reason |
|----------|-----------------|-------------------|--------|
| Create Pod | 100-500ms | Yes | Heavy K8s API call |
| Delete Pod | 100ms-6s | Yes | Pod graceful termination |
| Patch Pod | 10-100ms | Yes | Write operation |
| Update Sandbox Status | 10-50ms | Yes | Write operation |
| Checkpoint CR operation | 1-5s | Yes | Heavy async operation |
| CSI storage mount | 100ms-3s | Yes | Calls CSI driver to mount volume |
| Get Pod | 5-10ms | No | Lightweight read operation |
| Logic checks | <1ms | No | No IO involved |

#### Annotation Lifecycle

The annotation is **not proactively cleaned up**. After an operation completes, the annotation
remains on the Sandbox CR, but the "check before creating Span" principle ensures that subsequent
unrelated Reconcile passes do not produce Spans. The next user operation will naturally overwrite it.

### Span Classification

#### sandbox-manager Spans

| Span Name | Trigger | Attributes | Type |
|-----------|---------|------------|------|
| `sandbox-manager.CreateSandbox` | `POST /sandboxes` | `sandbox.id`, `template`, `namespace` | Root Span |
| `sandbox-manager.DeleteSandbox` | `DELETE /sandboxes/{id}` | `sandbox.id`, `reuse.enabled` | Root Span |
| `sandbox-manager.PauseSandbox` | `POST /sandboxes/{id}/pause` | `sandbox.id` | Root Span |
| `sandbox-manager.ResumeSandbox` | `POST /sandboxes/{id}/resume` | `sandbox.id` | Root Span |
| `sandbox-manager.ConnectSandbox` | `POST /sandboxes/{id}/connect` | `sandbox.id`, `timeout` | Root Span |
| `sandbox-manager.CreateSnapshot` | `POST /sandboxes/{id}/snapshots` | `sandbox.id` | Root Span |
| `sandbox-manager.DescribeSandbox` | `GET /sandboxes/{id}` | `sandbox.id` | Root Span (read-only) |
| `sandbox-manager.ListSandboxes` | `GET /v2/sandboxes` | (none) | Root Span (read-only) |
| `manager.ClaimSandbox` | Within CreateSandbox | `claim.lock_type`, `claim.retries` | Child Span |
| `manager.CloneSandbox` | Within CreateSandbox | | Child Span |
| `manager.DeleteSandbox` | Within DeleteSandbox | `reuse.triggered` | Child Span |
| `manager.PauseSandbox` | Within PauseSandbox | | Child Span |
| `manager.ResumeSandbox` | Within ResumeSandbox | | Child Span |
| `infra.ClaimSandbox` | Within manager.ClaimSandbox | `claim.duration` | Child Span, injects annotation |
| `infra.CloneSandbox` | Within manager.CloneSandbox | `clone.checkpoint_id` | Child Span, injects annotation |
| `manager.CreateSnapshot` | Within CreateSnapshot | `snapshot.keep_running`, `snapshot.ttl` | Child Span |
| `infra.CreateCheckpoint` | Within manager.CreateSnapshot | `checkpoint.duration` | Child Span, injects annotation to Checkpoint CR |
| `infra.ProcessCSIMounts` | Within ClaimSandbox/CloneSandbox | `csi.volume_count`, `csi.volumes` | Child Span, dynamic storage mount |
| `infra.Pause` | Within manager.PauseSandbox | | Child Span, injects annotation |
| `infra.Resume` | Within manager.ResumeSandbox | | Child Span, injects annotation |
| `proxy.syncRoute` | After each mutation operation | `route.id`, `peers.synced` | Child Span |

#### sandbox-controller Spans

| Span Name | Trigger | Attributes | Child Spans |
|-----------|---------|------------|-------------|
| `controller.Reconcile` | Each Reconcile with work | `sandbox.name`, `sandbox.namespace`, `sandbox.phase` | As needed |
| `controller.EnsureSandboxRunning` | Phase = Pending | | CreatePod, PatchPod |
| `controller.EnsureSandboxUpdated` | Phase = Running | | (in-place update) |
| `controller.EnsureSandboxPaused` | Phase = Paused | | DeletePod, Checkpoint |
| `controller.EnsureSandboxResumed` | Phase = Resuming | | CreatePod, Initialize |
| `controller.EnsureSandboxUpgraded` | Phase = Upgrading | | DeletePod, CreatePod, Initialize |
| `controller.EnsureSandboxTerminated` | DeletionTimestamp not empty | | DeletePod |
| `controller.CreatePod` | Pod creation | `pod.name` | — |
| `controller.DeletePod` | Pod deletion | `pod.name` | — |
| `controller.PatchPod` | Pod annotation patch | `pod.name`, `patch.type` | — |
| `controller.Checkpoint` | Checkpoint CR operation | `checkpoint.name` | — |
| `controller.updateSandboxStatus` | Status update | `phase.before`, `phase.after` | — |

#### checkpoint-controller Spans

| Span Name | Trigger | Attributes | Child Spans |
|-----------|---------|------------|-------------|
| `checkpoint.Reconcile` | Each Reconcile with work | `checkpoint.name`, `checkpoint.phase`, `sandbox.id` | As needed |
| `checkpoint.Snapshot` | Snapshot creation operation | `checkpoint.id`, `persistent_contents` | — |
| `checkpoint.updateStatus` | Status update | `phase.before`, `phase.after` | — |

### End-to-End Trace Examples

#### Create Sandbox

```plaintext
Trace #1 (TraceID: aaa)

sandbox-manager.CreateSandbox
████████████████████████████████████████████████████████████████████  (30s, root Span)
  ├─ manager.ClaimSandbox
  │  ├─ infra.ClaimSandbox                    ████                     (80ms)
  │  └─ proxy.syncRoute                        █                        (3ms)
  └─ Inject trace-context annotation (traceparent-A)
         │
         ▼  controller Reconcile (sibling Spans)
         ├─ controller.Reconcile #1  ████████                                (200ms)
         │    └─ controller.CreatePod  ██████                               (180ms)
         ├─ controller.Reconcile #2~#6  █ (10ms each) ← check Pod status
         └─ controller.Reconcile #7  ███                                    (30ms)
              └─ controller.updateSandboxStatus  ██                          (20ms)

Key insights:
  - Root Span 30s = end-to-end latency (user-perceived)
  - Total Reconcile ~280ms = controller processing time
  - Gaps between Spans ~29.7s = Pod startup time (image pull, container start)
  - Bottleneck: Pod startup (not controller)
```

#### Pause Sandbox

```plaintext
Trace #2 (TraceID: bbb)

sandbox-manager.PauseSandbox
████████████████████████████████████████████████████████████  (12s, root Span)
  ├─ manager.PauseSandbox
  │  └─ infra.Pause  ████                                     (50ms)
  └─ Inject trace-context annotation (traceparent-B, overwrites A)
         ├─ controller.Reconcile #1  ████████████                        (5s)
         │    └─ controller.Checkpoint  ██████████                        (5s) ← create Checkpoint CR
         ├─ controller.Reconcile #2  █                                    (10ms) ← Checkpoint in progress
         └─ controller.Reconcile #3  ████████████                        (6s)
              └─ controller.DeletePod  ██████████                        (5.5s) ← Pod graceful termination

Key insights: Checkpoint 5s + DeletePod 5.5s = two main time-consuming steps
```

#### Resume Sandbox

```plaintext
Trace #3 (TraceID: ccc)

sandbox-manager.ResumeSandbox
████████████████████████████████████████████████████████████████  (15s, root Span)
  ├─ manager.ResumeSandbox
  │  └─ infra.Resume  ████                                     (50ms)
  └─ Inject trace-context annotation (traceparent-C, overwrites B)
         ├─ controller.Reconcile #1  ██████                              (2s)
         │    └─ controller.PatchPod  █████                               (2s) ← restore Pod
         ├─ controller.Reconcile #2  █                                   (10ms) ← wait for Ready
         └─ controller.Reconcile #4  ██████                             (3s)
              └─ controller.Initialize  █████                            (3s) ← Runtime initialization

Key insights: Resume 15s vs Create 30s (saves image pull); PatchPod slower than CreatePod but
total time shorter; Initialize 3s is additional overhead
```

#### Delete Sandbox (with Reuse)

```plaintext
Trace #4a (TraceID: ddd, reuse=true)

sandbox-manager.DeleteSandbox
████████████████████████████████████████  (200ms, root Span)
  └─ controller.Reconcile #1  ████                               (50ms)
       └─ controller.PatchPod  ███                                (40ms) ← reset annotations

Key insights: 200ms vs Create's 30s (600x speedup)
```

#### Delete Sandbox (without Reuse)

```plaintext
Trace #4b (TraceID: ddd, reuse=false)

sandbox-manager.DeleteSandbox
████████████████████████████████████████████  (8s, root Span)
  ├─ controller.Reconcile #1  ████████████                        (6s)
  │    └─ controller.DeletePod  ██████████                        (5.5s) ← Pod graceful termination
  └─ controller.Reconcile #2  ██                                  (20ms)
       └─ controller.updateSandboxStatus  █                       (10ms)

Key insights: DeletePod 5.5s is the main cost; reuse vs delete: 200ms vs 8s (40x)
```

#### Create Snapshot (CreateSnapshot)

User creates a snapshot of a running sandbox to save the current filesystem state.
sandbox-manager synchronously waits for Checkpoint completion (`WaitSuccessTimeout` default 60s).

```plaintext
Trace #5 (TraceID: eee)

Timeline:  0s        10s       20s       30s       40s       50s       60s
           │         │         │         │         │         │         │
           ▼         ▼         ▼         ▼         ▼         ▼         ▼

sandbox-manager.CreateSnapshot
████████████████████████████████████████████████████████████████████  (45s, root Span)
  ├─ manager.CreateSnapshot
  │  └─ infra.CreateCheckpoint  ██████                              (100ms)
  │      └─ Create Checkpoint CR + SandboxTemplate CR
  │      └─ Inject trace-context annotation into Checkpoint CR
  └─ Synchronously wait for Checkpoint completion (WaitSuccessTimeout=60s)
         │
         ▼  checkpoint-controller Reconcile (sibling Spans)
         │
         ├─ checkpoint.Reconcile #1  ████████████████████████████████████  (40s)
         │    └─ checkpoint.Snapshot  ██████████████████████████████████   (38s) ← snapshot creation
         ├─ checkpoint.Reconcile #2  █                                     (10ms) ← update status
         └─ checkpoint.Reconcile #3  █                                     (10ms) ← complete

Key insights:
  - Root Span 45s = snapshot end-to-end latency (user-perceived)
  - checkpoint.Snapshot 38s = actual snapshot operation time (bottleneck here)
  - Manager side only 100ms (creating CR), remaining time all waiting
  - Compared to Checkpoint 5s in PauseSandbox: standalone snapshot may have larger data
```

#### Clone Sandbox from Checkpoint

Create a new sandbox using a snapshot ID as a template, restoring the filesystem state.

```plaintext
Trace #6 (TraceID: fff)

Timeline:  0s        3s        6s        9s       12s       15s       20s
           │         │         │         │         │         │         │
           ▼         ▼         ▼         ▼         ▼         ▼         ▼

sandbox-manager.CreateSandbox (clone from Checkpoint)
████████████████████████████████████████████████████████████████████  (20s, root Span)
  ├─ manager.CloneSandbox
  │  ├─ infra.CloneSandbox
  │  │  ├─ Find Checkpoint + SandboxTemplate  ████              (80ms)
  │  │  ├─ Create Sandbox CR  █                                       (20ms) ← inject annotation
  │  │  ├─ Wait for Sandbox Ready  ████████████████████               (15s) ← Pod restored from snapshot
  │  │  ├─ Re-init Runtime  ████                                    (3s)
  │  │  └─ CSI Mount  ████                                          (500ms) ← dynamic storage mount (if any)
  │  └─ proxy.syncRoute  █                                          (3ms)
  └─ Inject trace-context annotation into Sandbox CR (traceparent-F)
         │
         ▼  sandbox-controller Reconcile (sibling Spans)
         │
         ├─ controller.Reconcile #1  ██████                              (200ms)
         │    └─ controller.CreatePod  █████                               (180ms) ← restore Pod from snapshot
         ├─ controller.Reconcile #2~#5  █ (10ms each) ← check Pod status
         └─ controller.Reconcile #6  ███                                (30ms)
              └─ controller.updateSandboxStatus  ██                    (20ms)

Key insights:
  - Clone 20s vs Create 30s: saves image pull (snapshot restore is faster)
  - But adds Re-init Runtime 3s (runtime needs re-initialization)
  - Compared to Resume 15s: resume doesn't need Pod recreation, clone does
  - Clone time breakdown: find 80ms + wait Ready 15s + Re-init 3s + other ~2s
```

### Package Structure

New top-level package `pkg/tracing/`:

```plaintext
pkg/tracing/
├── provider.go        # TracerProvider initialization, OTLP gRPC exporter, Resource config
├── provider_test.go   # provider unit tests
├── propagator.go      # trace context inject/extract (annotation ↔ context)
├── propagator_test.go # annotation propagation unit tests
├── middleware.go      # sandbox-manager HTTP tracing middleware
├── middleware_test.go # HTTP middleware unit tests
├── reconcile.go       # sandbox-controller Reconcile Span helper functions
├── reconcile_test.go  # Reconcile helper unit tests
├── spans.go           # Span name and attribute key constants
└── doc.go             # package documentation
```

**Core function signatures**:

```go
// pkg/tracing/provider.go
type Config struct {
    Enabled       bool
    Endpoint      string // OTLP gRPC endpoint, e.g., "otel-collector:4317"
    ServiceName   string // e.g., "sandbox-controller" or "sandbox-manager"
    SamplingRatio float64 // 0.0 to 1.0, default 1.0
    Insecure      bool    // whether to use insecure gRPC (dev environment)
}
func InitTracerProvider(ctx context.Context, cfg Config) (func(context.Context) error, error)
func Tracer(name string) trace.Tracer

// pkg/tracing/propagator.go
const TraceContextAnnotationKey = "agents.kruise.io/trace-context"
func InjectTraceContext(ctx context.Context, annotations map[string]string) map[string]string
func ExtractTraceContext(ctx context.Context, annotations map[string]string) context.Context

// pkg/tracing/middleware.go
func HTTPMiddleware(handler http.Handler, serviceName string) http.Handler

// pkg/tracing/reconcile.go
func StartReconcileSpan(ctx context.Context, obj client.Object, controllerName string) (context.Context, trace.Span)
func StartChildSpan(ctx context.Context, spanName string, attrs ...trace.SpanOption) (context.Context, trace.Span)
```

### HTTP Middleware Design

sandbox-manager uses `otelhttp.NewHandler` to wrap the entire mux, automatically creating a root
Span for every HTTP request. Zero intrusion, no need to modify existing route registration code.

**Integration**: Before starting the HTTP server, wrap the mux with `otelhttp.NewHandler`:

```go
handler := otelhttp.NewHandler(mux, "sandbox-manager")
server := &http.Server{Handler: handler}
```

Span names follow the `{HTTP_METHOD} {HTTP_PATH}` format (e.g., `POST /sandboxes`), standard
otelhttp behavior. Span attributes include `http.method`, `http.url`, `http.status_code`, etc.

**Future optimization**: If semantic Span naming is needed (e.g., `sandbox-manager.CreateSandbox`),
a `web.MiddleWare` middleware approach can be adopted in a later version to precisely map routes to
named Spans.

### Annotation Injection Points

The entry points for operating on Sandbox CRs in sandbox-manager are scattered across multiple
files:

| Operation | Code Entry File | Injection Point |
|-----------|----------------|-----------------|
| Create (Claim) | `infra/sandboxcr/claim.go` → `TryClaimSandbox` | Before creating/updating Sandbox CR |
| Create (Clone) | `infra/sandboxcr/clone.go` → `CloneSandbox` | Before creating Sandbox CR |
| Pause | `infra/sandboxcr/sandbox.go` → `Sandbox.Pause` → `retryUpdate` | In update modifier |
| Resume | `infra/sandboxcr/sandbox.go` → `Sandbox.Resume` → `retryUpdate` | In update modifier |
| Delete | `infra/sandboxcr/sandbox.go` → `Sandbox.Kill` | Before delete/update |
| CreateSnapshot | `infra/sandboxcr/clone.go` → `CreateCheckpoint` | Before creating Checkpoint CR |
| Clone from Checkpoint | `infra/sandboxcr/clone.go` → `createSandboxFromCheckpoint` | Before creating Sandbox CR |

**Sandbox CR injection**: Uniformly call `tracing.InjectTraceContext(ctx, &sandbox.Annotations)` at
all places that write to Sandbox CRs.

**Checkpoint CR injection**: When creating a Checkpoint CR in `CreateCheckpoint()`, also inject
trace-context into the Checkpoint CR's annotation, enabling checkpoint-controller to establish the
parent-child relationship.

### Reconcile Span Placement

In `pkg/controller/sandbox/sandbox_controller.go`'s `Reconcile`, the Span is created **after** the
following early return paths:

1. ✅ Sandbox not found → return (no Span needed)
2. ✅ Expectation not satisfied → requeue (no Span needed)
3. ✅ Terminal state (Failed/Succeeded) → return (no Span needed)
4. ✅ Empty template (after termination handling) → return (no Span needed)

Specifically, the placement is after `addSandboxFinalizerAndHash` and before `calculateStatus`:

```go
// sandbox_controller.go Reconcile():
box, err = r.addSandboxFinalizerAndHash(ctx, box)
if err != nil { return reconcile.Result{}, err }

// --- Tracing: create Reconcile Span ---
reconcileCtx, reconcileSpan := tracing.StartReconcileSpan(ctx, box, "sandbox-controller")
defer reconcileSpan.End()

// calculate sandbox status
var shouldRequeue bool
newStatus, shouldRequeue = calculateStatus(args)
// ...
```

### Controller Child Spans

Child Span creation locations in `pkg/controller/sandbox/core/common_control.go`:

| Span | File | Location |
|------|------|----------|
| `controller.CreatePod` | `common_control.go` | Around `r.Create(ctx, pod)` in `createPod()` |
| `controller.DeletePod` | `common_control.go` | Around `r.Delete(ctx, pod)` in `EnsureSandboxPaused`/`EnsureSandboxTerminated` |
| `controller.PatchPod` | `common_control.go` | Around Pod annotation patch |
| `controller.updateSandboxStatus` | `sandbox_controller.go` | Around `r.Status().Patch()` in `updateSandboxStatus()` |

#### checkpoint-controller Child Spans

| Span | File | Location |
|------|------|----------|
| `checkpoint.Reconcile` | checkpoint controller's Reconcile | After fetching CR, before status calculation (same pattern as sandbox-controller) |
| `checkpoint.Snapshot` | Actual snapshot operation | Around snapshot API call |
| `checkpoint.updateStatus` | Status update | Around `Status().Patch()` |

checkpoint-controller follows the same "check before creating Span" principle as sandbox-controller:
Reconcile passes for terminal states (Succeeded/Failed) do not create Spans.

### Visualization Backend

Uses standard OTLP gRPC export, backend-agnostic. Any OTLP-compatible backend can consume the data:

| Scenario | Recommended Backend | Reason |
|----------|---------------------|--------|
| Enterprise deployment | Any OTLP backend | Existing infrastructure, strong diagnostic views |
| Open source community | Jaeger | Lightweight single-container deployment |
| Existing Grafana | Grafana Tempo | Unified dashboard with metrics |

Standard span tree + waterfall visualization (natively supported by Jaeger/Tempo) suffices for
the two-component architecture. DAG rendering is handled by the backend (e.g., some backends render
DAG from standard OTLP data).

**OTel Collector pipeline**:

```plaintext
sandbox-manager  ──┐
                   ├──► OTel Collector (OTLP gRPC :4317) ──► [Backend: Jaeger/Tempo]
sandbox-controller ┘
```

### Configuration

**CLI flags** (applicable to both sandbox-manager and sandbox-controller):

```go
flag.BoolVar(&tracingEnabled, "tracing-enabled", false, "Enable OpenTelemetry distributed tracing")
flag.StringVar(&tracingEndpoint, "tracing-endpoint", "", "OTLP gRPC export endpoint")
flag.Float64Var(&tracingSamplingRatio, "tracing-sampling-ratio", 1.0, "Trace sampling ratio")
flag.BoolVar(&tracingInsecure, "tracing-insecure", true, "Whether to use insecure gRPC export")
```

**Environment variables** (standard OTel SDK conventions):

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector.observability.svc:4317
OTEL_SERVICE_NAME=sandbox-controller  # or sandbox-manager
OTEL_TRACES_SAMPLER=parentbased_always_on
```

CLI flags take precedence over environment variables.

### Feature Gate

New feature gate `SandboxTracingGate`, only affects sandbox-controller (imports `pkg/features`).
sandbox-manager uses CLI flags (per AGENTS.md constraint: must not import `pkg/features`).

```go
// pkg/features/features.go
SandboxTracingGate featuregate.Feature = "SandboxTracing"
// Default: false (Alpha)
SandboxTracingGate: {Default: false, PreRelease: featuregate.Alpha},
```

When disabled, all tracing operations become no-ops (OTel SDK defaults to `NoopTracerProvider`).

### Cross-Operation Context Propagation

#### Multi-Operation Intersection Scenario

When multiple operations target the same Sandbox simultaneously (e.g., checkpoint triggered during
an upgrade), the controller needs to determine which operation's Trace the Reconcile belongs to.

**Solution**: Each operation writes its own `traceparent` to the Sandbox annotation. When creating
a Checkpoint CR, the current trace context is propagated to the Checkpoint's annotation:

```plaintext
Upgrade operation (TraceID: eee):
  ├─ sandbox-manager triggers upgrade → injects traceparent-E into Sandbox annotation
  ├─ controller.Reconcile: detects upgrade → creates Checkpoint CR
  │    └─ Injects traceparent-E into Checkpoint CR annotation
  ├─ checkpoint-controller.Reconcile: extracts traceparent-E from Checkpoint CR
  │    └─ Span: checkpoint.Reconcile (child Span of trace-E)
  └─ controller.Reconcile: checkpoint complete → continue upgrade
```

The controller determines whether a checkpoint is a sub-step of the current operation or an
independent operation by checking the trace-context annotation on the Sandbox CR.

#### Standalone Snapshot Operation

When a user creates a snapshot via `POST /sandboxes/{id}/snapshots`, sandbox-manager directly
creates a Checkpoint CR and injects trace-context. This is an independent Trace, unrelated to the
source Sandbox's creation Trace:

```plaintext
Source sandbox creation (TraceID: aaa)    Snapshot operation (TraceID: eee)
  └─ controller.Reconcile ...               ├─ sandbox-manager.CreateSnapshot (root Span)
                                             └─ infra.CreateCheckpoint (injects traceparent-E into Checkpoint CR)
                                                  └─ checkpoint.Reconcile (extracts traceparent-E from Checkpoint CR)
                                                       └─ checkpoint.Snapshot
```

Subsequent cloning from snapshot (TraceID: fff) is also an independent Trace. The three Traces
(creation, snapshot, clone) are correlated via sandbox UID / checkpoint ID as Span attributes,
supporting manual search.

#### Mixed Scenario Conflict Analysis

When multiple operations intersect, the annotation's traceparent may be overwritten. The core
safeguard: **At the beginning of each Reconcile, the trace context is extracted once from the
annotation and stored in ctx. All subsequent operations use that ctx, even if the annotation is
overwritten mid-Reconcile, the current Reconcile is unaffected.**

| Mixed Scenario | Conflict? | Reason |
|----------------|-----------|--------|
| Create then immediately pause | No | sandbox-manager waits synchronously; user can only send pause request after receiving sandbox ID, at which point the creation Trace has ended |
| Upgrade + standalone snapshot simultaneously | No | Two Checkpoint CRs are different resources, each carrying its own traceparent; controller uses the traceparent from the current Reconcile ctx when creating Checkpoint CR |
| Resume + delete simultaneously | No | Last annotation writer wins, but controller executes the operation matching the latest spec, traceparent matches the operation |
| Clone + original sandbox deletion | No | Operating on different Sandbox CRs, annotations are independent |
| Consecutive kubectl operations | No | Degradation scenario; when annotation doesn't exist, controller starts a new root Span |

If kubectl is used to rapidly modify the Sandbox spec in succession (e.g., first patch to Paused,
then patch to Resumed), the annotation is overwritten by the last operation. The controller's next
Reconcile uses the latest traceparent, which matches the latest user intent — this is correct
behavior, not a conflict.

## User Stories

### Story 1: Diagnosing Slow Sandbox Creation

As an SRE, search by sandbox ID in the trace backend and see the complete trace tree:
- Root Span 30s = end-to-end latency
- Total Reconcile ~280ms = controller processing time
- Gaps between Spans ~29.7s = Pod startup time → bottleneck is Pod startup, not controller

### Story 2: Debugging Pause/Resume Latency

As a developer, trace pause/resume cycles and compare Checkpoint 5s + DeletePod 5.5s vs PatchPod 2s
+ Initialize 3s to identify optimization directions.

### Story 3: Reuse Flow Observability

As an SRE, verify reuse is working: DeleteSandbox (reuse=true) trace shows only 200ms (PatchPod
40ms), compared to 8s without reuse (DeletePod 5.5s), quantifying the value of reuse.

## Implementation Details and Constraints

1. **OTel SDK initialization order**: TracerProvider must be initialized **before** any controller
   or HTTP server starts. In `main()`, after feature gate parsing, before `ctrl.NewManager()` /
   `sandboxController.Init()`.

2. **Shutdown**: The returned shutdown function is called during graceful shutdown to flush
   pending Spans.

3. **go.mod dependencies**: Promote the following indirect to direct:
   `go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/sdk`, `go.opentelemetry.io/otel/trace`,
   `go.opentelemetry.io/otel/exporters/otlp/otlptrace`,
   `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc`,
   `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`

4. **sandbox-manager must not import `pkg/features`**: Use CLI flags and environment variables.

5. **Annotation injection**: Inject when constructing/updating Sandbox objects in the `sandboxcr`
   infra layer.

6. **Annotation extraction**: Extract after the controller Reconcile fetches the Sandbox, before
   `calculateStatus()`.

7. **Check before creating Span**: The controller must check whether work is needed before calling
   `StartReconcileSpan()`.

8. **Child Spans only for meaningful IO**: CreatePod, DeletePod, PatchPod, Checkpoint, UpdateStatus.

9. **Performance**: OTel SDK batch span processor exports asynchronously, ~1μs per Span creation.

10. **Test isolation**: Use `NoopTracerProvider` or in-memory exporter.

## Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| OTel Collector unavailable | Traces cannot be exported | Batch processor drops Spans after timeout; log warning if unreachable at startup |
| Annotation size limit | K8s annotation 256KB limit | `traceparent` is only ~55 bytes |
| Reconcile latency increase | Span creation overhead | ~1μs/Span, async batch export |
| Stale annotation residue | Unrelated Reconcile generates noise Spans | "Check before creating Span" principle |
| `pkg/features` import violation | sandbox-manager cannot use feature gate | Use CLI flags / environment variables |
| Multi-operation intersection | Upgrade+checkpoint trace confusion | Each operation writes its own traceparent, Checkpoint CR carries trace-context |

## Alternative Approaches

1. **Use CR ID as TraceID**: Bind the entire Sandbox lifecycle to a single trace.
   - Rejected: Cannot distinguish latency of different operations; "creation slow" and "resume slow"
     cannot be analyzed in the same trace

2. **Use Pod UID as TraceID**: Derive TraceID from Pod UID.
   - Rejected: Pod UID changes after pause/resume (Pod is deleted and recreated), and cannot trace
     operations that don't involve Pods

3. **Use Jaeger client directly**: `github.com/uber/jaeger-client-go`.
   - Rejected: Jaeger client is in maintenance mode; OTel SDK is the successor

4. **Correlate by Trace ID only (no annotation propagation)**: Both components have independent
   traces, sharing a TraceID.
   - Rejected: Loses parent-child Span relationship, cannot perform cross-component latency analysis

5. **OTel auto-instrumentation (eBPF)**: `go.opentelemetry.io/auto`.
   - Deferred: Not yet production-ready for controller-runtime reconcile loops

## Upgrade Strategy

1. **No breaking changes**: New tracing is incrementally added, existing functionality unaffected
2. **Opt-in**: `SandboxTracingGate` defaults to false; requires feature gate + CLI flags + Collector
   deployment
3. **Configuration**: Deploy OTel Collector and configure `--tracing-endpoint`, no CRD schema
   changes needed
4. **Rollback**: Disable `SandboxTracingGate` to turn off all tracing

## Implementation History

- [x] 06/25/2026: Proposed design (original Chinese document)
- [x] 06/25/2026: Defined trace boundaries (independent Trace per operation)
- [x] 06/25/2026: Defined multi-Reconcile Span modeling (sibling Spans, check-before-create)
- [x] 06/25/2026: Completed end-to-end trace examples
- [x] 07/02/2026: Analyzed sandbox-manager and sandbox-controller code structure
- [x] 07/02/2026: Determined HTTP middleware approach (otelhttp wrapping mux, zero intrusion)
- [x] 07/02/2026: Mapped annotation injection points to actual code entry points
- [x] 07/02/2026: Determined visualization backend (OTLP gRPC, backend-agnostic)
- [x] 07/02/2026: Designed cross-operation context propagation (Checkpoint CR annotation)
- [ ] TBD: Implementation
- [ ] TBD: Integrate into sandbox-manager HTTP server
- [ ] TBD: Integrate into sandbox-controller Reconcile loop
- [ ] TBD: Add annotation injection/extraction
- [ ] TBD: Add CLI flags and deployment manifests
- [ ] TBD: Unit tests
- [ ] TBD: E2E validation (Jaeger/Tempo backend)
