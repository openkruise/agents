# Sandbox OTel Tracing Implementation Design

## Background

This document provides implementation details for the [OpenTelemetry-based Distributed Tracing
Proposal](../proposals/20260702-sandbox-otel-distributed-tracing-en.md), covering precise code
locations, function signatures, and integration steps.

## Goals

- Define the `pkg/tracing/` package structure and precise function signatures
- Specify exact code locations for Span creation in sandbox-manager and sandbox-controller
- Define the concrete implementation of annotation injection/extraction mechanism
- Specify CLI flag registration and initialization order
- Define feature gate integration approach
- Provide test plan and acceptance criteria

## Non-Goals

- Does not implement the OTel SDK itself (uses upstream `go.opentelemetry.io/otel`)
- Does not modify the Sandbox CRD schema (annotation is transparent)
- Does not instrument agent-runtime or sandbox-gateway in this phase
- Does not add custom sampling strategies

---

## Component 1: `pkg/tracing/provider.go`

### Purpose

Initialize the global OTel TracerProvider and OTLP gRPC exporter.

### Function Signatures

```go
package tracing

type Config struct {
    Enabled       bool
    Endpoint      string // OTLP gRPC endpoint, e.g., "otel-collector:4317"
    ServiceName   string // e.g., "sandbox-controller" or "sandbox-manager"
    SamplingRatio float64 // 0.0 to 1.0, default 1.0
    Insecure      bool    // Use insecure gRPC (dev environment)
}

// InitTracerProvider initializes the global TracerProvider and returns a shutdown function.
// Must be called once at startup, before any controller or HTTP server starts.
// If cfg.Enabled is false, returns a no-op shutdown function.
func InitTracerProvider(ctx context.Context, cfg Config) (func(context.Context) error, error)

// Tracer returns the global tracer for the specified instrumentation scope.
func Tracer(name string) trace.Tracer
```

### Implementation Notes

- Use `otlptracegrpc.NewClient` to create the OTLP gRPC exporter
- Use `sdktrace.NewTracerProvider` + `sdktrace.WithBatcher` for async batch export
- Resource attributes: `service.name`, `service.version`, `service.namespace`
- Sampler: `sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SamplingRatio))`
- When `cfg.Enabled` is false, set `otel.SetTracerProvider(trace.NewNoopTracerProvider())`

---

## Component 2: `pkg/tracing/propagator.go`

### Purpose

Inject/extract W3C Trace Context between `context.Context` and Sandbox CRD annotations.

### Function Signatures

```go
package tracing

const TraceContextAnnotationKey = "agents.kruise.io/trace-context"

// InjectTraceContext injects the current trace context from ctx into annotations.
// If annotations is nil, initializes a new map.
// If tracing is disabled or no active span exists, returns annotations unchanged.
func InjectTraceContext(ctx context.Context, annotations map[string]string) map[string]string

// ExtractTraceContext extracts trace context from annotations and returns a context
// carrying the extracted span context. If annotation doesn't exist or is invalid,
// returns ctx unchanged.
func ExtractTraceContext(ctx context.Context, annotations map[string]string) context.Context
```

### Implementation Notes

- Use `otel.GetTextMapPropagator()` with a custom `carrier` type
- Propagator is `trace.TraceContext` (W3C Trace Context format)
- Carrier implements the `propagation.TextMapCarrier` interface, wrapping `map[string]string`

### Carrier Implementation

```go
type annotationCarrier struct {
    annotations map[string]string
}

func (c *annotationCarrier) Get(key string) string { return c.annotations[key] }
func (c *annotationCarrier) Set(key, value string) { c.annotations[key] = value }
func (c *annotationCarrier) Keys() []string { /* return all keys */ }
```

---

## Component 3: `pkg/tracing/middleware.go`

### Purpose

sandbox-manager HTTP middleware that creates a root Span for each request. Uses
`otelhttp.NewHandler` to wrap the entire mux with zero intrusion.

### Function Signatures

```go
// HTTPMiddleware wraps http.Handler with otelhttp, starting a root Span for each HTTP request.
// Span name format: "{HTTP_METHOD} {HTTP_PATH}" (e.g., "POST /sandboxes").
// Span attributes: http.method, http.url, http.status_code, etc.
// Span context is automatically injected into the request context for downstream use.
func HTTPMiddleware(handler http.Handler, serviceName string) http.Handler
```

### Integration Point

Wrap the mux before starting the HTTP server:

```go
func (sc *Controller) Run() error {
    sc.registerRoutes()
    handler := tracing.HTTPMiddleware(sc.mux, "sandbox-manager")
    return http.ListenAndServe(":3000", handler)
}
```

### Span Naming

otelhttp defaults to `{HTTP_METHOD} {HTTP_PATH}` format (e.g., `POST /sandboxes`).

| HTTP Route | Span Name |
|------------|----------|
| `POST /sandboxes` | `POST /sandboxes` |
| `DELETE /sandboxes/{id}` | `DELETE /sandboxes/{id}` |
| `POST /sandboxes/{id}/pause` | `POST /sandboxes/{id}/pause` |
| `POST /sandboxes/{id}/resume` | `POST /sandboxes/{id}/resume` |
| `POST /sandboxes/{id}/connect` | `POST /sandboxes/{id}/connect` |
| `POST /sandboxes/{id}/snapshots` | `POST /sandboxes/{id}/snapshots` |
| `GET /sandboxes/{id}` | `GET /sandboxes/{id}` |
| `GET /v2/sandboxes` | `GET /v2/sandboxes` |

### Future Optimization

If semantic Span naming is needed (e.g., `sandbox-manager.CreateSandbox`), a `web.MiddleWare`
middleware approach can be adopted in a later version to precisely map routes to named Spans.

---

## Component 4: `pkg/tracing/reconcile.go`

### Purpose

Create Spans for controller-runtime Reconcile iterations.

### Function Signatures

```go
// StartReconcileSpan creates a Span for a controller-runtime reconcile iteration.
// Extracts trace context from the Sandbox object's annotation to establish parent-child
// relationship with the sandbox-manager root Span. Multiple Reconcile passes produce sibling Spans.
// Note: The caller should check whether work is needed before calling this function.
func StartReconcileSpan(ctx context.Context, obj client.Object, controllerName string) (context.Context, trace.Span)

// StartChildSpan creates a child Span for a specific IO operation within Reconcile.
func StartChildSpan(ctx context.Context, spanName string, attrs ...trace.SpanOption) (context.Context, trace.Span)
```

### Integration Point

In `pkg/controller/sandbox/sandbox_controller.go`'s `Reconcile` method:

```go
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (crl ctrl.Result, err error) {
    // ... existing early return paths (Sandbox not found, expectation, terminal state, etc.) ...

    box, err = r.addSandboxFinalizerAndHash(ctx, box)
    if err != nil { return reconcile.Result{}, err }

    // --- Tracing: create Reconcile Span ---
    reconcileCtx, reconcileSpan := tracing.StartReconcileSpan(ctx, box, "sandbox-controller")
    defer reconcileSpan.End()

    // calculate sandbox status
    var shouldRequeue bool
    newStatus, shouldRequeue = calculateStatus(args)
    // ...
}
```

**Key constraint**: Span is created **after** all "no work needed" early return paths:
- Sandbox not found
- Expectation not satisfied
- Terminal state (Failed/Succeeded)
- Empty template (after termination handling)

Span coverage: `calculateStatus` → phase dispatch → `updateSandboxStatus`.

---

## Component 5: Annotation Injection Points

### sandbox-manager Injection Points

| Operation | File | Function | Injection Method |
|-----------|------|----------|-----------------|
| Create (Claim) | `infra/sandboxcr/claim.go` | `TryClaimSandbox` (when creating/updating Sandbox CR) | Inject annotation before writing CR |
| Create (Clone) | `infra/sandboxcr/clone.go` | `CloneSandbox` (when creating Sandbox CR) | Same as above |
| Pause | `infra/sandboxcr/sandbox.go` | `Sandbox.Pause` → `retryUpdate` (in modifier function) | Inject in update modifier |
| Resume | `infra/sandboxcr/sandbox.go` | `Sandbox.Resume` → `retryUpdate` (in modifier function) | Same as above |
| Delete | `infra/sandboxcr/sandbox.go` | `Sandbox.Kill` → `deleteSandbox` or `retryUpdate` | Inject before delete/update |

### Implementation Pattern

Each injection point uniformly calls:

```go
sandbox.Annotations = tracing.InjectTraceContext(ctx, sandbox.Annotations)
```

For `retryUpdate` operations (Pause/Resume):

```go
func (s *Sandbox) Pause(ctx context.Context, opts infra.PauseOptions) error {
    err := retryUpdate(ctx, s, func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
        sbx.Spec.Paused = true
        // Inject trace context
        sbx.Annotations = tracing.InjectTraceContext(ctx, sbx.Annotations)
        return true, nil
    })
    // ...
}
```

---

## Component 6: Controller Child Spans

### Child Span Locations in `common_control.go`

#### CreatePod

```go
func (r *commonControl) createPod(ctx context.Context, box *agentsv1alpha1.Sandbox, ...) (*corev1.Pod, error) {
    // ... generate Pod ...

    ctx, span := tracing.StartChildSpan(ctx, tracing.SpanCreatePod,
        trace.WithAttributes(attribute.String(tracing.AttrPodName, pod.Name)))
    defer span.End()

    err = r.Create(ctx, pod)
    // ...
}
```

#### DeletePod

In `EnsureSandboxPaused` and `EnsureSandboxTerminated`:

```go
ctx, span := tracing.StartChildSpan(ctx, tracing.SpanDeletePod,
    trace.WithAttributes(attribute.String(tracing.AttrPodName, pod.Name)))
defer span.End()
err = r.Delete(ctx, pod, &client.DeleteOptions{...})
```

#### updateSandboxStatus

In `sandbox_controller.go`:

```go
func (r *SandboxReconciler) updateSandboxStatus(ctx context.Context, ...) error {
    if reflect.DeepEqual(box.Status, newStatus) { return nil }

    ctx, span := tracing.StartChildSpan(ctx, tracing.SpanUpdateStatus,
        trace.WithAttributes(
            attribute.String(tracing.AttrPhaseBefore, string(box.Status.Phase)),
            attribute.String(tracing.AttrPhaseAfter, string(newStatus.Phase)),
        ))
    defer span.End()
    // ... status patch ...
}
```

### Child Span Granularity Rules

| Operation | Create Child Span? | Reason |
|-----------|-------------------|--------|
| `r.Create(ctx, pod)` | Yes | Heavy IO (100-500ms) |
| `r.Delete(ctx, pod)` | Yes | Pod graceful termination (100ms-6s) |
| `r.Status().Patch(...)` | Yes | Write operation (10-50ms) |
| `r.Get(ctx, key, pod)` | No | Lightweight read (5-10ms) |
| Phase dispatch logic | No | No IO (<1ms) |
| `r.Patch(ctx, pod, ...)` | Yes | Write operation |

---

## Component 7: Initialization Sequence

### sandbox-controller (`cmd/agent-sandbox-controller/main.go`)

```go
func main() {
    // ... existing flag parsing ...

    // After feature gate parsing, before ctrl.NewManager:
    if utilfeature.DefaultFeatureGate.Enabled(features.SandboxTracingGate) {
        tracingShutdown, err := tracing.InitTracerProvider(ctx, tracing.Config{
            Enabled:       true,
            Endpoint:      tracingEndpoint,
            ServiceName:   "sandbox-controller",
            SamplingRatio: tracingSamplingRatio,
            Insecure:      tracingInsecure,
        })
        if err != nil { setupLog.Error(err, "..."); os.Exit(1) }
        defer func() { _ = tracingShutdown(context.Background()) }()
    }

    // ... ctrl.NewManager, controller setup, mgr.Start ...
}
```

New CLI flags:

```go
flag.StringVar(&tracingEndpoint, "tracing-endpoint", "", "OTLP gRPC export endpoint")
flag.Float64Var(&tracingSamplingRatio, "tracing-sampling-ratio", 1.0, "Trace sampling ratio")
flag.BoolVar(&tracingInsecure, "tracing-insecure", true, "Use insecure gRPC export")
```

### sandbox-manager (`cmd/sandbox-manager/main.go`)

sandbox-manager uses CLI flags, not feature gates (per AGENTS.md constraint):

```go
func main() {
    var tracingEnabled bool
    var tracingEndpoint string
    var tracingSamplingRatio float64
    var tracingInsecure bool

    pflag.BoolVar(&tracingEnabled, "tracing-enabled", false, "Enable OpenTelemetry distributed tracing")
    pflag.StringVar(&tracingEndpoint, "tracing-endpoint", "", "OTLP gRPC export endpoint")
    pflag.Float64Var(&tracingSamplingRatio, "tracing-sampling-ratio", 1.0, "Trace sampling ratio")
    pflag.BoolVar(&tracingInsecure, "tracing-insecure", true, "Use insecure gRPC export")

    // ... pflag.Parse() ...

    if tracingEnabled {
        tracingShutdown, err := tracing.InitTracerProvider(ctx, tracing.Config{
            Enabled:       true,
            Endpoint:      tracingEndpoint,
            ServiceName:   "sandbox-manager",
            SamplingRatio: tracingSamplingRatio,
            Insecure:      tracingInsecure,
        })
        if err != nil { klog.Fatalf("...") }
        defer func() { _ = tracingShutdown(context.Background()) }()
    }

    // ... sandboxController.Init() ...
}
```

### Tracing Middleware Registration

Conditionally wrap the mux before starting the HTTP server:

```go
func (sc *Controller) Run() error {
    sc.registerRoutes()
    handler := sc.mux
    if /* tracing enabled */ {
        handler = tracing.HTTPMiddleware(handler, "sandbox-manager")
    }
    return http.ListenAndServe(":3000", handler)
}
```

---

## Component 8: Feature Gate

In `pkg/features/features.go`:

```go
const (
    // SandboxTracingGate enables OpenTelemetry distributed tracing for sandbox lifecycle.
    // Only affects sandbox-controller. sandbox-manager uses CLI flags.
    SandboxTracingGate featuregate.Feature = "SandboxTracing"
)

var defaultFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
    // ... existing gates ...
    SandboxTracingGate: {Default: false, PreRelease: featuregate.Alpha},
}
```

---

## Component 9: go.mod Dependencies

Promote from indirect to direct:

```plaintext
go.opentelemetry.io/otel
go.opentelemetry.io/otel/sdk
go.opentelemetry.io/otel/trace
go.opentelemetry.io/otel/exporters/otlp/otlptrace
go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc
go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp
```

After adding, run:

```bash
go mod tidy
go mod vendor
```

---

## Component 10: Cross-Operation Context Propagation

### Checkpoint CR Annotation Propagation

When sandbox-controller creates a Checkpoint CR during pause/upgrade, it propagates the current
trace context:

```go
checkpoint := &agentsv1alpha1.Checkpoint{
    ObjectMeta: metav1.ObjectMeta{
        Name:      checkpointName,
        Namespace: box.Namespace,
        Annotations: map[string]string{},
    },
    // ...
}
// Propagate current Reconcile span's trace context to Checkpoint CR
checkpoint.Annotations = tracing.InjectTraceContext(ctx, checkpoint.Annotations)
```

### Multi-Operation Intersection Rules

1. Each operation writes its own `traceparent` to the Sandbox annotation (overwrites the previous one)
2. The controller always uses the **current** annotation's traceparent
3. If checkpoint is a sub-step of the current operation (e.g., during upgrade), the Checkpoint CR
   carries the upgrade's traceparent
4. If checkpoint is a standalone operation (e.g., user-initiated snapshot), the Sandbox annotation
   has the snapshot operation's traceparent

---

## Test Plan

### Unit Tests

| Test File | Coverage Target |
|-----------|----------------|
| `provider_test.go` | InitTracerProvider enabled/disabled config; shutdown flush |
| `propagator_test.go` | InjectTraceContext with/without active span; ExtractTraceContext with valid/missing/invalid annotation |
| `middleware_test.go` | Root Span creation; Span attributes (method, path, sandbox ID); Span ended on response |
| `reconcile_test.go` | StartReconcileSpan with/without annotation; sibling Span verification; StartChildSpan attributes |

### Test Strategy

- Use `trace.NewNoopTracerProvider()` for tests that don't need to verify span export
- Use in-memory exporter (`tracetest.NewInMemoryExporter()`) to verify span attributes and hierarchy
- Table-driven tests with descriptive `name` fields (per AGENTS.md)
- Use `expectError string` pattern (per AGENTS.md testing conventions)

### Acceptance Criteria

1. **Feature gate disabled**: No span export, no annotation injection, zero overhead
2. **Feature gate enabled, no Collector**: Spans dropped after batch timeout, no functional impact
3. **Create sandbox trace**: Root Span (sandbox-manager) + sibling Spans (controller Reconcile) +
   child Spans (CreatePod), sharing the same TraceID
4. **Pause/Resume/Delete trace**: Each operation produces an independent trace with correct
   parent-child relationships
5. **kubectl-created sandbox**: Controller starts a new root Span, searchable by sandbox UID
6. **Multi-Reconcile**: Only Reconcile passes with actual work create Spans; terminal-state Reconcile
   does not create Spans

---

## Implementation Phases

### Phase 1: Core Package

1. Create `pkg/tracing/` package (`provider.go`, `propagator.go`, `spans.go`, `doc.go`)
2. Add go.mod dependencies and `go mod vendor`
3. Write provider and propagator unit tests

### Phase 2: sandbox-manager Integration

1. Add `middleware.go`
2. Add tracing middleware in `pkg/servers/e2b/routes.go`
3. Add annotation injection in `pkg/sandbox-manager/infra/sandboxcr/` (claim.go, clone.go, sandbox.go)
4. Add CLI flags in `cmd/sandbox-manager/main.go`
5. Write unit tests

### Phase 3: sandbox-controller Integration

1. Add `reconcile.go` (`StartReconcileSpan`, `StartChildSpan`)
2. Add Reconcile Span in `pkg/controller/sandbox/sandbox_controller.go`
3. Add child Spans in `pkg/controller/sandbox/core/common_control.go`
4. Add feature gate in `pkg/features/features.go`
5. Add CLI flags in `cmd/agent-sandbox-controller/main.go`
6. Write unit tests

### Phase 4: Deployment and Documentation

1. Add deployment manifest patches (tracing CLI flags)
2. Add OTel Collector deployment example
3. Verify end-to-end with Jaeger or Tempo backend
4. Update documentation
