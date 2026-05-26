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

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// CleanupFunc removes all metric series for one object identified by
// namespace/name. It MUST be idempotent and safe to invoke after the
// object is gone. Panics are recovered by the worker.
type CleanupFunc func(namespace, name string)

// Key is the workqueue payload. Identical Keys collapse via workqueue
// deduplication.
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

// Options configures Pool. Zero values are replaced by safe defaults.
type Options struct {
	// Workers is the number of goroutines that drain the queue.
	// Defaults to 8 when zero; negative values are clamped to 1.
	Workers int

	// DrainTimeout caps how long Start blocks after the parent context
	// is cancelled, waiting for in-flight tasks to finish. Values <= 0
	// mean "do not wait"; the caller (typically the controller cmd) is
	// responsible for choosing a sensible default such as 5s.
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
	if out.DrainTimeout <= 0 {
		out.DrainTimeout = 0
	}
	if out.Name == "" {
		out.Name = "metrics_async"
	}
	return out
}

// Pool is a shared async cleanup pool. It satisfies controller-runtime's
// manager.Runnable contract via Start (added in a later task).
type Pool struct {
	opts  Options
	queue workqueue.TypedInterface[Key]
	col   *collectors

	mu       sync.RWMutex
	registry map[string]CleanupFunc

	enqueueAt sync.Map // Key -> int64 (unix nanos) of latest enqueue
	depth     sync.Map // kind string -> *int64 atomic counter
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
