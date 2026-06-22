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

package sandboxcr

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	fallbackReasonRVExpectation = "rv_expectation_unsatisfied"
	fallbackReasonCacheLagging  = "cache_lagging_behind_route"
)

var sandboxFallbackTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name:        "sandbox_get_claimed_fallback_total",
		Help:        "Number of GetClaimedSandbox fallbacks to APIReader, broken down by reason.",
		ConstLabels: prometheus.Labels{"source": "e2b"},
	},
	[]string{"namespace", "reason"},
)

func init() {
	metrics.Registry.MustRegister(sandboxFallbackTotal)
}
