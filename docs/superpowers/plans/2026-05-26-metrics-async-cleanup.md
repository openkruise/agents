# Metrics Async Cleanup Pool — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move Sandbox metric cleanup off the Reconcile hot path into a shared, configurable goroutine pool so that 10⁵ create/delete QPS no longer serializes Reconcile workers on Prometheus vector mutexes.

**Architecture:** A new package `pkg/utils/metricsasync` provides a `Pool` (a `controller-runtime` `manager.Runnable`) backed by `client-go/util/workqueue.NewTyped[Key]()`. Reconcilers call `pool.Enqueue(kind, ns, name)` (O(1)); registered worker goroutines drain the queue, deduplicate by `(kind, ns, name)`, and run a registered `CleanupFunc`. The Sandbox controller registers `DeleteSandboxMetrics` for kind `"sandbox"` and replaces its synchronous call site with `Enqueue`.

**Tech Stack:** Go, `client-go/util/workqueue`, `prometheus/client_golang`, `sigs.k8s.io/controller-runtime`, `k8s.io/klog/v2`.

**Spec:** `docs/specs/2026-05-26-metrics-async-cleanup-design.md`.

**Branch:** `features/observability-perf-refine-20260525` (current).

---

## File Structure

| File | Responsibility |
| --- | --- |
| `pkg/utils/metricsasync/pool.go` (new) | `Pool`, `Options`, `Key`, `Enqueuer`, `CleanupFunc`. Workqueue, registration, enqueue, worker loop, shutdown. |
| `pkg/utils/metricsasync/metrics.go` (new) | Self-observability prometheus collectors registered against `controller-runtime/pkg/metrics.Registry`. |
| `pkg/utils/metricsasync/pool_test.go` (new) | Table-driven tests for dedup, concurrency, panic recovery, drain, drain-timeout, queue cap, unregistered kind. |
| `pkg/controller/sandbox/metrics.go` (modify) | Rename `deleteSandboxMetrics` → exported `DeleteSandboxMetrics`. No behavior change. |
| `pkg/controller/sandbox/sandbox_controller.go` (modify) | Add `metricsCleanup metricsasync.Enqueuer` field; thread it through `Add`; replace line 120 sync call with `Enqueue`. |
| `pkg/controller/sandbox/metrics_test.go` (modify) | Update call sites to use new exported name. |
| `pkg/controller/sandbox/sandbox_controller_test.go` (modify) | Add `TestReconcileEnqueuesAsyncCleanupOnNotFound` using a fake enqueuer. |
| `pkg/controller/controllers.go` (modify) | Change `controllerAddFuncs` to take an `Adders` struct holding the pool, propagate to `sandbox.Add`. |
| `cmd/agent-sandbox-controller/main.go` (modify) | Add three flags + env fallbacks, build pool, register kind, `mgr.Add(pool)`, pass through `controller.SetupWithManager`. |

---

## Task 1: Scaffold `metricsasync` package with Options, Key, types

**Files:**
- Create: `pkg/utils/metricsasync/pool.go`
- Create: `pkg/utils/metricsasync/pool_test.go`

- [ ] **Step 1: Write the failing test for Options defaults**

```go
// pkg/utils/metricsasync/pool_test.go
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metricsasync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestOptions_applyDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   Options
		want Options
	}{
		{
			name: "zero values get defaults",
			in:   Options{},
			want: Options{
				Workers:      8,
				DrainTimeout: 5 * time.Second,
				QueueCap:     0,
				Name:         "metrics_async",
			},
		},
		{
			name: "explicit values preserved",
			in:   Options{Workers: 16, DrainTimeout: time.Second, QueueCap: 100, Name: "custom"},
			want: Options{Workers: 16, DrainTimeout: time.Second, QueueCap: 100, Name: "custom"},
		},
		{
			name: "negative workers clamped to 1",
			in:   Options{Workers: -3},
			want: Options{Workers: 1, DrainTimeout: 5 * time.Second, Name: "metrics_async"},
		},
		{
			name: "negative drain treated as zero (no wait)",
			in:   Options{Workers: 2, DrainTimeout: -1},
			want: Options{Workers: 2, DrainTimeout: 0, Name: "metrics_async"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.applyDefaults()
			assert.Equal(t, tt.want, got)
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/utils/metricsasync/ -run TestOptions_applyDefaults -v`
Expected: FAIL — package not found / `Options` undefined.

- [ ] **Step 3: Create the package skeleton**

```go
// pkg/utils/metricsasync/pool.go
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package metricsasync provides a shared goroutine pool that performs
// expensive Prometheus metric series cleanup off the controller Reconcile
// hot path. Reconcilers enqueue (kind, namespace, name) tuples and
// registered CleanupFunc implementations run on worker goroutines with
// per-key serialization and best-effort shutdown drain.
package metricsasync

import "time"

// CleanupFunc removes all metric series for one object identified by
// namespace/name. It MUST be idempotent and safe to invoke after the
// object is gone. Panics are recovered by the worker.
type CleanupFunc func(namespace, name string)

// Key is the workqueue payload. Identical Keys collapse via workqueue
// deduplication; the per-enqueue timestamp is tracked separately so
// re-enqueuing only refreshes that timestamp.
type Key struct {
	Kind      string
	Namespace string
	Name      string
}

// Enqueuer is the narrow interface reconcilers depend on. Pool
// implements it; tests inject a fake.
type Enqueuer interface {
	Enqueue(kind, namespace, name string)
}

// Options configures Pool. Zero values fall back to defaults via
// applyDefaults.
type Options struct {
	// Workers is the number of goroutines that drain the queue.
	// Defaults to 8 when <= 0; negative values are clamped to 1.
	Workers int

	// DrainTimeout caps how long Start blocks after the parent context
	// is cancelled, waiting for in-flight tasks to finish. Defaults to
	// 5s when zero. Negative values are normalized to 0 (do not wait).
	DrainTimeout time.Duration

	// QueueCap, when > 0, bounds the queue length. Enqueue calls
	// observed at or above the cap are dropped and counted under
	// metrics_async_dropped_total{reason="queue_full"}. 0 means
	// unbounded.
	QueueCap int

	// Name is the prometheus subsystem prefix. Defaults to
	// "metrics_async". Tests use a unique name to avoid double-register.
	Name string
}

func (o Options) applyDefaults() Options {
	out := o
	switch {
	case out.Workers == 0:
		out.Workers = 8
	case out.Workers < 0:
		out.Workers = 1
	}
	if out.DrainTimeout == 0 {
		out.DrainTimeout = 5 * time.Second
	} else if out.DrainTimeout < 0 {
		out.DrainTimeout = 0
	}
	if out.Name == "" {
		out.Name = "metrics_async"
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/utils/metricsasync/ -run TestOptions_applyDefaults -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/utils/metricsasync/pool.go pkg/utils/metricsasync/pool_test.go
git commit -m "feat(metricsasync): scaffold pool package with Options"
```

---

## Task 2: Self-observability metric collectors

**Files:**
- Create: `pkg/utils/metricsasync/metrics.go`
- Modify: `pkg/utils/metricsasync/pool.go`
- Modify: `pkg/utils/metricsasync/pool_test.go`

- [ ] **Step 1: Write the failing test**

Append to `pkg/utils/metricsasync/pool_test.go`:

```go
func TestNewCollectors_namesAndLabels(t *testing.T) {
	c := newCollectors("metrics_async_test1")
	// Each Vec is non-nil and accepts the documented label set.
	c.queueDepth.WithLabelValues("sandbox").Set(0)
	c.processedTotal.WithLabelValues("sandbox", "ok").Inc()
	c.duration.WithLabelValues("sandbox").Observe(0.001)
	c.latency.WithLabelValues("sandbox").Observe(0.001)
	c.dropped.WithLabelValues("sandbox", "queue_full").Inc()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/utils/metricsasync/ -run TestNewCollectors -v`
Expected: FAIL — `newCollectors` undefined.

- [ ] **Step 3: Implement collectors**

```go
// pkg/utils/metricsasync/metrics.go
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metricsasync

import (
	"github.com/prometheus/client_golang/prometheus"
)

// collectors holds the self-observability metric vectors for a Pool.
// They are scoped to a configurable subsystem name so tests can use
// unique names and avoid panics from duplicate registration.
type collectors struct {
	queueDepth     *prometheus.GaugeVec
	processedTotal *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	latency        *prometheus.HistogramVec
	dropped        *prometheus.CounterVec
}

func newCollectors(subsystem string) *collectors {
	return &collectors{
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Subsystem: subsystem,
			Name:      "queue_depth",
			Help:      "Current depth of the metric async cleanup queue per kind",
		}, []string{"kind"}),
		processedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "processed_total",
			Help:      "Total cleanup tasks processed by the metric async pool",
		}, []string{"kind", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Subsystem: subsystem,
			Name:      "duration_seconds",
			Help:      "Time spent inside the cleanup function",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 12), // 1ms -> ~4s
		}, []string{"kind"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Subsystem: subsystem,
			Name:      "latency_seconds",
			Help:      "Time from Enqueue to start of cleanup function",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms -> ~32s
		}, []string{"kind"}),
		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "dropped_total",
			Help:      "Tasks dropped without execution (unregistered, queue_full, drain_timeout)",
		}, []string{"kind", "reason"}),
	}
}

// register attaches the collectors to the supplied Registerer. Errors
// from prometheus (e.g., AlreadyRegisteredError) are returned unchanged
// so the caller can decide whether to ignore them.
func (c *collectors) register(reg prometheus.Registerer) error {
	for _, col := range []prometheus.Collector{c.queueDepth, c.processedTotal, c.duration, c.latency, c.dropped} {
		if err := reg.Register(col); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/utils/metricsasync/ -run TestNewCollectors -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/utils/metricsasync/metrics.go pkg/utils/metricsasync/pool_test.go
git commit -m "feat(metricsasync): add self-observability prometheus collectors"
```

---

## Task 3: Pool construction and kind registration

**Files:**
- Modify: `pkg/utils/metricsasync/pool.go`
- Modify: `pkg/utils/metricsasync/pool_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestPool_RegisterKind(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(p *Pool) error
		register    func(p *Pool) error
		expectError string
	}{
		{
			name:     "register single kind succeeds",
			register: func(p *Pool) error { return p.RegisterKind("sandbox", func(string, string) {}) },
		},
		{
			name: "register duplicate kind fails",
			setup: func(p *Pool) error {
				return p.RegisterKind("sandbox", func(string, string) {})
			},
			register:    func(p *Pool) error { return p.RegisterKind("sandbox", func(string, string) {}) },
			expectError: "already registered",
		},
		{
			name:        "empty kind rejected",
			register:    func(p *Pool) error { return p.RegisterKind("", func(string, string) {}) },
			expectError: "empty kind",
		},
		{
			name:        "nil func rejected",
			register:    func(p *Pool) error { return p.RegisterKind("sandbox", nil) },
			expectError: "nil CleanupFunc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPool(Options{Name: "metrics_async_test_register_" + tt.name})
			if tt.setup != nil {
				assert.NoError(t, tt.setup(p))
			}
			err := tt.register(p)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/utils/metricsasync/ -run TestPool_RegisterKind -v`
Expected: FAIL — `NewPool` and `RegisterKind` undefined.

- [ ] **Step 3: Implement Pool, NewPool, RegisterKind**

Append to `pkg/utils/metricsasync/pool.go` (after the `applyDefaults` method):

```go
import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/util/workqueue"
)
```

(Update the existing import block — add `errors`, `fmt`, `sync`, `sync/atomic`, and `k8s.io/client-go/util/workqueue`. The full import block should be:)

```go
import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)
```

(Some of those imports are used in later tasks — add them all now to avoid churn.)

Add the Pool type and constructor at the bottom of `pool.go`:

```go
// Pool is a shared async cleanup pool. It satisfies controller-runtime's
// manager.Runnable contract via Start.
type Pool struct {
	opts    Options
	queue   workqueue.TypedInterface[Key]
	col     *collectors

	mu       sync.RWMutex
	registry map[string]CleanupFunc

	enqueueAt sync.Map // Key -> int64 (unix nanos) of latest enqueue
	depth     sync.Map // kind string -> *int64 atomic counter

	started   atomic.Bool
}

// NewPool creates a Pool with the given Options. It does not start any
// goroutines; call Start to drive the pool from a manager.
func NewPool(opts Options) *Pool {
	o := opts.applyDefaults()
	return &Pool{
		opts:     o,
		queue:    workqueue.NewTyped[Key](),
		col:      newCollectors(o.Name),
		registry: make(map[string]CleanupFunc),
	}
}

// RegisterKind associates a CleanupFunc with a kind. Must be called
// before Start. Re-registering the same kind returns an error so
// configuration mistakes surface loudly.
func (p *Pool) RegisterKind(kind string, fn CleanupFunc) error {
	if kind == "" {
		return errors.New("metricsasync: empty kind")
	}
	if fn == nil {
		return errors.New("metricsasync: nil CleanupFunc")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, dup := p.registry[kind]; dup {
		return fmt.Errorf("metricsasync: kind %q already registered", kind)
	}
	p.registry[kind] = fn
	return nil
}

func (p *Pool) lookup(kind string) (CleanupFunc, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	fn, ok := p.registry[kind]
	return fn, ok
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/utils/metricsasync/ -run TestPool_RegisterKind -v`
Expected: PASS (4 sub-tests).

- [ ] **Step 5: Commit**

```bash
git add pkg/utils/metricsasync/pool.go pkg/utils/metricsasync/pool_test.go
git commit -m "feat(metricsasync): add Pool, NewPool, RegisterKind"
```

---

## Task 4: Enqueue with dedup, drop counters, depth tracking

**Files:**
- Modify: `pkg/utils/metricsasync/pool.go`
- Modify: `pkg/utils/metricsasync/pool_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestPool_Enqueue(t *testing.T) {
	tests := []struct {
		name       string
		opts       Options
		register   bool
		enqueues   []Key
		wantQueued int
		wantDrop   map[string]float64 // reason -> count
	}{
		{
			name:       "single enqueue queues one",
			opts:       Options{Name: "metrics_async_test_enqueue_single"},
			register:   true,
			enqueues:   []Key{{Kind: "sandbox", Namespace: "ns", Name: "a"}},
			wantQueued: 1,
		},
		{
			name:     "duplicate keys collapse",
			opts:     Options{Name: "metrics_async_test_enqueue_dedup"},
			register: true,
			enqueues: []Key{
				{Kind: "sandbox", Namespace: "ns", Name: "a"},
				{Kind: "sandbox", Namespace: "ns", Name: "a"},
				{Kind: "sandbox", Namespace: "ns", Name: "a"},
			},
			wantQueued: 1,
		},
		{
			name:       "unregistered kind dropped",
			opts:       Options{Name: "metrics_async_test_enqueue_unreg"},
			register:   false,
			enqueues:   []Key{{Kind: "sandbox", Namespace: "ns", Name: "a"}},
			wantQueued: 0,
			wantDrop:   map[string]float64{"unregistered": 1},
		},
		{
			name:     "queue_full drops past cap",
			opts:     Options{Name: "metrics_async_test_enqueue_cap", QueueCap: 1},
			register: true,
			enqueues: []Key{
				{Kind: "sandbox", Namespace: "ns", Name: "a"},
				{Kind: "sandbox", Namespace: "ns", Name: "b"},
				{Kind: "sandbox", Namespace: "ns", Name: "c"},
			},
			wantQueued: 1,
			wantDrop:   map[string]float64{"queue_full": 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPool(tt.opts)
			if tt.register {
				assert.NoError(t, p.RegisterKind("sandbox", func(string, string) {}))
			}
			for _, k := range tt.enqueues {
				p.Enqueue(k.Kind, k.Namespace, k.Name)
			}
			assert.Equal(t, tt.wantQueued, p.queue.Len())
			for reason, want := range tt.wantDrop {
				got := testCounter(t, p.col.dropped, "sandbox", reason)
				assert.Equal(t, want, got, "drop reason %s", reason)
			}
		})
	}
}

// testCounter reads a CounterVec child value via Write to a dto.Metric.
func testCounter(t *testing.T, vec *prometheus.CounterVec, lvs ...string) float64 {
	t.Helper()
	m, err := vec.GetMetricWithLabelValues(lvs...)
	assert.NoError(t, err)
	var dto dto.Metric
	assert.NoError(t, m.(prometheus.Metric).Write(&dto))
	return dto.GetCounter().GetValue()
}
```

Add to imports of `pool_test.go`: `"github.com/prometheus/client_golang/prometheus"`, `dto "github.com/prometheus/client_model/go"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/utils/metricsasync/ -run TestPool_Enqueue -v`
Expected: FAIL — `Enqueue` not defined.

- [ ] **Step 3: Implement Enqueue and depth helpers**

Append to `pool.go`:

```go
// Enqueue adds a (kind, namespace, name) tuple to the queue. Safe to
// call before or after Start. O(1) under contention; no metric vector
// locks are taken.
func (p *Pool) Enqueue(kind, namespace, name string) {
	if _, ok := p.lookup(kind); !ok {
		p.col.dropped.WithLabelValues(kind, "unregistered").Inc()
		klog.V(4).InfoS("metricsasync: enqueue dropped, unregistered kind", "kind", kind)
		return
	}
	if p.opts.QueueCap > 0 && p.queue.Len() >= p.opts.QueueCap {
		p.col.dropped.WithLabelValues(kind, "queue_full").Inc()
		return
	}
	key := Key{Kind: kind, Namespace: namespace, Name: name}
	p.enqueueAt.Store(key, time.Now().UnixNano())
	p.incDepth(kind)
	p.queue.Add(key)
}

func (p *Pool) incDepth(kind string) {
	v, ok := p.depth.Load(kind)
	if !ok {
		var n int64
		v, _ = p.depth.LoadOrStore(kind, &n)
	}
	atomic.AddInt64(v.(*int64), 1)
	p.col.queueDepth.WithLabelValues(kind).Set(float64(atomic.LoadInt64(v.(*int64))))
}

func (p *Pool) decDepth(kind string) {
	v, ok := p.depth.Load(kind)
	if !ok {
		return
	}
	atomic.AddInt64(v.(*int64), -1)
	p.col.queueDepth.WithLabelValues(kind).Set(float64(atomic.LoadInt64(v.(*int64))))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/utils/metricsasync/ -run TestPool_Enqueue -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/utils/metricsasync/pool.go pkg/utils/metricsasync/pool_test.go
git commit -m "feat(metricsasync): implement Enqueue with dedup and drop counters"
```

---

## Task 5: Worker loop, Start, and panic recovery

**Files:**
- Modify: `pkg/utils/metricsasync/pool.go`
- Modify: `pkg/utils/metricsasync/pool_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestPool_Start_processesAndRecovers(t *testing.T) {
	type result struct {
		ns, name string
	}
	tests := []struct {
		name      string
		fn        CleanupFunc
		enqueues  int
		distinct  int
		wantOK    float64
		wantPanic float64
	}{
		{
			name:     "ok path counts processed",
			fn:       func(string, string) {},
			enqueues: 5,
			distinct: 5,
			wantOK:   5,
		},
		{
			name:      "panic recorded as panic, worker survives",
			fn:        func(string, string) { panic("boom") },
			enqueues:  3,
			distinct:  3,
			wantPanic: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPool(Options{Name: "metrics_async_test_start_" + tt.name, Workers: 2, DrainTimeout: time.Second})
			var got sync.Map
			fn := tt.fn
			wrapped := func(ns, name string) {
				got.Store(result{ns, name}, struct{}{})
				fn(ns, name)
			}
			assert.NoError(t, p.RegisterKind("sandbox", wrapped))

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- p.Start(ctx) }()

			for i := 0; i < tt.enqueues; i++ {
				p.Enqueue("sandbox", "ns", fmt.Sprintf("n%d", i))
			}
			// Wait until processed.
			assert.Eventually(t, func() bool {
				return testCounter(t, p.col.processedTotal, "sandbox", "ok")+
					testCounter(t, p.col.processedTotal, "sandbox", "panic") >= float64(tt.distinct)
			}, 2*time.Second, 5*time.Millisecond)

			cancel()
			assert.NoError(t, <-done)

			if tt.wantOK > 0 {
				assert.Equal(t, tt.wantOK, testCounter(t, p.col.processedTotal, "sandbox", "ok"))
			}
			if tt.wantPanic > 0 {
				assert.Equal(t, tt.wantPanic, testCounter(t, p.col.processedTotal, "sandbox", "panic"))
			}
		})
	}
}
```

Add to test imports: `"context"`, `"fmt"`, `"sync"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/utils/metricsasync/ -run TestPool_Start_processesAndRecovers -v`
Expected: FAIL — `Start` not defined.

- [ ] **Step 3: Implement Start and worker loop**

Append to `pool.go`:

```go
// Start runs Workers worker goroutines and blocks until ctx is
// cancelled. After cancellation it shuts the queue down and waits up
// to DrainTimeout for in-flight tasks. Items still queued at timeout
// are reported via dropped_total{reason="drain_timeout"}.
//
// Implements sigs.k8s.io/controller-runtime/pkg/manager.Runnable.
func (p *Pool) Start(ctx context.Context) error {
	if !p.started.CompareAndSwap(false, true) {
		return errors.New("metricsasync: Start called twice")
	}
	// Register self-observability metrics against the controller-runtime
	// metrics registry. AlreadyRegistered is tolerated to keep tests and
	// repeated process starts (in unit tests using the same Name) safe.
	if err := p.col.register(metrics.Registry); err != nil {
		var are prometheus.AlreadyRegisteredError
		if !errors.As(err, &are) {
			return fmt.Errorf("metricsasync: register collectors: %w", err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < p.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.runWorker()
		}()
	}

	<-ctx.Done()
	p.queue.ShutDown()

	if p.opts.DrainTimeout <= 0 {
		// Do not wait; remaining items are dropped.
		remaining := p.queue.Len()
		if remaining > 0 {
			p.col.dropped.WithLabelValues("", "drain_timeout").Add(float64(remaining))
		}
		return nil
	}

	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
		return nil
	case <-time.After(p.opts.DrainTimeout):
		remaining := p.queue.Len()
		if remaining > 0 {
			p.col.dropped.WithLabelValues("", "drain_timeout").Add(float64(remaining))
			klog.InfoS("metricsasync: drain timeout reached", "remaining", remaining, "timeout", p.opts.DrainTimeout)
		}
		return nil
	}
}

func (p *Pool) runWorker() {
	for {
		key, shutdown := p.queue.Get()
		if shutdown {
			return
		}
		p.process(key)
		p.queue.Done(key)
	}
}

func (p *Pool) process(key Key) {
	p.decDepth(key.Kind)

	var enqueueAt int64
	if v, ok := p.enqueueAt.LoadAndDelete(key); ok {
		enqueueAt = v.(int64)
	}

	fn, ok := p.lookup(key.Kind)
	if !ok {
		p.col.dropped.WithLabelValues(key.Kind, "unregistered").Inc()
		return
	}

	defer func() {
		if r := recover(); r != nil {
			p.col.processedTotal.WithLabelValues(key.Kind, "panic").Inc()
			klog.ErrorS(fmt.Errorf("%v", r), "metricsasync: cleanup panicked",
				"kind", key.Kind, "namespace", key.Namespace, "name", key.Name)
		}
	}()

	if enqueueAt != 0 {
		p.col.latency.WithLabelValues(key.Kind).Observe(float64(time.Now().UnixNano()-enqueueAt) / 1e9)
	}
	start := time.Now()
	fn(key.Namespace, key.Name)
	p.col.duration.WithLabelValues(key.Kind).Observe(time.Since(start).Seconds())
	p.col.processedTotal.WithLabelValues(key.Kind, "ok").Inc()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/utils/metricsasync/ -run TestPool_Start_processesAndRecovers -v`
Expected: PASS (2 sub-tests).

- [ ] **Step 5: Commit**

```bash
git add pkg/utils/metricsasync/pool.go pkg/utils/metricsasync/pool_test.go
git commit -m "feat(metricsasync): implement Start, worker loop, panic recovery"
```

---

## Task 6: Same-key serialization test

**Files:**
- Modify: `pkg/utils/metricsasync/pool_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestPool_SameKey_NeverConcurrent(t *testing.T) {
	p := NewPool(Options{Name: "metrics_async_test_serial", Workers: 4, DrainTimeout: time.Second})

	const distinctKeys = 8
	const enqueuePerKey = 50
	var inflight [distinctKeys]int32
	var maxObserved int32
	fn := func(_, name string) {
		idx := int(name[1] - '0')
		now := atomic.AddInt32(&inflight[idx], 1)
		// snapshot peak per key
		for {
			cur := atomic.LoadInt32(&maxObserved)
			if now <= cur || atomic.CompareAndSwapInt32(&maxObserved, cur, now) {
				break
			}
		}
		// Hold long enough that a concurrent processor would race in.
		time.Sleep(2 * time.Millisecond)
		atomic.AddInt32(&inflight[idx], -1)
	}
	assert.NoError(t, p.RegisterKind("sandbox", fn))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()

	for i := 0; i < distinctKeys; i++ {
		for j := 0; j < enqueuePerKey; j++ {
			p.Enqueue("sandbox", "ns", fmt.Sprintf("n%d", i))
		}
	}

	// Each distinct key should be processed at least once; dedup means total <= distinctKeys * enqueuePerKey.
	assert.Eventually(t, func() bool {
		return testCounter(t, p.col.processedTotal, "sandbox", "ok") >= distinctKeys
	}, 3*time.Second, 5*time.Millisecond)

	cancel()
	assert.NoError(t, <-done)

	assert.LessOrEqual(t, atomic.LoadInt32(&maxObserved), int32(1),
		"workqueue must serialize processing of identical keys")
}
```

- [ ] **Step 2: Run test to verify it passes**

Run: `go test ./pkg/utils/metricsasync/ -run TestPool_SameKey_NeverConcurrent -v -race`
Expected: PASS (relies only on already-implemented behavior; this test pins the contract).

- [ ] **Step 3: Commit**

```bash
git add pkg/utils/metricsasync/pool_test.go
git commit -m "test(metricsasync): pin same-key serialization invariant"
```

---

## Task 7: Drain timeout behavior test

**Files:**
- Modify: `pkg/utils/metricsasync/pool_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestPool_Start_DrainTimeout(t *testing.T) {
	p := NewPool(Options{Name: "metrics_async_test_drain", Workers: 1, DrainTimeout: 50 * time.Millisecond})

	block := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(block)
		}
	}()
	fn := func(string, string) {
		<-block // never returns until released
	}
	assert.NoError(t, p.RegisterKind("sandbox", fn))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()

	// Enqueue two items; the first will block the only worker, the
	// second remains in the queue at shutdown.
	p.Enqueue("sandbox", "ns", "blocker")
	p.Enqueue("sandbox", "ns", "queued")

	// Give the worker a moment to pick up the first.
	time.Sleep(20 * time.Millisecond)

	cancel()
	startWait := time.Now()
	assert.NoError(t, <-done)
	elapsed := time.Since(startWait)
	assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond)
	assert.Less(t, elapsed, 500*time.Millisecond, "Start must return shortly after DrainTimeout")

	// The queued item is reported as drained-out.
	assert.GreaterOrEqual(t, testCounter(t, p.col.dropped, "", "drain_timeout"), float64(1))

	close(block)
	released = true
}
```

- [ ] **Step 2: Run test to verify it passes**

Run: `go test ./pkg/utils/metricsasync/ -run TestPool_Start_DrainTimeout -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add pkg/utils/metricsasync/pool_test.go
git commit -m "test(metricsasync): cover bounded drain timeout"
```

---

## Task 8: Export DeleteSandboxMetrics

**Files:**
- Modify: `pkg/controller/sandbox/metrics.go:544`
- Modify: `pkg/controller/sandbox/sandbox_controller.go:120`
- Modify: `pkg/controller/sandbox/metrics_test.go` (all call sites)

- [ ] **Step 1: Rename the function**

Edit `pkg/controller/sandbox/metrics.go`. At the function definition (line ~543):

Old:
```go
// deleteSandboxMetrics removes all metrics for a sandbox that has been deleted.
func deleteSandboxMetrics(namespace, name string) {
```

New:
```go
// DeleteSandboxMetrics removes all metrics for a sandbox that has been deleted.
// Exported so that the metricsasync pool (wired in cmd/agent-sandbox-controller)
// can register it as a CleanupFunc for kind "sandbox".
func DeleteSandboxMetrics(namespace, name string) {
```

- [ ] **Step 2: Update the synchronous call site (will be removed in Task 10, but keep tests compiling now)**

Edit `pkg/controller/sandbox/sandbox_controller.go:120`:

Old:
```go
deleteSandboxMetrics(req.NamespacedName.Namespace, req.NamespacedName.Name)
```

New (temporary, replaced in Task 10):
```go
DeleteSandboxMetrics(req.NamespacedName.Namespace, req.NamespacedName.Name)
```

- [ ] **Step 3: Update all test call sites**

In `pkg/controller/sandbox/metrics_test.go`, run a textual `replace_all`:
- `deleteSandboxMetrics(` → `DeleteSandboxMetrics(`

(There are ~50 occurrences; use the editor's replace-all and verify the diff.)

- [ ] **Step 4: Run tests**

Run: `go test ./pkg/controller/sandbox/ -run TestRecord -v`
Expected: PASS (compiles and existing test cases unchanged).

- [ ] **Step 5: Commit**

```bash
git add pkg/controller/sandbox/metrics.go pkg/controller/sandbox/sandbox_controller.go pkg/controller/sandbox/metrics_test.go
git commit -m "refactor(sandbox): export DeleteSandboxMetrics for async pool"
```

---

## Task 9: Inject Enqueuer into SandboxReconciler

**Files:**
- Modify: `pkg/controller/sandbox/sandbox_controller.go`

- [ ] **Step 1: Add the field and update the constructor**

Edit `pkg/controller/sandbox/sandbox_controller.go`:

Old (line ~59):
```go
func Add(mgr manager.Manager) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxGate) || !discovery.DiscoverGVK(sandboxControllerKind) {
		return nil
	}

	rateLimiter := core.NewRateLimiter()
	err := (&SandboxReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:      mgr.GetClient(),
			APIReader:   mgr.GetAPIReader(),
			Recorder:    mgr.GetEventRecorderFor("sandbox"),
			RateLimiter: rateLimiter,
		}), rateLimiter: rateLimiter,
	}).SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.Infof("Started SandboxReconciler successfully")
	return nil
}
```

New:
```go
// Enqueuer is the contract the Sandbox controller depends on for async
// metric cleanup. metricsasync.Pool satisfies it.
type Enqueuer interface {
	Enqueue(kind, namespace, name string)
}

func Add(mgr manager.Manager, metricsCleanup Enqueuer) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxGate) || !discovery.DiscoverGVK(sandboxControllerKind) {
		return nil
	}
	if metricsCleanup == nil {
		return fmt.Errorf("sandbox: metricsCleanup enqueuer is required")
	}

	rateLimiter := core.NewRateLimiter()
	err := (&SandboxReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:      mgr.GetClient(),
			APIReader:   mgr.GetAPIReader(),
			Recorder:    mgr.GetEventRecorderFor("sandbox"),
			RateLimiter: rateLimiter,
		}), rateLimiter: rateLimiter,
		metricsCleanup: metricsCleanup,
	}).SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.Infof("Started SandboxReconciler successfully")
	return nil
}
```

Old (struct definition near line 83):
```go
// SandboxReconciler reconciles a Sandbox object
type SandboxReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	controls    map[string]core.SandboxControl
	rateLimiter *core.RateLimiter
}
```

New:
```go
// SandboxReconciler reconciles a Sandbox object
type SandboxReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	controls       map[string]core.SandboxControl
	rateLimiter    *core.RateLimiter
	metricsCleanup Enqueuer
}
```

- [ ] **Step 2: Build to surface compile errors**

Run: `go build ./pkg/controller/sandbox/...`
Expected: FAIL — `sandbox.Add` signature changed; existing callers in `pkg/controller/controllers.go` no longer compile.

(That is intentional. Task 11 fixes it. Skip rebuilding the full module here.)

- [ ] **Step 3: Commit**

```bash
git add pkg/controller/sandbox/sandbox_controller.go
git commit -m "refactor(sandbox): accept Enqueuer in SandboxReconciler.Add"
```

---

## Task 10: Replace synchronous DeleteSandboxMetrics call

**Files:**
- Modify: `pkg/controller/sandbox/sandbox_controller.go:120`
- Modify: `pkg/controller/sandbox/sandbox_controller_test.go`

- [ ] **Step 1: Add a fake Enqueuer test helper**

Append to `pkg/controller/sandbox/sandbox_controller_test.go` (anywhere outside an existing function):

```go
// fakeEnqueuer captures Enqueue invocations for assertion.
type fakeEnqueuer struct {
	mu    sync.Mutex
	calls []struct{ Kind, Namespace, Name string }
}

func (f *fakeEnqueuer) Enqueue(kind, namespace, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct{ Kind, Namespace, Name string }{kind, namespace, name})
}

func (f *fakeEnqueuer) snapshot() []struct{ Kind, Namespace, Name string } {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]struct{ Kind, Namespace, Name string }, len(f.calls))
	copy(out, f.calls)
	return out
}
```

Add `"sync"` to the test file imports if not already present.

- [ ] **Step 2: Write the failing test**

Append:

```go
func TestReconcile_NotFoundEnqueuesAsyncCleanup(t *testing.T) {
	scheme := runtime.NewScheme()
	assert.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	assert.NoError(t, corev1.AddToScheme(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	enq := &fakeEnqueuer{}
	r := &SandboxReconciler{
		Client:         cli,
		Scheme:         scheme,
		metricsCleanup: enq,
	}

	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "missing"},
	})
	assert.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, res)

	calls := enq.snapshot()
	assert.Len(t, calls, 1)
	assert.Equal(t, "sandbox", calls[0].Kind)
	assert.Equal(t, "ns", calls[0].Namespace)
	assert.Equal(t, "missing", calls[0].Name)
}
```

Add to test imports as needed: `"context"`, `"github.com/stretchr/testify/assert"`, `"k8s.io/apimachinery/pkg/runtime"`, `"k8s.io/apimachinery/pkg/types"`, `corev1 "k8s.io/api/core/v1"`, `"sigs.k8s.io/controller-runtime/pkg/client/fake"`, `"sigs.k8s.io/controller-runtime/pkg/reconcile"`, `agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"` (most likely already present).

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./pkg/controller/sandbox/ -run TestReconcile_NotFoundEnqueuesAsyncCleanup -v`
Expected: FAIL — assertion of 1 call fails because the synchronous `DeleteSandboxMetrics` is still in place; `enq.calls` is empty.

- [ ] **Step 4: Replace the synchronous call with Enqueue**

Edit `pkg/controller/sandbox/sandbox_controller.go:120`:

Old:
```go
		if errors.IsNotFound(err) {
			box.Namespace = req.NamespacedName.Namespace
			box.Name = req.NamespacedName.Name
			core.ResourceVersionExpectations.Delete(box)
			core.ScaleExpectation.DeleteExpectations(utils.GetControllerKey(box))
			DeleteSandboxMetrics(req.NamespacedName.Namespace, req.NamespacedName.Name)
		}
```

New:
```go
		if errors.IsNotFound(err) {
			box.Namespace = req.NamespacedName.Namespace
			box.Name = req.NamespacedName.Name
			core.ResourceVersionExpectations.Delete(box)
			core.ScaleExpectation.DeleteExpectations(utils.GetControllerKey(box))
			r.metricsCleanup.Enqueue("sandbox", req.NamespacedName.Namespace, req.NamespacedName.Name)
		}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./pkg/controller/sandbox/ -run TestReconcile_NotFoundEnqueuesAsyncCleanup -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/controller/sandbox/sandbox_controller.go pkg/controller/sandbox/sandbox_controller_test.go
git commit -m "feat(sandbox): enqueue async metric cleanup on NotFound reconcile"
```

---

## Task 11: Update controllers.go to forward the pool

**Files:**
- Modify: `pkg/controller/controllers.go`

- [ ] **Step 1: Replace the package contents**

Old:
```go
package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/openkruise/agents/pkg/controller/sandbox"
	"github.com/openkruise/agents/pkg/controller/sandboxclaim"
	"github.com/openkruise/agents/pkg/controller/sandboxset"
	"github.com/openkruise/agents/pkg/controller/sandboxupdateops"
)

var controllerAddFuncs []func(manager.Manager) error

func init() {
	controllerAddFuncs = append(controllerAddFuncs, sandbox.Add)
	controllerAddFuncs = append(controllerAddFuncs, sandboxset.Add)
	controllerAddFuncs = append(controllerAddFuncs, sandboxclaim.Add)
	controllerAddFuncs = append(controllerAddFuncs, sandboxupdateops.Add)
}

func SetupWithManager(m manager.Manager) error {
	for _, f := range controllerAddFuncs {
		if err := f(m); err != nil {
			return err
		}
	}
	return nil
}
```

New:
```go
package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/openkruise/agents/pkg/controller/sandbox"
	"github.com/openkruise/agents/pkg/controller/sandboxclaim"
	"github.com/openkruise/agents/pkg/controller/sandboxset"
	"github.com/openkruise/agents/pkg/controller/sandboxupdateops"
	"github.com/openkruise/agents/pkg/utils/metricsasync"
)

// Deps bundles process-wide dependencies passed to controller Add funcs.
// New dependencies should be appended here rather than introducing extra
// AddFunc parameters across all controllers.
type Deps struct {
	MetricsCleanup *metricsasync.Pool
}

func SetupWithManager(m manager.Manager, deps Deps) error {
	if err := sandbox.Add(m, deps.MetricsCleanup); err != nil {
		return err
	}
	if err := sandboxset.Add(m); err != nil {
		return err
	}
	if err := sandboxclaim.Add(m); err != nil {
		return err
	}
	if err := sandboxupdateops.Add(m); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 2: Verify package compiles in isolation**

Run: `go vet ./pkg/controller/...`
Expected: PASS for `pkg/controller`, may FAIL for cmd until Task 12.

- [ ] **Step 3: Commit**

```bash
git add pkg/controller/controllers.go
git commit -m "refactor(controllers): pass Deps with metrics cleanup pool"
```

---

## Task 12: Wire pool in main; add flags + env

**Files:**
- Modify: `cmd/agent-sandbox-controller/main.go`

- [ ] **Step 1: Add a small env-fallback helper above main**

Find the imports block in `cmd/agent-sandbox-controller/main.go` and ensure these are present (add if missing):

```go
	"strconv"
	"time"

	"github.com/openkruise/agents/pkg/utils/metricsasync"
```

Above the existing flag declarations (just before `flag.StringVar(&metricsAddr, ...`), add:

```go
	// envInt returns the integer value of envVar, or fallback if unset/invalid.
	envInt := func(envVar string, fallback int) int {
		if v := os.Getenv(envVar); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		return fallback
	}
	envDuration := func(envVar string, fallback time.Duration) time.Duration {
		if v := os.Getenv(envVar); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				return d
			}
		}
		return fallback
	}
```

- [ ] **Step 2: Add the three flags**

Right after the `metricLabelsAllowlist` flag (line ~120), add:

```go
	var metricsAsyncWorkers int
	var metricsAsyncDrainTimeout time.Duration
	var metricsAsyncQueueCap int
	flag.IntVar(&metricsAsyncWorkers, "metrics-async-workers",
		envInt("METRICS_ASYNC_WORKERS", 8),
		"Number of goroutines draining the async metric cleanup queue.")
	flag.DurationVar(&metricsAsyncDrainTimeout, "metrics-async-drain-timeout",
		envDuration("METRICS_ASYNC_DRAIN_TIMEOUT", 5*time.Second),
		"Bounded time the manager waits for async metric cleanup to drain at shutdown.")
	flag.IntVar(&metricsAsyncQueueCap, "metrics-async-queue-cap",
		envInt("METRICS_ASYNC_QUEUE_CAP", 0),
		"Optional cap on the async metric cleanup queue. 0 means unbounded.")
```

- [ ] **Step 3: Build and register the pool, change SetupWithManager call**

Find the block that currently reads (line ~284):

```go
	setupLog.Info("setup controllers")
	if err = controller.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to setup controllers")
		os.Exit(1)
	}
```

Replace with:

```go
	metricsCleanupPool := metricsasync.NewPool(metricsasync.Options{
		Workers:      metricsAsyncWorkers,
		DrainTimeout: metricsAsyncDrainTimeout,
		QueueCap:     metricsAsyncQueueCap,
	})
	if err := metricsCleanupPool.RegisterKind("sandbox", sandboxctrl.DeleteSandboxMetrics); err != nil {
		setupLog.Error(err, "unable to register sandbox metric cleanup")
		os.Exit(1)
	}
	if err := mgr.Add(metricsCleanupPool); err != nil {
		setupLog.Error(err, "unable to add metrics cleanup pool to manager")
		os.Exit(1)
	}

	setupLog.Info("setup controllers",
		"metricsAsyncWorkers", metricsAsyncWorkers,
		"metricsAsyncDrainTimeout", metricsAsyncDrainTimeout,
		"metricsAsyncQueueCap", metricsAsyncQueueCap)
	if err = controller.SetupWithManager(mgr, controller.Deps{MetricsCleanup: metricsCleanupPool}); err != nil {
		setupLog.Error(err, "unable to setup controllers")
		os.Exit(1)
	}
```

- [ ] **Step 4: Build the binary**

Run: `go build ./cmd/agent-sandbox-controller/`
Expected: PASS, binary in `agent-sandbox-controller`. Delete it: `rm -f agent-sandbox-controller`.

- [ ] **Step 5: Commit**

```bash
git add cmd/agent-sandbox-controller/main.go
git commit -m "feat(cmd): wire metricsasync pool with flags and env fallbacks"
```

---

## Task 13: Final verification

**Files:** none

- [ ] **Step 1: Run package tests**

Run: `go test ./pkg/utils/metricsasync/ ./pkg/controller/sandbox/ -race -count=1`
Expected: PASS for both packages.

- [ ] **Step 2: Vet whole module**

Run: `go vet ./...`
Expected: PASS.

- [ ] **Step 3: Build the binary**

Run: `go build ./cmd/agent-sandbox-controller/ && rm -f agent-sandbox-controller`
Expected: PASS.

- [ ] **Step 4: Confirm metric names**

Run: `grep -n "metrics_async_" pkg/utils/metricsasync/metrics.go`
Expected: see `queue_depth`, `processed_total`, `duration_seconds`, `latency_seconds`, `dropped_total` under subsystem `metrics_async`.

- [ ] **Step 5: Commit any cleanup if needed**

If verification surfaced fixes, commit them with a short message; otherwise skip.

---

## Self-Review Notes

**Spec coverage:**
- §4.1 Pool package — Tasks 1-7.
- §4.2 Public API (Options, Key, Enqueuer, Pool, NewPool, RegisterKind, Enqueue, Start) — Tasks 1, 3, 4, 5.
- §4.3 Enqueue dedup + drop counters — Task 4.
- §4.4 Worker loop with panic recovery — Task 5.
- §4.5 Lifecycle, drain, drain-timeout — Tasks 5, 7.
- §4.6 Self-observability collectors — Task 2.
- §4.7 Sandbox controller wiring — Tasks 8, 9, 10.
- §4.7 cmd flags + env + register — Task 12; controllers Deps — Task 11.
- §6 Risks — same-key serialization invariant pinned by Task 6; panic recovery by Task 5; queue cap drop by Task 4; drain timeout drop by Task 7.
- §7 Test plan — covered across Tasks 1-7 and 10.

**Type consistency check:**
- `Enqueuer` is defined twice: once in `metricsasync` (Task 1) and once in `pkg/controller/sandbox` (Task 9). The sandbox-side definition is a local interface satisfied by `*metricsasync.Pool` — this is intentional (narrow consumer interface) and matches the design's §4.7. Different package, different name resolution; not a duplication bug.
- `Pool.Enqueue(kind, namespace, name)` signature matches the `Enqueuer` interface in both packages.
- `DeleteSandboxMetrics` signature `(namespace, name string)` matches `CleanupFunc`.
- `controller.Deps.MetricsCleanup *metricsasync.Pool` is the concrete type because `mgr.Add` requires a `manager.Runnable`. The downstream consumer (`sandbox.Add`) takes the narrower `Enqueuer` interface.

**Placeholder scan:** none found.
