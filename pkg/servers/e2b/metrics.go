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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Interface metrics are recorded by the E2B HTTP handlers from the moment a
// request enters the handler, so samples cover parameter parsing, sandbox
// lookup, and the operation itself. Lower layers must not record them again.
// None of the series is labeled by namespace: the sandbox-manager dashboard
// aggregates them without that dimension.
var (
	// sandboxClaimDuration tracks the create request duration of the claim path.
	sandboxClaimDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_duration_seconds",
			Help:        "Duration of sandbox create requests served by claim in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
	)

	// sandboxClaimCreationResponses tracks create request results of the claim
	// path.
	sandboxClaimCreationResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_claim_creation_responses",
			Help:        "Total number of sandbox create requests served by claim and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"}, // "success" or "failure"
	)

	// sandboxCloneDuration tracks the create request duration of the clone path.
	sandboxCloneDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_clone_duration_seconds",
			Help:        "Duration of sandbox create requests served by clone in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
	)

	// sandboxCloneTotal tracks create request results of the clone path.
	sandboxCloneTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_clone_total",
			Help:        "Total number of sandbox create requests served by clone and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"},
	)

	// sandboxPauseDuration tracks the pause request duration.
	sandboxPauseDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_pause_duration_seconds",
			Help:        "Duration of sandbox pause requests in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
	)

	// sandboxPauseResponses tracks total pause requests and their results.
	sandboxPauseResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_pause_responses",
			Help:        "Total number of sandbox pause requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"},
	)

	// sandboxResumeDuration tracks the resume request duration.
	sandboxResumeDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_resume_duration_seconds",
			Help:        "Duration of sandbox resume requests in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
	)

	// sandboxResumeResponses tracks total resume requests and their results.
	sandboxResumeResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_resume_responses",
			Help:        "Total number of sandbox resume requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"},
	)

	// sandboxDeleteDuration tracks the delete request duration.
	sandboxDeleteDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_delete_duration_seconds",
			Help:        "Duration of sandbox delete requests in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
	)

	// sandboxDeleteResponses tracks total delete requests and their results.
	sandboxDeleteResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_delete_responses",
			Help:        "Total number of sandbox delete requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"},
	)
)

// Snapshot series are not labeled by namespace. The sandbox-manager dashboard
// aggregates them without that dimension.
var (
	// snapshotDuration tracks snapshot creation latency.
	snapshotDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_snapshot_duration_seconds",
			Help:    "Snapshot creation latency in seconds",
			Buckets: prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
	)

	// snapshotTotal tracks total snapshot operations by result.
	snapshotTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_snapshot_total",
			Help: "Total number of snapshot operations",
		},
		[]string{"result"},
	)
)

// handlerCall records the interface outcome of a single handler invocation.
// A request is only counted once its operation has been attempted, so requests
// rejected during parsing or sandbox lookup produce no samples. Panics produce
// no samples either, whenever they happen. Successful requests also feed the
// duration histogram, measured from the handler entry.
type handlerCall struct {
	start     time.Time
	attempted bool
	succeeded bool
	duration  prometheus.Histogram
	responses *prometheus.CounterVec
}

func newHandlerCall(start time.Time, duration prometheus.Histogram, responses *prometheus.CounterVec) *handlerCall {
	return &handlerCall{start: start, duration: duration, responses: responses}
}

// attempt marks that the handler is about to invoke the operation.
func (c *handlerCall) attempt() { c.attempted = true }

// succeed marks that the handler is about to return its success response.
func (c *handlerCall) succeed() { c.succeeded = true }

// record reports the outcome. Handlers defer it so every return path produces
// at most one sample. A panicking handler produces no sample: recover the
// panic only to skip recording, then re-panic so the framework's recover still
// rewrites the response.
func (c *handlerCall) record() {
	if r := recover(); r != nil {
		panic(r)
	}
	switch {
	case !c.attempted:
		return
	case !c.succeeded:
		c.responses.WithLabelValues("failure").Inc()
	default:
		c.duration.Observe(time.Since(c.start).Seconds())
		c.responses.WithLabelValues("success").Inc()
	}
}

func init() {
	metrics.Registry.MustRegister(
		// Interface metrics.
		sandboxClaimDuration, sandboxClaimCreationResponses,
		sandboxCloneDuration, sandboxCloneTotal,
		sandboxPauseDuration, sandboxPauseResponses,
		sandboxResumeDuration, sandboxResumeResponses,
		sandboxDeleteDuration, sandboxDeleteResponses,
		// Snapshot metrics.
		snapshotDuration, snapshotTotal,
	)
}
