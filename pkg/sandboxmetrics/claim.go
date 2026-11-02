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

package sandboxmetrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Values of SandboxClaimResult.Result.
const (
	ClaimResultSuccess = "success"
	ClaimResultFailure = "failure"
)

// SandboxClaimResult describes one claim result of an actually bound instance;
// it never covers failures that happen before any sandbox is bound.
// Producers emit exactly one result per bound attempt, including across retries.
type SandboxClaimResult struct {
	UID       string
	Namespace string
	Method    string
	// Result is ClaimResultSuccess or ClaimResultFailure.
	Result   string
	Duration time.Duration
}

// RecordSandboxClaims records each valid bound-attempt fact once. Repeated bindings
// of the same UID are separate claims. Method preserves hot-start/cold-start values.
func RecordSandboxClaims(results []SandboxClaimResult, total *prometheus.CounterVec, duration *prometheus.HistogramVec) {
	for _, result := range results {
		if result.UID == "" || result.Method == "" ||
			(result.Result != ClaimResultSuccess && result.Result != ClaimResultFailure) ||
			result.Duration < 0 {
			continue
		}
		total.WithLabelValues(result.Namespace, result.Result, result.Method).Inc()
		duration.WithLabelValues(result.Namespace, result.Result, result.Method).Observe(result.Duration.Seconds())
	}
}
