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

package sandboxset

import (
	"context"
	"testing"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCalculateSandboxSetStatusFromGroup(t *testing.T) {
	tests := []struct {
		name              string
		initialStatus     *agentsv1alpha1.SandboxSetStatus
		groups            GroupedSandboxes
		dirtyScaleUp      map[expectations.ScaleAction][]string
		expectedReplicas  int32
		expectedAvailable int32
		description       string
	}{
		{
			name: "empty groups and no dirty scale up",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating:  []*agentsv1alpha1.Sandbox{},
				Available: []*agentsv1alpha1.Sandbox{},
				Used:      []*agentsv1alpha1.Sandbox{},
				Dead:      []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp:      map[expectations.ScaleAction][]string{},
			expectedReplicas:  0,
			expectedAvailable: 0,
			description:       "should have 0 replicas and 0 available when all groups are empty",
		},
		{
			name: "only available sandboxes",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-3"}},
				},
				Used: []*agentsv1alpha1.Sandbox{},
				Dead: []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp:      map[expectations.ScaleAction][]string{},
			expectedReplicas:  3,
			expectedAvailable: 3,
			description:       "should count 3 available sandboxes",
		},
		{
			name: "only creating sandboxes",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
				},
				Available: []*agentsv1alpha1.Sandbox{},
				Used:      []*agentsv1alpha1.Sandbox{},
				Dead:      []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp:      map[expectations.ScaleAction][]string{},
			expectedReplicas:  2,
			expectedAvailable: 0,
			description:       "should count creating sandboxes in replicas but not in available",
		},
		{
			name: "creating and available sandboxes",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
				},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-3"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-4"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-5"}},
				},
				Used: []*agentsv1alpha1.Sandbox{},
				Dead: []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp:      map[expectations.ScaleAction][]string{},
			expectedReplicas:  5,
			expectedAvailable: 3,
			description:       "should count both creating and available sandboxes",
		},
		{
			name: "with dirty scale up (expectations not satisfied)",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
				},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-3"}},
				},
				Used: []*agentsv1alpha1.Sandbox{},
				Dead: []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp: map[expectations.ScaleAction][]string{
				expectations.Create: {"sandbox-pending-1", "sandbox-pending-2"},
			},
			expectedReplicas:  5,
			expectedAvailable: 2,
			description:       "should include dirty scale up in replicas count",
		},
		{
			name: "used sandboxes should not be counted",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
				},
				Used: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-3"}},
				},
				Dead: []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp:      map[expectations.ScaleAction][]string{},
			expectedReplicas:  1,
			expectedAvailable: 1,
			description:       "should not count used sandboxes",
		},
		{
			name: "dead sandboxes should not be counted",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
				},
				Used: []*agentsv1alpha1.Sandbox{},
				Dead: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-dead-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-dead-2"}},
				},
			},
			dirtyScaleUp:      map[expectations.ScaleAction][]string{},
			expectedReplicas:  1,
			expectedAvailable: 1,
			description:       "should not count dead sandboxes",
		},
		{
			name: "all types of sandboxes combined",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-creating-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-creating-2"}},
				},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-available-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-available-2"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-available-3"}},
				},
				Used: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-used-1"}},
				},
				Dead: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-dead-1"}},
				},
			},
			dirtyScaleUp: map[expectations.ScaleAction][]string{
				expectations.Create: {"sandbox-pending-1"},
			},
			expectedReplicas:  6,
			expectedAvailable: 3,
			description:       "should only count creating + available + dirtyCreate for replicas, and available for availableReplicas",
		},
		{
			name: "large scale with dirty scale up",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-3"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-4"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-5"}},
				},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-6"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-7"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-8"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-9"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-10"}},
				},
				Used: []*agentsv1alpha1.Sandbox{},
				Dead: []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp: map[expectations.ScaleAction][]string{
				expectations.Create: {
					"sandbox-pending-1",
					"sandbox-pending-2",
					"sandbox-pending-3",
				},
			},
			expectedReplicas:  13,
			expectedAvailable: 5,
			description:       "should correctly count large numbers of sandboxes",
		},
		{
			name: "dirty scale up with delete action should not affect replicas",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
				},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
				},
				Used: []*agentsv1alpha1.Sandbox{},
				Dead: []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp: map[expectations.ScaleAction][]string{
				expectations.Create: {"sandbox-pending-1"},
				expectations.Delete: {"sandbox-to-delete-1", "sandbox-to-delete-2"},
			},
			expectedReplicas:  3,
			expectedAvailable: 1,
			description:       "should only count Create dirty expectations, not Delete",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			// Make a copy of the initial status
			status := tt.initialStatus.DeepCopy()

			// Call the function
			calculateSandboxSetStatusFromGroup(ctx, status, tt.groups, tt.dirtyScaleUp)

			// Assert results
			assert.Equal(t, tt.expectedReplicas, status.Replicas, tt.description+" - replicas mismatch")
			assert.Equal(t, tt.expectedAvailable, status.AvailableReplicas, tt.description+" - availableReplicas mismatch")

			// Additional validation
			assert.GreaterOrEqual(t, status.Replicas, status.AvailableReplicas,
				"replicas should be >= availableReplicas")
		})
	}
}
