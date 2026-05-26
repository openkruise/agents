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
	"time"

	"k8s.io/client-go/util/workqueue"
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
