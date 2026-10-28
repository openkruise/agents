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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
			name: "实际 Claim collector 名称与标签", collectors: []prometheus.Collector{sandboxClaimTotal},
			counter: sandboxClaimTotal, metricName: "sandbox_claim_total", labels: []string{"register-test", "success", "create"},
		},
		{
			name: "实际 Creation collector 名称与标签", collectors: []prometheus.Collector{sandboxCreationTotal},
			counter: sandboxCreationTotal, metricName: "sandbox_creation_total", labels: []string{"register-test", "success"},
		},
		{
			name: "实际旧 Claim collector 迁移说明", collectors: []prometheus.Collector{sandboxClaimCreationResponses},
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
				assert.Len(t, families, len(tt.collectors), "每个 collector 都应注册并暴露")
				return
			}
			tt.counter.WithLabelValues(tt.labels...).Inc()
			t.Cleanup(func() {
				assert.True(t, tt.counter.DeleteLabelValues(tt.labels...), "清理真实 collector 的测试序列")
			})
			families, err := reg.Gather()
			require.NoError(t, err)
			require.Len(t, families, 1)
			assert.Equal(t, tt.metricName, families[0].GetName())
			if tt.deprecated {
				assert.Contains(t, families[0].GetHelp(), "Deprecated: use sandbox_claim_total")
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
				if len(tt.labels) == 3 {
					want["lock_type"] = "create"
				}
				assert.Equal(t, want, labels)
				require.NotNil(t, metric.Counter)
				assert.Equal(t, float64(1), metric.GetCounter().GetValue())
				found = true
			}
			assert.True(t, found, "实际导出中应包含本测试的计数序列")
		})
	}
}

// isolateClaimMetrics 隔离进程级注册和兼容状态，不依赖子测试执行顺序。
func isolateClaimMetrics(t *testing.T) {
	t.Helper()
	oldRegistry, oldEnabled := metrics.Registry, deprecatedResponseMetricsEnabled.Load()
	oldClaim, oldCreation, oldDelete := sandboxClaimTotal, sandboxCreationTotal, sandboxDeleteTotal
	oldClaimLegacy, oldPause, oldResume, oldDeleteLegacy := sandboxClaimCreationResponses, sandboxPauseResponses, sandboxResumeResponses, sandboxDeleteResponses
	oldCollectors := deprecatedResponseCounters
	t.Cleanup(func() {
		metrics.Registry = oldRegistry
		sandboxClaimTotal, sandboxCreationTotal, sandboxDeleteTotal = oldClaim, oldCreation, oldDelete
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
	sandboxClaimTotal = newCounter("sandbox_claim_total", "namespace", "result", "lock_type")
	sandboxCreationTotal = newCounter("sandbox_creation_total", "namespace", "result")
	sandboxDeleteTotal = newCounter("sandbox_delete_total", "namespace", "result")
	sandboxClaimCreationResponses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandbox_claim_creation_responses", Help: "Deprecated: use sandbox_claim_total", ConstLabels: prometheus.Labels{"source": "e2b"},
	}, []string{"namespace", "result"})
	sandboxPauseResponses = newCounter("sandbox_pause_responses", "namespace", "result")
	sandboxResumeResponses = newCounter("sandbox_resume_responses", "namespace", "result")
	sandboxDeleteResponses = newCounter("sandbox_delete_responses", "namespace", "result")
	deprecatedResponseCounters = []prometheus.Collector{sandboxClaimCreationResponses, sandboxPauseResponses, sandboxResumeResponses, sandboxDeleteResponses}
	metrics.Registry = prometheus.NewRegistry()
	metrics.Registry.MustRegister(sandboxClaimTotal, sandboxCreationTotal, sandboxDeleteTotal)
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
	tests := []struct {
		name             string
		results          []infra.SandboxCreationResult
		success, failure float64
	}{
		{name: "池命中或未观测终态", results: nil},
		{name: "首次 Ready", results: []infra.SandboxCreationResult{{UID: "a", Ready: true}}, success: 1},
		{name: "启动 Failed", results: []infra.SandboxCreationResult{{UID: "a"}}, failure: 1},
		{name: "重试保留多个 UID", results: []infra.SandboxCreationResult{{UID: "a"}, {UID: "b", Ready: true}, {UID: "c", Ready: true}}, success: 2, failure: 1},
		{name: "同 UID 结果互斥", results: []infra.SandboxCreationResult{{UID: "a", Ready: true}, {UID: "a"}, {UID: "a", Ready: true}}, success: 1},
		{name: "失败后重复 UID 不再计数", results: []infra.SandboxCreationResult{{UID: "a"}, {UID: "a", Ready: true}}, failure: 1},
		{name: "未知 UID 不归因", results: []infra.SandboxCreationResult{{Ready: true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateClaimMetrics(t)
			for i := range tt.results {
				tt.results[i].Namespace = "creation-test"
			}
			recordSandboxCreations(tt.results)
			assert.Equal(t, tt.success, resultCounterSum(t, sandboxCreationTotal, "creation-test", "success"))
			assert.Equal(t, tt.failure, resultCounterSum(t, sandboxCreationTotal, "creation-test", "failure"))
			assert.Zero(t, testutil.CollectAndCount(sandboxClaimTotal), "创建结果不会额外产生 Claim 请求")
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

			incClaimResult("metrics-test", "success", "update")
			incClaimResult("metrics-test", "success", "create")
			incClaimResult("metrics-test", "failure", "")
			incResult(sandboxDeleteTotal, sandboxDeleteResponses, "metrics-test", "success")
			recordSandboxCreations([]infra.SandboxCreationResult{{UID: "new-instance", Namespace: "metrics-test", Ready: true}})

			assert.Equal(t, float64(2), resultCounterSum(t, sandboxClaimTotal, "metrics-test", "success"))
			assert.Equal(t, float64(1), resultCounterSum(t, sandboxClaimTotal, "metrics-test", "failure"))
			assert.Equal(t, float64(1), testutil.ToFloat64(sandboxClaimTotal.WithLabelValues("metrics-test", "failure", "unknown")))
			assert.Equal(t, float64(1), resultCounterSum(t, sandboxCreationTotal, "metrics-test", "success"))
			for _, legacy := range []*prometheus.CounterVec{sandboxClaimCreationResponses, sandboxDeleteResponses} {
				count := testutil.CollectAndCount(legacy)
				if tt.enabled {
					assert.Positive(t, count)
				} else {
					assert.Zero(t, count, "关闭时旧指标不能创建 child series")
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
					assert.True(t, strings.Contains(family.GetHelp(), "Deprecated: use sandbox_claim_total"))
				}
			}
			assert.True(t, exported["sandbox_claim_total"])
			assert.True(t, exported["sandbox_creation_total"])
			assert.True(t, exported["sandbox_delete_total"])
			assert.Equal(t, tt.enabled, exported["sandbox_claim_creation_responses"])
			assert.Equal(t, tt.enabled, exported["sandbox_delete_responses"])
		})
	}
}
