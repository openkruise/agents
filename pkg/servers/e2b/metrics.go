package e2b

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// SnapshotDuration tracks snapshot creation latency.
	SnapshotDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandboxmanager_snapshot_duration_ms",
			Help:    "Snapshot creation latency in milliseconds",
			Buckets: prometheus.ExponentialBuckets(100, 2, 10), // 100ms to ~100s
		},
	)

	// SnapshotTotal tracks total snapshot operations.
	SnapshotTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandboxmanager_snapshot_total",
			Help: "Total number of snapshot operations",
		},
		[]string{"status"},
	)
)

func init() {
	metrics.Registry.MustRegister(SnapshotDuration, SnapshotTotal)
}
