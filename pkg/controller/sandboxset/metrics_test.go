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
		})
	}
}
