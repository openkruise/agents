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

package core

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/openkruise/agents/pkg/sandboxmetrics"
)

var (
	// sandboxSetClaimsTotal tracks the cumulative number of times a SandboxSet has been claimed.
	sandboxSetClaimsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandboxset_claims_total",
			Help:        "Total number of times a SandboxSet has been claimed by SandboxClaims",
			ConstLabels: prometheus.Labels{"source": "k8s"},
		},
		[]string{"namespace", "result"},
	)

	// sandboxClaimExpiredTotal tracks the cumulative number of SandboxClaims deleted due to TTL expiration.
	sandboxClaimExpiredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandboxclaim_expired_total",
			Help:        "Total number of SandboxClaims deleted due to TTL expiration",
			ConstLabels: prometheus.Labels{"source": "k8s"},
		},
		[]string{"namespace"},
	)

	// sandboxClaimTotal counts the instance claims driven by SandboxClaim reconciliation.
	// Instances claimed through sandbox-manager are reported by sandbox-manager under
	// source=e2b, so the two sources are mutually exclusive and may be summed.
	sandboxClaimTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_claim_total",
			Help: "Total number of completed instance claim processes handled by agent-sandbox-controller " +
				"through SandboxClaim, counted once per claim cycle of each sandbox. " +
				"type=hot-start: a pooled sandbox is claimed, including one still creating; " +
				"type=cold-start: a new sandbox is created and claimed because the pool is short. " +
				"result=failure means post-claim processing failed after binding; failures before any sandbox is bound are not counted",
			ConstLabels: prometheus.Labels{"source": "k8s"},
		},
		[]string{"namespace", "result", "type"},
	)

	// sandboxClaimDuration measures the same claim cycles as sandboxClaimTotal. Both
	// sources time the claim call itself, so the buckets match sandbox-manager's.
	sandboxClaimDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "sandbox_claim_duration_seconds",
			Help: "Wall-clock duration in seconds of completed instance claim processes handled by agent-sandbox-controller " +
				"through SandboxClaim, from claim start to the end of post-claim processing, for both success and failure. " +
				"type=hot-start: a pooled sandbox is claimed, including one still creating; " +
				"type=cold-start: a new sandbox is created and claimed because the pool is short",
			ConstLabels: prometheus.Labels{"source": "k8s"},
			Buckets:     append(prometheus.ExponentialBuckets(0.01, 2, 16), 600, 1200),
		},
		[]string{"namespace", "result", "type"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		sandboxSetClaimsTotal,
		sandboxClaimExpiredTotal,
		sandboxClaimTotal,
		sandboxClaimDuration,
	)
}

// recordSandboxClaims records every bound-attempt result using the shared recorder.
func recordSandboxClaims(results []sandboxmetrics.SandboxClaimResult) {
	sandboxmetrics.RecordSandboxClaims(results, sandboxClaimTotal, sandboxClaimDuration)
}
