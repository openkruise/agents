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
