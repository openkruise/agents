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

package e2b

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// SnapshotDuration tracks snapshot creation latency.
	SnapshotDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_snapshot_duration_seconds",
			Help:    "Snapshot creation latency in seconds",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10), // 100ms to ~51.2s
		},
	)

	// SnapshotTotal tracks total snapshot operations by result.
	SnapshotTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_snapshot_total",
			Help: "Total number of snapshot operations",
		},
		[]string{"result"},
	)
)

func init() {
	metrics.Registry.MustRegister(SnapshotDuration, SnapshotTotal)
}
