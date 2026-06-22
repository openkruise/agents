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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// droppedTotal counts Enqueue calls dropped because the GenericEvent channel
// was full. Reconcile latency and throughput come for free from
// controller_runtime_reconcile_* — this is the only failure mode that
// controller-runtime cannot observe on its own.
var droppedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "sandbox_metrics_gc_dropped_total",
		Help: "Enqueue calls dropped without being processed",
	},
	[]string{"reason"},
)

func init() {
	metrics.Registry.MustRegister(droppedTotal)
}
