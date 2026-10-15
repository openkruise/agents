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

package web

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// httpRequestsTotal counts E2B HTTP API responses by frozen endpoint alias
	// and the status code actually written. It has no namespace, method, or
	// source label: alias already identifies the handler, and namespace
	// cardinality belongs to the existing operation metrics.
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_http_requests_total",
			Help: "Total number of E2B HTTP API requests",
		},
		[]string{"endpoint", "code"},
	)

	// httpRequestDuration tracks E2B HTTP API latency by endpoint alias.
	// Buckets match the other manager latency histograms. The histogram has
	// no code label; success rate comes from httpRequestsTotal.
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandbox_http_request_duration_seconds",
			Help:    "E2B HTTP API request latency in seconds",
			Buckets: prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
		[]string{"endpoint"},
	)
)

func init() {
	metrics.Registry.MustRegister(httpRequestsTotal, httpRequestDuration)
}
