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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestResolveStartupBudget(t *testing.T) {
	tests := []struct {
		name             string
		maxUnavailable   *intstr.IntOrString
		observedReplicas int32
		expected         int
		expectError      string
	}{
		{name: "absent uses observed replicas", observedReplicas: 4, expected: 4},
		{name: "empty pool has budget one", observedReplicas: 0, expected: 1},
		{name: "absolute value", maxUnavailable: intOrStringPtr(intstr.FromInt(3)), observedReplicas: 10, expected: 3},
		{name: "percentage rounds up against observed replicas", maxUnavailable: intOrStringPtr(intstr.FromString("25%")), observedReplicas: 5, expected: 2},
		{name: "zero is raised to one", maxUnavailable: intOrStringPtr(intstr.FromInt(0)), observedReplicas: 5, expected: 1},
		{name: "invalid value", maxUnavailable: intOrStringPtr(intstr.FromString("invalid")), observedReplicas: 5, expectError: "invalid value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual, err := resolveStartupBudget(tt.maxUnavailable, tt.observedReplicas)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, actual)
		})
	}
}

func TestCalculateScalingLimited(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	newPending := func(name string, age time.Duration) *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(now.Add(-age))},
			Status:     agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPending},
		}
	}
	newFailed := func(name string) *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(now.Add(-time.Second))},
			Status: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{{
					Type:   string(agentsv1alpha1.SandboxConditionReady),
					Status: metav1.ConditionFalse,
					Reason: agentsv1alpha1.SandboxReadyReasonStartContainerFailed,
				}},
			},
		}
	}

	tests := []struct {
		name                 string
		maxUnavailable       *intstr.IntOrString
		sbxMaxPendingTimeout time.Duration
		observedReplicas     int32
		groups               GroupedSandboxes
		expectStatus         metav1.ConditionStatus
		expectReason         string
		expectMessage        string
		expectRequeue        bool
	}{
		{
			name:             "blockers below budget keep gate open",
			observedReplicas: 2,
			groups:           GroupedSandboxes{Creating: []*agentsv1alpha1.Sandbox{newPending("timeout", 61*time.Second)}},
			expectStatus:     metav1.ConditionFalse,
			expectReason:     scalingLimitedReasonBudgetAvailable,
			expectMessage:    "Timeout=1, Failed=0",
		},
		{
			name:           "timeout exhausts budget",
			maxUnavailable: intOrStringPtr(intstr.FromInt(1)),
			groups:         GroupedSandboxes{Creating: []*agentsv1alpha1.Sandbox{newPending("timeout", 61*time.Second)}},
			expectStatus:   metav1.ConditionTrue,
			expectReason:   scalingLimitedReasonBudgetExhausted,
			expectMessage:  "Timeout=1, Failed=0",
		},
		{
			name:           "failed and timeout are aggregated",
			maxUnavailable: intOrStringPtr(intstr.FromInt(2)),
			groups:         GroupedSandboxes{Creating: []*agentsv1alpha1.Sandbox{newPending("timeout", 61*time.Second), newFailed("failed")}},
			expectStatus:   metav1.ConditionTrue,
			expectReason:   scalingLimitedReasonBudgetExhausted,
			expectMessage:  "Timeout=1, Failed=1",
		},
		{
			name:                 "configured pending timeout is used",
			maxUnavailable:       intOrStringPtr(intstr.FromInt(1)),
			sbxMaxPendingTimeout: 25 * time.Second,
			groups:               GroupedSandboxes{Creating: []*agentsv1alpha1.Sandbox{newPending("timeout", 26*time.Second)}},
			expectStatus:         metav1.ConditionTrue,
			expectReason:         scalingLimitedReasonBudgetExhausted,
			expectMessage:        "Timeout=1, Failed=0",
		},
		{
			name:           "pending before timeout schedules requeue",
			maxUnavailable: intOrStringPtr(intstr.FromInt(1)),
			groups:         GroupedSandboxes{Creating: []*agentsv1alpha1.Sandbox{newPending("pending", 30*time.Second)}},
			expectStatus:   metav1.ConditionFalse,
			expectReason:   scalingLimitedReasonBudgetAvailable,
			expectMessage:  "Timeout=0, Failed=0",
			expectRequeue:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbxMaxPendingTimeout := tt.sbxMaxPendingTimeout
			if sbxMaxPendingTimeout == 0 {
				sbxMaxPendingTimeout = 60 * time.Second
			}
			r := &Reconciler{
				Recorder:             record.NewFakeRecorder(10),
				sbxMaxPendingTimeout: sbxMaxPendingTimeout,
			}
			sbs := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", Generation: 3},
				Spec: agentsv1alpha1.SandboxSetSpec{ScaleStrategy: agentsv1alpha1.SandboxSetScaleStrategy{
					MaxUnavailable: tt.maxUnavailable,
				}},
			}
			statusReplicas := tt.observedReplicas
			if statusReplicas == 0 {
				statusReplicas = int32(len(tt.groups.Creating))
			}
			status := &agentsv1alpha1.SandboxSetStatus{Replicas: statusReplicas}

			requeueAfter, err := r.calculateScalingLimited(context.Background(), sbs, status, tt.groups, now)
			require.NoError(t, err)
			condition := apiMeta.FindStatusCondition(status.Conditions, string(agentsv1alpha1.SandboxSetConditionScalingLimited))
			require.NotNil(t, condition)
			assert.Equal(t, tt.expectStatus, condition.Status)
			assert.Equal(t, tt.expectReason, condition.Reason)
			assert.Equal(t, int64(3), condition.ObservedGeneration)
			assert.Contains(t, condition.Message, tt.expectMessage)
			assert.Equal(t, tt.expectRequeue, requeueAfter > 0)
		})
	}
}
