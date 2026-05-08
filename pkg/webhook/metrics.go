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

package webhook

import (
	"context"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var (
	// sandbox_admission_duration_seconds tracks per-call latency of admission
	// webhook handlers, partitioned by webhook path, admission operation, and
	// whether the request was allowed.
	admissionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandbox_admission_duration_seconds",
			Help:    "Duration of admission webhook handler invocations in seconds.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12), // 1ms -> ~4s
		},
		[]string{"webhook", "operation", "allowed"},
	)

	// sandbox_admission_total counts admission webhook handler invocations,
	// partitioned by webhook path, operation, and whether the request was allowed.
	admissionTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_admission_total",
			Help: "Total number of admission webhook handler invocations.",
		},
		[]string{"webhook", "operation", "allowed"},
	)
)

func init() {
	metrics.Registry.MustRegister(admissionDuration, admissionTotal)
}

// instrumentedHandler wraps an admission.Handler with Prometheus instrumentation:
// it records both a latency histogram and a call counter for each invocation,
// labelled by webhook path, admission operation, and allow/deny outcome.
type instrumentedHandler struct {
	inner admission.Handler
	path  string
}

// newInstrumentedHandler wraps h so each Handle call observes the admission
// metrics. path is used as the "webhook" label value (e.g. "/validate-pod-delete").
func newInstrumentedHandler(path string, h admission.Handler) admission.Handler {
	return &instrumentedHandler{inner: h, path: path}
}

func (i *instrumentedHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	start := time.Now()
	resp := i.inner.Handle(ctx, req)
	elapsed := time.Since(start).Seconds()

	operation := string(req.Operation)
	allowed := strconv.FormatBool(resp.Allowed)
	admissionDuration.WithLabelValues(i.path, operation, allowed).Observe(elapsed)
	admissionTotal.WithLabelValues(i.path, operation, allowed).Inc()

	return resp
}
