package sandboxcr

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// SandboxCreationLatency tracks the time from request to return
	SandboxCreationLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_creation_latency_ms",
			Help:    "Latency of sandbox creation in milliseconds",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10), // 10ms to ~10s
		},
	)

	// SandboxCreationResponses tracks total requests and failures
	SandboxCreationResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_creation_responses",
			Help: "Total number of sandbox creation requests and their results",
		},
		[]string{"result"}, // "success" or "failure"
	)
)

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(SandboxCreationLatency, SandboxCreationResponses)
}
