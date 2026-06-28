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

package quota

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	acquireTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "e2b_quota_acquire_total",
			Help: "Total quota acquire decisions.",
		},
		[]string{"result"},
	)
	backendErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "e2b_quota_backend_errors_total",
			Help: "Total quota backend errors by operation.",
		},
		[]string{"operation"},
	)
	breakerStateTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "e2b_quota_breaker_state_total",
			Help: "Total quota backend breaker state transitions.",
		},
		[]string{"state"},
	)
	breakerOpenDurationSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name: "e2b_quota_breaker_open_duration_seconds",
			Help: "Configured quota backend breaker open window duration.",
		},
	)
	releaseTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "e2b_quota_release_total",
			Help: "Total quota release decisions.",
		},
		[]string{"result"},
	)
	antiDriftSkippedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "e2b_quota_antidrift_skipped_total",
			Help: "Total quota anti-drift skips by reason.",
		},
		[]string{"reason"},
	)
	antiDriftErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "e2b_quota_antidrift_errors_total",
			Help: "Total quota anti-drift errors by operation.",
		},
		[]string{"operation"},
	)
	antiDriftEventReleaseTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "e2b_quota_antidrift_event_release_total",
			Help: "Total quota anti-drift event release results.",
		},
		[]string{"result"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		acquireTotal,
		backendErrorsTotal,
		breakerStateTotal,
		breakerOpenDurationSeconds,
		releaseTotal,
		antiDriftSkippedTotal,
		antiDriftErrorsTotal,
		antiDriftEventReleaseTotal,
	)
}
