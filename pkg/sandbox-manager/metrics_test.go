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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/openkruise/agents/pkg/sandbox-manager/config"
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := registerCollectors(prometheus.NewRegistry(), tt.collectors...)
			if tt.expectErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestBuildExposesDeprecatedResponseMetrics(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
	}{
		// The switch is process-global and sticky once enabled, so the disabled
		// case has to run first.
		{name: "deprecated metrics disabled", enabled: false},
		{name: "deprecated metrics enabled", enabled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := config.SandboxManagerOptions{EnableDeprecatedResponseMetrics: tt.enabled}
			// Build twice to confirm the registration is not repeated.
			for range 2 {
				_, err := NewSandboxManagerBuilder(opts).WithCustomInfra(withTestInfra(t, opts)).Build()
				require.NoError(t, err)
			}

			legacyBefore := testutil.ToFloat64(sandboxDeleteResponses.WithLabelValues("metrics-test", "success"))
			sandboxClaimCreationResponses.WithLabelValues("metrics-test", "success").Inc()
			incResult(sandboxDeleteTotal, sandboxDeleteResponses, "metrics-test", "success")

			// A disabled legacy counter is neither recorded nor exported.
			legacyWant := legacyBefore
			if tt.enabled {
				legacyWant++
			}
			assert.Equal(t, legacyWant, testutil.ToFloat64(sandboxDeleteResponses.WithLabelValues("metrics-test", "success")))

			names := []string{"sandbox_claim_creation_responses", "sandbox_delete_total"}
			if tt.enabled {
				names = append(names, "sandbox_delete_responses")
			}
			for _, name := range names {
				count, err := testutil.GatherAndCount(metrics.Registry, name)
				require.NoError(t, err)
				assert.Positive(t, count, "%s should be exported", name)
			}
			if !tt.enabled {
				count, err := testutil.GatherAndCount(metrics.Registry, "sandbox_delete_responses")
				require.NoError(t, err)
				assert.Zero(t, count, "deprecated counter should not be exported")
			}
			count, err := testutil.GatherAndCount(metrics.Registry, "sandbox_creation_total")
			require.NoError(t, err)
			assert.Zero(t, count, "manager should not export a CR creation counter")

			families, err := metrics.Registry.Gather()
			require.NoError(t, err)
			for _, family := range families {
				if family.GetName() == "sandbox_claim_creation_responses" {
					assert.False(t, strings.Contains(family.GetHelp(), "Deprecated"), "claim counter is not deprecated")
				}
			}
		})
	}
}
