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

package sandboxmetricsgc

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxctrl "github.com/openkruise/agents/pkg/controller/sandbox"
)

// Options configures the metrics GC controller. Zero values fall back to
// sensible defaults applied by NewReconciler.
type Options struct {
	// Workers controls MaxConcurrentReconciles. Defaults to 8.
	Workers int
	// ChannelBuffer sizes the GenericEvent channel. Defaults to 50000.
	// Sends that would block are dropped and counted under
	// sandbox_metrics_gc_dropped_total{reason="channel_full"}.
	ChannelBuffer int
}

const (
	defaultWorkers       = 8
	defaultChannelBuffer = 50000
)

// Reconciler garbage-collects Prometheus series for deleted Sandboxes.
type Reconciler struct {
	workers   int
	eventChan chan event.GenericEvent
}

// NewReconciler returns a Reconciler with the supplied options. It does not
// start any goroutines; call SetupWithManager.
func NewReconciler(opts Options) *Reconciler {
	if opts.Workers <= 0 {
		opts.Workers = defaultWorkers
	}
	if opts.ChannelBuffer <= 0 {
		opts.ChannelBuffer = defaultChannelBuffer
	}
	return &Reconciler{
		workers:   opts.Workers,
		eventChan: make(chan event.GenericEvent, opts.ChannelBuffer),
	}
}

// Enqueue is non-blocking. Calls that would block on a full channel are
// dropped and counted; the channel-fill threshold matches ChannelBuffer.
// Safe to call before or after SetupWithManager.
func (r *Reconciler) Enqueue(namespace, name string) {
	ev := event.GenericEvent{Object: &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}}
	select {
	case r.eventChan <- ev:
	default:
		droppedTotal.WithLabelValues("channel_full").Inc()
	}
}

// Reconcile drops all Prometheus series owned by the Sandbox controller for
// req.NamespacedName. DeleteSandboxMetrics is idempotent so repeated
// reconciles are safe and never error.
func (r *Reconciler) Reconcile(_ context.Context, req ctrl.Request) (ctrl.Result, error) {
	sandboxctrl.DeleteSandboxMetrics(req.Namespace, req.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the controller with the manager using a Channel
// source backed by r.eventChan. Workqueue dedup happens automatically once
// requests reach the controller queue.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("sandbox-metrics-gc").
		WithOptions(controller.Options{MaxConcurrentReconciles: r.workers}).
		WatchesRawSource(source.Channel(r.eventChan, &handler.EnqueueRequestForObject{})).
		Complete(r)
}
