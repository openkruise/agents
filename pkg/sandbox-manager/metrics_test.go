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

package sandbox_manager

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func newTestCounterVec(name string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name}, []string{"namespace", "result"})
}

func TestRegisterCollectors(t *testing.T) {
	dup := newTestCounterVec("test_dup_total")
	tests := []struct {
		name       string
		collectors []prometheus.Collector
		expectErr  bool
		counter    *prometheus.CounterVec
		metricName string
		labels     []string
		deprecated bool
	}{
		{
			name:       "registers every collector",
			collectors: []prometheus.Collector{newTestCounterVec("test_a_total"), newTestCounterVec("test_b_total")},
		},
		{
			name:       "duplicate collector is rejected",
			collectors: []prometheus.Collector{dup, dup},
			expectErr:  true,
		},
		{
			name: "production Claim collector name and labels", collectors: []prometheus.Collector{sandboxClaimTotal},
			counter: sandboxClaimTotal, metricName: "sandbox_claim_total", labels: []string{"register-test", "success", "cold-start", "create"},
		},
		{
			name: "production Creation collector name and labels", collectors: []prometheus.Collector{sandboxCreateTotal},
			counter: sandboxCreateTotal, metricName: "sandbox_create_total", labels: []string{"register-test", "success"},
		},
		{
			name: "production legacy Claim collector migration guidance", collectors: []prometheus.Collector{sandboxClaimCreationResponses},
			counter: sandboxClaimCreationResponses, metricName: "sandbox_claim_creation_responses", labels: []string{"register-test", "success"}, deprecated: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			err := registerCollectors(reg, tt.collectors...)
			if tt.expectErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tt.counter == nil {
				for _, collector := range tt.collectors {
					counter, ok := collector.(*prometheus.CounterVec)
					require.True(t, ok)
					counter.WithLabelValues("register-test", "success").Inc()
				}
				families, err := reg.Gather()
				require.NoError(t, err)
				assert.Len(t, families, len(tt.collectors), "every collector must be registered and exposed")
				return
			}
			tt.counter.WithLabelValues(tt.labels...).Inc()
			t.Cleanup(func() {
				assert.True(t, tt.counter.DeleteLabelValues(tt.labels...), "remove the test series from the production collector")
			})
			families, err := reg.Gather()
			require.NoError(t, err)
			require.Len(t, families, 1)
			assert.Equal(t, tt.metricName, families[0].GetName())
			if tt.deprecated {
				assert.Contains(t, families[0].GetHelp(), "Deprecated request-level claim results")
			}
			found := false
			for _, metric := range families[0].Metric {
				labels := map[string]string{}
				for _, label := range metric.Label {
					labels[label.GetName()] = label.GetValue()
				}
				if labels["namespace"] != "register-test" {
					continue
				}
				want := map[string]string{"namespace": "register-test", "result": "success", "source": "e2b"}
				if len(tt.labels) == 4 {
					want["type"] = "cold-start"
					want["lock_type"] = "create"
				}
				assert.Equal(t, want, labels)
				require.NotNil(t, metric.Counter)
				assert.Equal(t, float64(1), metric.GetCounter().GetValue())
				found = true
			}
			assert.True(t, found, "the exported metrics must include this test's counter series")
		})
	}
}

// isolateClaimMetrics isolates process-wide registration and compatibility state from subtest order.
func isolateClaimMetrics(t *testing.T) {
	t.Helper()
	oldRegistry, oldEnabled := metrics.Registry, deprecatedResponseMetricsEnabled.Load()
	oldClaim, oldCreation, oldDelete := sandboxClaimTotal, sandboxCreateTotal, sandboxDeleteTotal
	oldClaimDuration, oldCreateDuration := sandboxClaimDuration, sandboxCreateDuration
	oldClaimLegacy, oldPause, oldResume, oldDeleteLegacy := sandboxClaimCreationResponses, sandboxPauseResponses, sandboxResumeResponses, sandboxDeleteResponses
	oldCollectors := deprecatedResponseCounters
	t.Cleanup(func() {
		metrics.Registry = oldRegistry
		sandboxClaimTotal, sandboxCreateTotal, sandboxDeleteTotal = oldClaim, oldCreation, oldDelete
		sandboxClaimDuration, sandboxCreateDuration = oldClaimDuration, oldCreateDuration
		sandboxClaimCreationResponses, sandboxPauseResponses, sandboxResumeResponses, sandboxDeleteResponses = oldClaimLegacy, oldPause, oldResume, oldDeleteLegacy
		deprecatedResponseCounters = oldCollectors
		deprecatedResponseMetricsEnabled.Store(oldEnabled)
		registerDeprecatedOnce = sync.Once{}
		if oldEnabled {
			registerDeprecatedOnce.Do(func() {})
		}
	})
	newCounter := func(name string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, ConstLabels: prometheus.Labels{"source": "e2b"}}, labels)
	}
	sandboxClaimTotal = newCounter("sandbox_claim_total", "namespace", "result", "type", "lock_type")
	sandboxClaimDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "sandbox_claim_duration_seconds", ConstLabels: prometheus.Labels{"source": "e2b"},
		Buckets: append(prometheus.ExponentialBuckets(0.01, 2, 16), 600, 1200),
	}, []string{"namespace", "result", "type"})
	sandboxCreateDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "sandbox_create_duration_seconds", ConstLabels: prometheus.Labels{"source": "e2b"},
		Buckets: prometheus.ExponentialBuckets(0.02, 2, 12),
	}, []string{"namespace"})
	sandboxCreateTotal = newCounter("sandbox_create_total", "namespace", "result")
	sandboxDeleteTotal = newCounter("sandbox_delete_total", "namespace", "result")
	sandboxClaimCreationResponses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandbox_claim_creation_responses", Help: "Deprecated request-level claim results", ConstLabels: prometheus.Labels{"source": "e2b"},
	}, []string{"namespace", "result"})
	sandboxPauseResponses = newCounter("sandbox_pause_responses", "namespace", "result")
	sandboxResumeResponses = newCounter("sandbox_resume_responses", "namespace", "result")
	sandboxDeleteResponses = newCounter("sandbox_delete_responses", "namespace", "result")
	deprecatedResponseCounters = []prometheus.Collector{sandboxClaimCreationResponses, sandboxPauseResponses, sandboxResumeResponses, sandboxDeleteResponses}
	metrics.Registry = prometheus.NewRegistry()
	metrics.Registry.MustRegister(sandboxClaimTotal, sandboxClaimDuration, sandboxCreateTotal, sandboxCreateDuration, sandboxDeleteTotal)
	registerDeprecatedOnce = sync.Once{}
	deprecatedResponseMetricsEnabled.Store(false)
}

func resultCounterSum(t *testing.T, collector prometheus.Collector, namespace, result string) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(collector)
	families, err := reg.Gather()
	require.NoError(t, err)
	var sum float64
	for _, family := range families {
		for _, metric := range family.Metric {
			var ns, outcome string
			for _, label := range metric.Label {
				switch label.GetName() {
				case "namespace":
					ns = label.GetValue()
				case "result":
					outcome = label.GetValue()
				}
			}
			if ns == namespace && outcome == result {
				sum += metric.GetCounter().GetValue()
			}
		}
	}
	return sum
}

func TestRecordSandboxCreations(t *testing.T) {
	duration, negative, zero := 2*time.Second, -time.Second, time.Duration(0)
	tests := []struct {
		name             string
		results          []infra.SandboxCreationResult
		success, failure float64
		count            uint64
		sum              float64
	}{
		{name: "pool hit or no observed terminal outcome", results: nil},
		{name: "first Ready", results: []infra.SandboxCreationResult{{UID: "a", Ready: true}}, success: 1},
		{name: "startup Failed", results: []infra.SandboxCreationResult{{UID: "a"}}, failure: 1},
		{name: "retries preserve multiple UIDs", results: []infra.SandboxCreationResult{{UID: "a"}, {UID: "b", Ready: true}, {UID: "c", Ready: true}}, success: 2, failure: 1},
		{name: "mutually exclusive outcomes for the same UID", results: []infra.SandboxCreationResult{{UID: "a", Ready: true}, {UID: "a"}, {UID: "a", Ready: true}}, success: 1},
		{name: "duplicate UID after failure is not counted again", results: []infra.SandboxCreationResult{{UID: "a"}, {UID: "a", Ready: true}}, failure: 1},
		{name: "unknown UID is not attributed", results: []infra.SandboxCreationResult{{Ready: true}}},
		{name: "credible first Ready duration is recorded once", results: []infra.SandboxCreationResult{{UID: "a", Ready: true, Duration: &duration}, {UID: "a", Ready: true, Duration: &duration}}, success: 1, count: 1, sum: 2},
		{name: "real zero duration is not missing", results: []infra.SandboxCreationResult{{UID: "a", Ready: true, Duration: &zero}}, success: 1, count: 1},
		{name: "negative duration keeps counter only", results: []infra.SandboxCreationResult{{UID: "a", Ready: true, Duration: &negative}}, success: 1},
		{name: "Failed never records a success duration", results: []infra.SandboxCreationResult{{UID: "a", Duration: &duration}}, failure: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateClaimMetrics(t)
			for i := range tt.results {
				tt.results[i].Namespace = "creation-test"
			}
			recordSandboxCreations(tt.results)
			assert.Equal(t, tt.success, resultCounterSum(t, sandboxCreateTotal, "creation-test", "success"))
			assert.Equal(t, tt.failure, resultCounterSum(t, sandboxCreateTotal, "creation-test", "failure"))
			hist := managerHistogram(t, sandboxCreateDuration, "creation-test")
			assert.Equal(t, tt.count, hist.GetSampleCount())
			assert.Equal(t, tt.sum, hist.GetSampleSum())
			assert.Zero(t, testutil.CollectAndCount(sandboxClaimTotal), "creation facts do not increase instance claims")
		})
	}
}

func managerHistogram(t *testing.T, histogram *prometheus.HistogramVec, labels ...string) *dto.Histogram {
	t.Helper()
	metric := &dto.Metric{}
	require.NoError(t, histogram.WithLabelValues(labels...).(prometheus.Metric).Write(metric))
	return metric.GetHistogram()
}

func TestRecordSandboxClaims(t *testing.T) {
	claim := func(uid, id, method, result string, lock infra.LockType) infra.SandboxClaimResult {
		return infra.SandboxClaimResult{UID: uid, ID: id, Namespace: "claim-test", Method: method, Result: result, LockType: lock, Duration: 250 * time.Millisecond}
	}
	hot := claim("a", "1", "hot-start", "success", infra.LockTypeUpdate)
	speculate := claim("b", "2", "hot-start", "success", infra.LockTypeSpeculate)
	cold := claim("c", "3", "cold-start", "success", infra.LockTypeCreate)
	failed := claim("d", "4", "cold-start", "failure", infra.LockTypeCreate)
	invalid := hot
	invalid.Duration = -time.Second
	tests := []struct {
		name                                 string
		results                              []infra.SandboxClaimResult
		hotSuccess, coldSuccess, coldFailure float64
	}{
		{name: "a failure before binding produces no instance fact"},
		{name: "two hot claims and one cold claim", results: []infra.SandboxClaimResult{hot, speculate, cold}, hotSuccess: 2, coldSuccess: 1},
		{name: "failure after binding plus a successful retry on another instance", results: []infra.SandboxClaimResult{failed, cold}, coldSuccess: 1, coldFailure: 1},
		{name: "same UID and same cycle deduplicated", results: []infra.SandboxClaimResult{hot, hot}, hotSuccess: 1},
		{name: "same UID with a new cycle recorded separately", results: []infra.SandboxClaimResult{hot, claim("a", "new", "hot-start", "success", infra.LockTypeUpdate)}, hotSuccess: 2},
		{name: "missing identity or invalid result is not attributed", results: []infra.SandboxClaimResult{claim("", "1", "hot-start", "success", infra.LockTypeUpdate), claim("a", "", "hot-start", "success", infra.LockTypeUpdate), claim("a", "1", "", "success", infra.LockTypeUpdate), claim("a", "1", "hot-start", "unknown", infra.LockTypeUpdate), claim("a", "1", "hot-start", "success", ""), invalid}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateClaimMetrics(t)
			recordSandboxClaims(tt.results)
			for _, outcome := range []struct {
				method, result string
				count          float64
			}{
				{"hot-start", "success", tt.hotSuccess}, {"cold-start", "success", tt.coldSuccess}, {"cold-start", "failure", tt.coldFailure},
			} {
				var counter float64
				for _, lock := range []string{"create", "update", "speculate"} {
					counter += testutil.ToFloat64(sandboxClaimTotal.WithLabelValues("claim-test", outcome.result, outcome.method, lock))
				}
				assert.Equal(t, outcome.count, counter)
				hist := managerHistogram(t, sandboxClaimDuration, "claim-test", outcome.result, outcome.method)
				assert.Equal(t, uint64(outcome.count), hist.GetSampleCount())
				assert.Equal(t, outcome.count*0.25, hist.GetSampleSum())
			}
			assert.Zero(t, testutil.CollectAndCount(sandboxCreateTotal))
			assert.Zero(t, testutil.CollectAndCount(sandboxClaimCreationResponses))
		})
	}
}

func TestClaimCreateHistogramContract(t *testing.T) {
	for _, tt := range []struct {
		name      string
		histogram *prometheus.HistogramVec
		labels    []string
		buckets   []float64
	}{
		{"sandbox_claim_duration_seconds", sandboxClaimDuration, []string{"hist-contract", "failure", "hot-start"}, append(prometheus.ExponentialBuckets(0.01, 2, 16), 600, 1200)},
		{"sandbox_create_duration_seconds", sandboxCreateDuration, []string{"hist-contract"}, prometheus.ExponentialBuckets(0.02, 2, 12)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.histogram.WithLabelValues(tt.labels...).Observe(0.25)
			t.Cleanup(func() { tt.histogram.DeleteLabelValues(tt.labels...) })
			reg := prometheus.NewRegistry()
			reg.MustRegister(tt.histogram)
			families, err := reg.Gather()
			require.NoError(t, err)
			require.Len(t, families, 1)
			assert.Equal(t, tt.name, families[0].GetName())
			found := false
			for _, metric := range families[0].Metric {
				labels := map[string]string{}
				for _, label := range metric.Label {
					labels[label.GetName()] = label.GetValue()
				}
				if labels["namespace"] != "hist-contract" {
					continue
				}
				want := map[string]string{"namespace": "hist-contract", "source": "e2b"}
				if tt.name == "sandbox_claim_duration_seconds" {
					want["result"], want["type"] = "failure", "hot-start"
				}
				assert.Equal(t, want, labels)
				found = true
			}
			assert.True(t, found, "the Histogram must export the full set of test labels")
			hist := managerHistogram(t, tt.histogram, tt.labels...)
			var bounds []float64
			for _, bucket := range hist.Bucket {
				bounds = append(bounds, bucket.GetUpperBound())
			}
			assert.Equal(t, tt.buckets, bounds)
		})
	}
}

func TestBuildExposesDeprecatedResponseMetrics(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
	}{
		{name: "deprecated metrics enabled", enabled: true},
		{name: "deprecated metrics disabled", enabled: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateClaimMetrics(t)
			opts := config.SandboxManagerOptions{EnableDeprecatedResponseMetrics: tt.enabled}
			// Build twice to confirm the registration is not repeated.
			for range 2 {
				_, err := NewSandboxManagerBuilder(opts).WithCustomInfra(withTestInfra(t, opts)).Build()
				require.NoError(t, err)
			}

			recordSandboxClaims([]infra.SandboxClaimResult{
				{UID: "hot", ID: "hot-cycle", Namespace: "metrics-test", Method: "hot-start", LockType: infra.LockTypeUpdate, Result: "success"},
				{UID: "cold", ID: "cold-cycle", Namespace: "metrics-test", Method: "cold-start", LockType: infra.LockTypeCreate, Result: "success"},
			})
			incClaimRequestResult("metrics-test", "success")
			incClaimRequestResult("metrics-test", "success")
			incClaimRequestResult("metrics-test", "failure")
			incResult(sandboxDeleteTotal, sandboxDeleteResponses, "metrics-test", "success")
			recordSandboxCreations([]infra.SandboxCreationResult{{UID: "new-instance", Namespace: "metrics-test", Ready: true}})

			assert.Equal(t, float64(2), resultCounterSum(t, sandboxClaimTotal, "metrics-test", "success"))
			assert.Zero(t, resultCounterSum(t, sandboxClaimTotal, "metrics-test", "failure"), "a failure before binding only feeds the legacy request counter")
			assert.Equal(t, float64(1), resultCounterSum(t, sandboxCreateTotal, "metrics-test", "success"))
			for _, legacy := range []*prometheus.CounterVec{sandboxClaimCreationResponses, sandboxDeleteResponses} {
				count := testutil.CollectAndCount(legacy)
				if tt.enabled {
					assert.Positive(t, count)
				} else {
					assert.Zero(t, count, "disabled legacy metrics must not create child series")
				}
			}
			if tt.enabled {
				assert.Equal(t, float64(2), resultCounterSum(t, sandboxClaimCreationResponses, "metrics-test", "success"))
				assert.Equal(t, float64(1), resultCounterSum(t, sandboxClaimCreationResponses, "metrics-test", "failure"))
			}

			families, err := metrics.Registry.Gather()
			require.NoError(t, err)
			exported := map[string]bool{}
			for _, family := range families {
				exported[family.GetName()] = true
				for _, metric := range family.Metric {
					foundSource := false
					for _, label := range metric.Label {
						if label.GetName() == "source" {
							assert.Equal(t, "e2b", label.GetValue())
							foundSource = true
						}
					}
					assert.True(t, foundSource)
				}
				if family.GetName() == "sandbox_claim_creation_responses" {
					assert.True(t, strings.Contains(family.GetHelp(), "Deprecated request-level claim results"))
				}
			}
			assert.True(t, exported["sandbox_claim_total"])
			assert.True(t, exported["sandbox_create_total"])
			assert.True(t, exported["sandbox_claim_duration_seconds"])
			assert.False(t, exported["sandbox_creation_total"])
			assert.False(t, exported["sandbox_creation_duration_seconds"])
			assert.True(t, exported["sandbox_delete_total"])
			assert.Equal(t, tt.enabled, exported["sandbox_claim_creation_responses"])
			assert.Equal(t, tt.enabled, exported["sandbox_delete_responses"])
		})
	}
}
