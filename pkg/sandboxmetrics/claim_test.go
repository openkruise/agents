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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordSandboxClaims(t *testing.T) {
	valid := SandboxClaimResult{UID: "uid", Namespace: "namespace", Method: "hot-start", Result: ClaimResultSuccess, Duration: time.Second}
	for _, tt := range []struct {
		name     string
		field    string
		value    string
		failure  bool
		duration time.Duration
		count    float64
	}{
		{name: "hot success", count: 1, duration: time.Second},
		{name: "cold failure", field: "method", value: "cold-start", failure: true, count: 1, duration: time.Second},
		{name: "missing UID", field: "uid"},
		{name: "missing method", field: "method"},
		{name: "missing result", field: "result"},
		{name: "unknown result", field: "result", value: "pending"},
		{name: "negative duration", duration: -time.Second},
		{name: "zero duration", count: 1},
		{name: "empty namespace is valid", field: "namespace", count: 1, duration: time.Second},
		{name: "nonempty method is preserved", field: "method", value: "custom", count: 1, duration: time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := valid
			result.Duration = tt.duration
			switch tt.field {
			case "uid":
				result.UID = tt.value
			case "method":
				result.Method = tt.value
			case "result":
				result.Result = tt.value
			case "namespace":
				result.Namespace = tt.value
			}
			if tt.failure {
				result.Result = ClaimResultFailure
			}
			total := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_claim_total", ConstLabels: prometheus.Labels{"source": "test"}}, []string{"namespace", "result", "type"})
			duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "test_claim_duration_seconds"}, []string{"namespace", "result", "type"})
			// Each supplied fact, including identical repeated bindings, is recorded
			// separately both within a batch and across calls.
			RecordSandboxClaims([]SandboxClaimResult{result, result}, total, duration)
			RecordSandboxClaims([]SandboxClaimResult{result}, total, duration)
			assert.Equal(t, 3*tt.count, testutil.ToFloat64(total.WithLabelValues(result.Namespace, result.Result, result.Method)))
			metric := &dto.Metric{}
			require.NoError(t, duration.WithLabelValues(result.Namespace, result.Result, result.Method).(prometheus.Metric).Write(metric))
			assert.Equal(t, uint64(3*tt.count), metric.GetHistogram().GetSampleCount())
			assert.Equal(t, 3*tt.count*tt.duration.Seconds(), metric.GetHistogram().GetSampleSum())
		})
	}
}
