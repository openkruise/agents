/*
Copyright 2025.

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

package sandboxset

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestRecordSandboxSetMetrics(t *testing.T) {
	tests := []struct {
		name              string
		namespace         string
		sbsName           string
		creationTimestamp time.Time
	}{
		{
			name:              "records creation timestamp and info for a SandboxSet",
			namespace:         "default",
			sbsName:           "test-sbs",
			creationTimestamp: time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC),
		},
		{
			name:              "records metrics for SandboxSet in custom namespace",
			namespace:         "production",
			sbsName:           "prod-sbs",
			creationTimestamp: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbs := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:              tt.sbsName,
					Namespace:         tt.namespace,
					CreationTimestamp: metav1.NewTime(tt.creationTimestamp),
				},
			}

			recordSandboxSetMetrics(sbs)
			defer deleteSandboxSetMetrics(tt.namespace, tt.sbsName)

			// Verify creation timestamp
			createdVal := testutil.ToFloat64(sandboxSetCreated.WithLabelValues(tt.namespace, tt.sbsName))
			expectedCreated := float64(tt.creationTimestamp.Unix())
			if createdVal != expectedCreated {
				t.Errorf("sandboxset_created = %v, want %v", createdVal, expectedCreated)
			}

		})
	}
}

func TestSandboxSetSandboxesCreatedTotal(t *testing.T) {
	tests := []struct {
		name        string
		namespace   string
		sbsName     string
		increments  int
		expectError string
	}{
		{
			name:        "counter increments once after a single sandbox creation",
			namespace:   "default",
			sbsName:     "counter-test-sbs-1",
			increments:  1,
			expectError: "",
		},
		{
			name:        "counter increments multiple times after multiple sandbox creations",
			namespace:   "production",
			sbsName:     "counter-test-sbs-2",
			increments:  5,
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up metrics before and after the test
			sandboxSetSandboxesCreatedTotal.Reset()

			for i := 0; i < tt.increments; i++ {
				sandboxSetSandboxesCreatedTotal.WithLabelValues(tt.namespace, tt.sbsName).Inc()
			}

			got := testutil.ToFloat64(sandboxSetSandboxesCreatedTotal.WithLabelValues(tt.namespace, tt.sbsName))
			expected := float64(tt.increments)
			if got != expected {
				t.Errorf("sandboxset_sandboxes_created_total = %v, want %v", got, expected)
			}
		})
	}
}

func TestSandboxSetSandboxesClaimedTotal(t *testing.T) {
	tests := []struct {
		name        string
		namespace   string
		sbsName     string
		increments  []int
		expected    float64
		expectError string
	}{
		{
			name:        "counter increments by count after a single call",
			namespace:   "default",
			sbsName:     "claimed-test-sbs-1",
			increments:  []int{1},
			expected:    1,
			expectError: "",
		},
		{
			name:        "counter accumulates after multiple calls",
			namespace:   "production",
			sbsName:     "claimed-test-sbs-2",
			increments:  []int{2, 3, 5},
			expected:    10,
			expectError: "",
		},
		{
			name:        "counter increments with a large count",
			namespace:   "staging",
			sbsName:     "claimed-test-sbs-3",
			increments:  []int{100},
			expected:    100,
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up metrics before and after the test
			sandboxSetSandboxesClaimedTotal.Reset()

			for _, count := range tt.increments {
				IncSandboxesClaimedTotal(tt.namespace, tt.sbsName, count)
			}

			got := testutil.ToFloat64(sandboxSetSandboxesClaimedTotal.WithLabelValues(tt.namespace, tt.sbsName))
			if got != tt.expected {
				t.Errorf("sandboxset_sandboxes_claimed_total = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestSandboxSetSandboxesClaimedTotalMultipleSandboxSets(t *testing.T) {
	tests := []struct {
		name string
		sets []struct {
			namespace string
			sbsName   string
			count     int
		}
		expectError string
	}{
		{
			name: "different namespace/name counters are independent",
			sets: []struct {
				namespace string
				sbsName   string
				count     int
			}{
				{namespace: "ns-a", sbsName: "sbs-x", count: 3},
				{namespace: "ns-b", sbsName: "sbs-y", count: 7},
				{namespace: "ns-a", sbsName: "sbs-z", count: 1},
			},
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up metrics before and after the test
			for _, s := range tt.sets {
				sandboxSetSandboxesClaimedTotal.DeleteLabelValues(s.namespace, s.sbsName)
			}
			defer func() {
				for _, s := range tt.sets {
					sandboxSetSandboxesClaimedTotal.DeleteLabelValues(s.namespace, s.sbsName)
				}
			}()

			// Increment each set's counter
			for _, s := range tt.sets {
				IncSandboxesClaimedTotal(s.namespace, s.sbsName, s.count)
			}

			// Verify each (namespace, name) counter equals its expected count
			for _, s := range tt.sets {
				got := testutil.ToFloat64(sandboxSetSandboxesClaimedTotal.WithLabelValues(s.namespace, s.sbsName))
				if got != float64(s.count) {
					t.Errorf("sandboxset_sandboxes_claimed_total[%s/%s] = %v, want %v", s.namespace, s.sbsName, got, float64(s.count))
				}
			}
		})
	}
}

func TestSandboxSetSandboxesClaimedTotalDeletion(t *testing.T) {
	tests := []struct {
		name        string
		namespace   string
		sbsName     string
		count       int
		expectError string
	}{
		{
			name:        "counter is removed after deleteSandboxSetMetrics",
			namespace:   "default",
			sbsName:     "claimed-delete-sbs",
			count:       5,
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up metrics before the test
			sandboxSetSandboxesClaimedTotal.Reset()

			// Increment counter
			IncSandboxesClaimedTotal(tt.namespace, tt.sbsName, tt.count)

			// Verify counter is set
			got := testutil.ToFloat64(sandboxSetSandboxesClaimedTotal.WithLabelValues(tt.namespace, tt.sbsName))
			if got != float64(tt.count) {
				t.Fatalf("sandboxset_sandboxes_claimed_total before delete = %v, want %v", got, float64(tt.count))
			}

			// Delete metrics - counters are now per-sandboxset and should be removed.
			deleteSandboxSetMetrics(tt.namespace, tt.sbsName)

			gotAfter := testutil.ToFloat64(sandboxSetSandboxesClaimedTotal.WithLabelValues(tt.namespace, tt.sbsName))
			if gotAfter != 0 {
				t.Errorf("sandboxset_sandboxes_claimed_total after delete = %v, want 0", gotAfter)
			}
		})
	}
}

func TestDeleteSandboxSetMetrics(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		sbsName   string
	}{
		{
			name:      "deletes all metrics for a SandboxSet",
			namespace: "default",
			sbsName:   "delete-test-sbs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbs := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:              tt.sbsName,
					Namespace:         tt.namespace,
					CreationTimestamp: metav1.NewTime(time.Now()),
				},
			}

			// Record all metrics first
			recordSandboxSetMetrics(sbs)
			sandboxSetReplicas.WithLabelValues(tt.namespace, tt.sbsName).Set(3)
			sandboxSetAvailableReplicas.WithLabelValues(tt.namespace, tt.sbsName).Set(2)
			sandboxSetDesiredReplicas.WithLabelValues(tt.namespace, tt.sbsName).Set(5)
			sandboxSetUpdatedReplicas.WithLabelValues(tt.namespace, tt.sbsName).Set(1)
			sandboxSetUpdatedAvailableReplicas.WithLabelValues(tt.namespace, tt.sbsName).Set(1)
			sandboxSetSandboxesCreatedTotal.WithLabelValues(tt.namespace, tt.sbsName).Inc()
			sandboxSetSandboxesCreatedTotal.WithLabelValues(tt.namespace, tt.sbsName).Inc()

			// Verify metrics are set
			if val := testutil.ToFloat64(sandboxSetCreated.WithLabelValues(tt.namespace, tt.sbsName)); val == 0 {
				t.Fatal("sandboxset_created should be set before delete")
			}
			if val := testutil.ToFloat64(sandboxSetReplicas.WithLabelValues(tt.namespace, tt.sbsName)); val != 3 {
				t.Fatalf("sandboxset_replicas before delete = %v, want 3", val)
			}
			if val := testutil.ToFloat64(sandboxSetUpdatedReplicas.WithLabelValues(tt.namespace, tt.sbsName)); val != 1 {
				t.Fatalf("sandboxset_updated_replicas before delete = %v, want 1", val)
			}

			// Delete all metrics
			deleteSandboxSetMetrics(tt.namespace, tt.sbsName)

			// After deletion, WithLabelValues creates a new zero-value gauge.
			if val := testutil.ToFloat64(sandboxSetCreated.WithLabelValues(tt.namespace, tt.sbsName)); val != 0 {
				t.Errorf("sandboxset_created after delete = %v, want 0", val)
			}
			if val := testutil.ToFloat64(sandboxSetReplicas.WithLabelValues(tt.namespace, tt.sbsName)); val != 0 {
				t.Errorf("sandboxset_replicas after delete = %v, want 0", val)
			}
			if val := testutil.ToFloat64(sandboxSetAvailableReplicas.WithLabelValues(tt.namespace, tt.sbsName)); val != 0 {
				t.Errorf("sandboxset_available_replicas after delete = %v, want 0", val)
			}
			if val := testutil.ToFloat64(sandboxSetDesiredReplicas.WithLabelValues(tt.namespace, tt.sbsName)); val != 0 {
				t.Errorf("sandboxset_desired_replicas after delete = %v, want 0", val)
			}
			if val := testutil.ToFloat64(sandboxSetUpdatedReplicas.WithLabelValues(tt.namespace, tt.sbsName)); val != 0 {
				t.Errorf("sandboxset_updated_replicas after delete = %v, want 0", val)
			}
			if val := testutil.ToFloat64(sandboxSetUpdatedAvailableReplicas.WithLabelValues(tt.namespace, tt.sbsName)); val != 0 {
				t.Errorf("sandboxset_updated_available_replicas after delete = %v, want 0", val)
			}
			if val := testutil.ToFloat64(sandboxSetSandboxesCreatedTotal.WithLabelValues(tt.namespace, tt.sbsName)); val != 0 {
				t.Errorf("sandboxset_sandboxes_created_total after delete = %v, want 0", val)
			}
		})
	}
}
