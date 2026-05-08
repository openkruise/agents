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
)

func init() {
	metrics.Registry.MustRegister(
		sandboxSetClaimsTotal,
		sandboxClaimExpiredTotal,
	)
}
