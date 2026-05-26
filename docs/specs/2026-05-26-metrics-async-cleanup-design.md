# Async Metric Cleanup Pool — Design

- Date: 2026-05-26
- Branch: `features/observability-perf-refine-20260525`
- Scope: `pkg/utils/metricsasync/` (new), `pkg/controller/sandbox/`,
  `cmd/agent-sandbox-controller/`

## 1. Problem

`pkg/controller/sandbox/sandbox_controller.go` couples Prometheus metric
maintenance directly into the Sandbox `Reconcile` hot path:

- Line 120: on a `NotFound` Sandbox the reconciler calls
  `deleteSandboxMetrics(namespace, name)` synchronously.
- Line 125 and line 319: every successful reconcile and every status update
  calls `recordSandboxMetrics(box)` synchronously.

`deleteSandboxMetrics` (`pkg/controller/sandbox/metrics.go:544`) performs:

- 12 `DeleteLabelValues` calls across single-series GaugeVecs.
- A 7-iteration loop over `allPhases` for `sandboxStatusPhase`.
- **Three `DeletePartialMatch` calls** on `sandboxInfo`,
  `sandboxStatusAbnormal`, and `sandboxLabels`. `DeletePartialMatch` walks
  the entire vector under the `MetricVec` write lock, performing label-by-
  label comparison.

At the target operating point (~10⁵ create/delete QPS with a comparable
number of live Sandboxes), three observations dominate:

1. Each `DeletePartialMatch` becomes O(N) under a write lock with N ≈ 10⁵.
   Per call, the lock can be held for several milliseconds; at delete QPS
   on the order of 10⁵/s the affected vectors are effectively serialized
   on a single global mutex.
2. `recordSandboxMetrics` runs on every reconcile and touches ~20
   `WithLabelValues` and ~7 `DeleteLabelValues` (stale-phase pruning) on
   shared GaugeVecs. Even though each individual op is sub-microsecond,
   the aggregate adds steady RWMutex contention across all controller
   workers.
3. The two effects compound: while a delete worker is holding the
   `sandboxInfo` write lock for a partial-match scan, every other worker
   that calls `recordSandboxMetrics` for an unrelated Sandbox blocks on
   the same vector.

The net effect is that Sandbox `Reconcile` tail latency grows superlinearly
with live Sandbox count once the delete rate is non-trivial.

## 2. Goal & Non-goals

### Goal

Remove metric cleanup from the Sandbox `Reconcile` synchronous path. The
reconciler must only enqueue a small, allocation-bounded record describing
which series to delete; a separate, configurable goroutine pool drains
that record and performs the expensive `Delete*` work off the reconcile
goroutine.

The pool must be reusable by future controllers (sandboxset, sandboxclaim)
and must integrate with controller-runtime `manager.Runnable` lifecycle so
that `mgr.Start` / `mgr.Stop` semantics are preserved.

### Non-goals

- Do **not** asynchronize `recordSandboxMetrics`. It mutates gauges that
  must reflect the current Sandbox state immediately after status updates;
  reordering across goroutines would let stale values win. Only the
  delete path is moved off the reconcile goroutine in this iteration.
- Do not change which metric series exist, their names, labels, or
  semantics. Cleanup latency is the only observable difference.
- Do not introduce a generic goroutine pool library. The pool is
  purpose-built for metric cleanup; tasks are short, idempotent,
  return no value, and need only same-key serialization.
- Do not modify SandboxSet, SandboxClaim, or Checkpoint controllers in
  this iteration. The pool is shared by construction; their migration is
  a follow-up.
- Do not retain a persistent queue. On controller restart, residual
  series are tolerated — the next `WithLabelValues` for a recreated
  Sandbox of the same name will overwrite them.

## 3. Constraints

- The async pool must add at most O(1) work to `Reconcile`: one map
  insert under a workqueue mutex, no metric-vec lock acquisition.
- Same `(kind, namespace, name)` enqueued twice while still queued must
  collapse to one task. The cleanup function is idempotent, but doing
  the partial-match scan twice is the exact cost we are avoiding.
- Same-key tasks must not run concurrently across workers. Two
  goroutines racing on `Delete*` for the same series could lose a
  delete-then-recreate ordering when a Sandbox is recreated under the
  same name.
- Pool size must be configurable via controller flag and environment
  variable; runtime resizing is out of scope.
- Manager shutdown must drain the queue best-effort under a bounded
  timeout, not block forever and not drop work without an observable
  counter.

## 4. Approach

### 4.1 New package `pkg/utils/metricsasync`

A small package exposing a single `Pool` type plus a `CleanupFunc`
contract. Backed by `client-go/util/workqueue.NewTyped[Key]()` where
`Key = struct{ Kind, Namespace, Name string }`. The workqueue
provides:

- Built-in deduplication on `comparable` items.
- Same-key serialization (`Get`/`Done` ensures one in-flight item per key
  across workers).
- A clean `ShutDown` signal that wakes blocked workers.
- A `MetricsProvider` hook compatible with controller-runtime's
  prometheus registry, so queue depth/latency are exposed without extra
  code.

### 4.2 Public API

```go
// CleanupFunc removes all metric series for a single object identified
// by namespace/name. It MUST be idempotent and panic-safe; the pool will
// recover panics but cannot retry semantically.
type CleanupFunc func(namespace, name string)

// Key is the workqueue payload. Comparable; identical Keys collapse via
// workqueue dedup. EnqueueAt is tracked separately (see 4.3) so that
// re-enqueuing the same Key only updates the most-recent timestamp
// rather than producing a distinct queue entry.
type Key struct {
    Kind      string // matches a registered kind
    Namespace string
    Name      string
}

// Enqueuer is the narrow interface reconcilers depend on. The Pool
// satisfies it; tests inject a fake.
type Enqueuer interface {
    Enqueue(kind, namespace, name string)
}

type Options struct {
    Workers      int           // default 8, min 1
    DrainTimeout time.Duration // default 5s; <=0 means do not block on stop
    QueueCap     int           // 0 = unbounded; >0 = drop+counter on overflow
    Name         string        // metrics subsystem prefix, default "metrics_async"
}

type Pool struct { /* unexported */ }

func NewPool(opts Options) *Pool
func (p *Pool) RegisterKind(kind string, fn CleanupFunc) error // before Start
func (p *Pool) Enqueue(kind, namespace, name string)           // O(1), safe pre/post Start
func (p *Pool) Start(ctx context.Context) error                // implements manager.Runnable
```

`Pool` is created once in `cmd/agent-sandbox-controller/main.go`, registered
on the controller-runtime manager via `mgr.Add(pool)`, and injected into
each reconciler through its constructor. There is exactly one `Pool`
instance per controller process.

### 4.3 Enqueue semantics

`Enqueue(kind, ns, name)`:

1. If the kind is not registered, log once at warn level (deduped via
   sync.Once-per-kind) and increment `metrics_async_dropped_total{kind,reason="unregistered"}`.
2. If `QueueCap > 0` and `queue.Len() >= cap`, increment
   `metrics_async_dropped_total{kind,reason="queue_full"}` and return.
3. Build `key := Key{Kind, Namespace, Name}`, store
   `time.Now().UnixNano()` into a `sync.Map[Key]int64` (the
   "enqueue-at map"), then call `queue.Add(key)`.

Identity is `Key` (no timestamp), so re-enqueuing the same triple while
a previous task is still queued collapses to a single entry. The
enqueue-at map always reflects the most recent enqueue time and is read
once by the worker (then `LoadAndDelete`) for the latency histogram.

### 4.4 Worker loop

Each worker runs:

```go
for {
    key, shutdown := queue.Get()
    if shutdown { return }
    enqueueAt, _ := enqueueAtMap.LoadAndDelete(key)
    func() {
        defer func() {
            if r := recover(); r != nil {
                processedTotal.WithLabelValues(key.Kind, "panic").Inc()
                klog.ErrorS(fmt.Errorf("%v", r), "metrics async cleanup panicked", "kind", key.Kind, "ns", key.Namespace, "name", key.Name)
            }
        }()
        fn := registry[key.Kind]
        start := time.Now()
        fn(key.Namespace, key.Name)
        durationSec := time.Since(start).Seconds()
        durationHistogram.WithLabelValues(key.Kind).Observe(durationSec)
        if enqueueAt != 0 {
            latencySec := float64(time.Now().UnixNano()-enqueueAt) / 1e9
            latencyHistogram.WithLabelValues(key.Kind).Observe(latencySec)
        }
        processedTotal.WithLabelValues(key.Kind, "ok").Inc()
    }()
    queue.Done(key)
}
```

`registry` is set during `RegisterKind` and is read-only after `Start`,
so no lock is needed in the hot path.

### 4.5 Lifecycle

- `Start(ctx) error` starts `Workers` goroutines, blocks on `<-ctx.Done()`,
  then enters drain:
  1. `queue.ShutDown()` (workqueue contract: workers exit once queue
     drains or `ShutDownWithDrain` returns).
  2. Start a timer with `DrainTimeout`. Wait for all workers via
     `sync.WaitGroup`. If the timer fires first, return early; the
     remaining queued items are reported via
     `metrics_async_dropped_total{reason="drain_timeout"}` using
     `queue.Len()` as the count.
- `Start` is idempotent against double-call by a sync.Once internal flag
  but, by contract, called exactly once by the manager.

### 4.6 Self-observability

Registered against `sigs.k8s.io/controller-runtime/pkg/metrics.Registry`:

- `metrics_async_queue_depth{kind}` — gauge, sampled every 5s by a
  background goroutine using `queue.Len()` (the per-kind split is
  approximate; we maintain a per-kind counter incremented on Enqueue and
  decremented on Done).
- `metrics_async_processed_total{kind,result}` — counter,
  result ∈ {ok, panic}.
- `metrics_async_duration_seconds{kind}` — histogram, exponential
  buckets 1ms → ~4s.
- `metrics_async_latency_seconds{kind}` — histogram, time from Enqueue
  to processing start, exponential buckets 1ms → ~30s. This is the
  primary SLI for the pool.
- `metrics_async_dropped_total{kind,reason}` — counter, reason ∈
  {unregistered, queue_full, drain_timeout}.

### 4.7 Sandbox controller wiring

Changes in `pkg/controller/sandbox/`:

- `metrics.go`: rename `deleteSandboxMetrics` → exported
  `DeleteSandboxMetrics(namespace, name string)`. Body unchanged.
- `sandbox_controller.go`:
  - `SandboxReconciler` gains an unexported field
    `metricsCleanup metricsasync.Enqueuer` (the narrow interface
    defined in 4.2 so tests can fake it).
  - The `Add` constructor takes the pool through
    `SandboxReconcilerOptions` (or a dedicated parameter; keeping
    backward compatibility with the existing `Add` signature is
    straightforward).
  - Line 120 changes from
    `deleteSandboxMetrics(req.NamespacedName.Namespace, req.NamespacedName.Name)`
    to
    `r.metricsCleanup.Enqueue("sandbox", req.NamespacedName.Namespace, req.NamespacedName.Name)`.
  - Lines 125 and 319 (`recordSandboxMetrics`) are unchanged.

Changes in `cmd/agent-sandbox-controller/main.go`:

- Add three flags (with env fallback):
  - `--metrics-async-workers` (env `METRICS_ASYNC_WORKERS`), default 8.
  - `--metrics-async-drain-timeout` (env `METRICS_ASYNC_DRAIN_TIMEOUT`),
    default `5s`.
  - `--metrics-async-queue-cap` (env `METRICS_ASYNC_QUEUE_CAP`),
    default 0 (unbounded).
- Build the pool via `metricsasync.NewPool(...)`.
- Call `pool.RegisterKind("sandbox", sandbox.DeleteSandboxMetrics)`.
- `mgr.Add(pool)` so its `Start(ctx)` is driven by the manager.
- Pass `pool` into the Sandbox reconciler `Add` invocation.

### 4.8 Backwards compatibility

- The exported metric names and labels do not change. Existing
  dashboards, alerts, and recording rules continue to work.
- Behavior under low QPS is observably equivalent to the synchronous
  path with sub-millisecond extra latency from the queue hop.
- On controller restart, residual series for already-deleted Sandboxes
  may persist for the lifetime of the new process. The same is already
  true today during shutdown; the drain timeout makes the worst case
  bounded rather than unbounded.

## 5. Performance expectations

- Reconcile path metric work drops from ~3 partial-match scans + ~20
  WithLabelValues to one workqueue `Add` (a hashed map insert under a
  short mutex).
- Pool can serialize the partial-match scans across `Workers` goroutines
  without competing with controller workers, and dedup absorbs bursts
  where the same Sandbox is reconciled twice in quick succession during
  finalizer removal.
- At 10⁵ delete QPS with N=10⁵ live Sandboxes, the pool itself becomes
  the bottleneck for cleanup throughput, which is the design intent —
  Reconcile tail latency is no longer a function of N.

## 6. Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| Pool saturates and queue grows unbounded under sustained 10⁵ QPS bursts | Operators can set `--metrics-async-queue-cap`; overflow is observable via `metrics_async_dropped_total`. Saturation also visible via `metrics_async_latency_seconds`. |
| Cleanup happens after a same-name Sandbox is recreated, deleting fresh series | Same-key serialization in workqueue prevents this within a single enqueue→process cycle. Recreation enqueues a new `recordSandboxMetrics` (synchronous) which always overwrites series; if the stale delete arrives later, the next reconcile of the recreated Sandbox restores them. The window is bounded by `metrics_async_latency_seconds`, and the controller’s `recordSandboxMetrics` runs on every reconcile, so steady-state recovery is automatic. |
| CleanupFunc panics | `defer recover()` in worker; counted as `result="panic"`; worker survives. |
| Manager shutdown leaves pending tasks | Best-effort drain with `DrainTimeout`; remaining count reported via `dropped_total{reason="drain_timeout"}`; residual series overwritten on next process start when the same name reappears. |

## 7. Test plan

- `pkg/utils/metricsasync/pool_test.go`, table-driven:
  - Enqueue dedup: enqueue same `(kind, ns, name)` 1000× while a worker
    is parked; only one CleanupFunc invocation is observed.
  - Concurrent enqueue from N goroutines spread across M kinds: total
    invocations equals total distinct keys.
  - Same-key serialization: a CleanupFunc that asserts `atomic.CompareAndSwap`
    on a per-key flag never observes overlap.
  - Unregistered kind: enqueue is a no-op except for
    `dropped_total{reason="unregistered"}`.
  - `QueueCap` overflow: enqueue beyond cap, verify `dropped_total{reason="queue_full"}`.
  - Panic in CleanupFunc: worker survives, `processed_total{result="panic"}`
    increments, subsequent enqueues still drain.
  - Shutdown drain: enqueue X, cancel context, all X complete within
    `DrainTimeout`.
  - Shutdown timeout: CleanupFunc blocks on a channel, cancel context,
    verify `Start` returns by `DrainTimeout` and
    `dropped_total{reason="drain_timeout"}` matches remaining count.

- `pkg/controller/sandbox/sandbox_controller_test.go`:
  - Existing tests continue to pass with a fake `Enqueuer` that records
    invocations.
  - New `TestReconcileEnqueuesAsyncCleanupOnNotFound`: feed a
    `NamespacedName` whose Sandbox does not exist, assert exactly one
    `Enqueue("sandbox", ns, name)` and zero direct calls to the real
    `DeleteSandboxMetrics`.
  - Existing `pkg/controller/sandbox/metrics_test.go` continues calling
    `DeleteSandboxMetrics` directly (now exported); no behavioral change.

- `cmd/agent-sandbox-controller`: smoke build only; flag parsing covered
  by existing pattern.

## 8. Rollout

Single-PR rollout. The async path is unconditional (no feature gate,
per requirements):

- Default config (8 workers, 5s drain, unbounded queue) preserves
  behavior in dev clusters where N is small.
- Operators running at scale tune workers/cap from the deployment.
- Rollback strategy: revert the PR. No persisted state, no API changes.

## 9. Out of scope / follow-ups

- Migrate `pkg/controller/sandboxset/` and `pkg/controller/sandboxclaim/`
  delete-paths onto the same pool via additional `RegisterKind` calls.
- Investigate making `recordSandboxMetrics` cheaper (e.g., skip when
  `box.Generation` and `box.Status.Phase` unchanged) — orthogonal to
  this design.
- Replace `DeletePartialMatch` callers in `recordSandboxMetrics`
  (line 438 phase pruning) with explicit per-phase `DeleteLabelValues`
  loops where possible — already the case in the current code; no
  action.
