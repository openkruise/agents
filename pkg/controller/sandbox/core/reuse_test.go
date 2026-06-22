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
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils"
)

type mockSandboxReuser struct {
	reuseErr      error
	completeVal   bool
	completeErr   error
	reuseCalled   bool
	completeCalls int
}

func (m *mockSandboxReuser) Reuse(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *corev1.Pod) error {
	m.reuseCalled = true
	return m.reuseErr
}

func (m *mockSandboxReuser) IsReuseComplete(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *corev1.Pod) (bool, error) {
	m.completeCalls++
	return m.completeVal, m.completeErr
}

func newTestReuseControl(t *testing.T, objs []client.Object, reuser SandboxReuser, reuseTimeout, gracePeriod time.Duration) (*SandboxReuseControl, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&agentsv1alpha1.Sandbox{}).
		Build()
	control := NewSandboxReuseControl(fakeClient, record.NewFakeRecorder(10), SandboxReuseConfig{
		Reuser:      reuser,
		Timeout:     reuseTimeout,
		GracePeriod: gracePeriod,
	})
	return control, fakeClient
}

func TestEnsureSandboxReused(t *testing.T) {
	readyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	tests := []struct {
		name             string
		reuser           *mockSandboxReuser
		reuseTimeout     time.Duration
		reuseGracePeriod time.Duration
		box              *agentsv1alpha1.Sandbox
		pod              *corev1.Pod
		newStatus        *agentsv1alpha1.SandboxStatus
		sbs              *agentsv1alpha1.SandboxSet
		expectError      string
		expectPhase      agentsv1alpha1.SandboxPhase
		expectRequeue    bool
		expectReuseCount   int32
		expectCondReason   string
		expectShutdownTime bool
		expectDeleted      bool
	}{
		{
			name:             "missing sandbox-pool label - reuse failed",
			reuser:           &mockSandboxReuser{},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			},
			newStatus:        &agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxReusing},
			expectCondReason: agentsv1alpha1.SandboxReusingReasonFailed,
		},
		{
			name:             "sandboxset not found - reuse failed",
			reuser:           &mockSandboxReuser{},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "nonexistent-pool",
					},
				},
			},
			newStatus:        &agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxReusing},
			expectCondReason: agentsv1alpha1.SandboxReusingReasonFailed,
		},
		{
			name:             "first entry - reuse started with noop reuser",
			reuser:           &mockSandboxReuser{},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuse:        "true",
						agentsv1alpha1.AnnotationReuseEnabled: "true",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus:        &agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxReusing},
			expectCondReason: agentsv1alpha1.SandboxReusingReasonStarted,
		},
		{
			name: "reuse in progress - not complete, no requeue",
			reuser: &mockSandboxReuser{
				completeVal: false,
			},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxReusingReasonStarted,
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectCondReason: agentsv1alpha1.SandboxReusingReasonStarted,
		},
		{
			name: "reuse in progress - complete, pod ready, enters grace period",
			reuser: &mockSandboxReuser{
				completeVal: true,
			},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuse: "true",
					},
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxReusingReasonStarted,
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectRequeue:    true,
			expectCondReason: agentsv1alpha1.SandboxReusingReasonCompleted,
		},
		{
			name: "reuse in progress - complete but pod not ready, waiting",
			reuser: &mockSandboxReuser{
				completeVal: true,
			},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			},
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxReusingReasonStarted,
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectCondReason: agentsv1alpha1.SandboxReusingReasonStarted,
		},
		{
			name: "pod is nil - reuse failed",
			reuser: &mockSandboxReuser{
				completeVal: true,
			},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
				},
			},
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxReusingReasonStarted,
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectCondReason: agentsv1alpha1.SandboxReusingReasonFailed,
		},
		{
			name:             "grace period elapsed - returns to pool",
			reuser:           &mockSandboxReuser{},
			reuseGracePeriod: 1 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxReusingReasonCompleted,
						LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * time.Second)),
					},
				},
			},
			expectPhase:      agentsv1alpha1.SandboxRunning,
			expectReuseCount: 1,
			expectCondReason: agentsv1alpha1.SandboxReusingReasonSucceeded,
		},
		{
			name: "reuse timeout - condition reason Timeout",
			reuser: &mockSandboxReuser{
				completeVal: false,
			},
			reuseTimeout:     1 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxReusingReasonStarted,
						LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * time.Second)),
					},
				},
			},
			expectCondReason: agentsv1alpha1.SandboxReusingReasonTimeout,
		},
		{
			name: "reuse failed - IsReuseComplete returns error",
			reuser: &mockSandboxReuser{
				completeErr: assert.AnError,
			},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxReusingReasonStarted,
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectCondReason: agentsv1alpha1.SandboxReusingReasonFailed,
		},
		{
			name: "reuse in progress - IsReuseComplete returns retriable error - retried",
			reuser: &mockSandboxReuser{
				completeErr: &RetriableError{Err: assert.AnError},
			},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxReusingReasonStarted,
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectError:      "assert.AnError general error for testing",
			expectCondReason: agentsv1alpha1.SandboxReusingReasonStarted,
		},
		{
			name: "reuse failed with valid retain-on-failure duration - ShutdownTime set",
			reuser: &mockSandboxReuser{
				completeErr: assert.AnError,
			},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuseRetainOnFailure: "5m",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxReusingReasonStarted,
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectRequeue:      true,
			expectCondReason:   agentsv1alpha1.SandboxReusingReasonFailed,
			expectShutdownTime: true,
		},
		{
			name: "reuse failed with invalid retain-on-failure value - sandbox deleted",
			reuser: &mockSandboxReuser{
				completeErr: assert.AnError,
			},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuseRetainOnFailure: "not-a-duration",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxReusingReasonStarted,
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectCondReason: agentsv1alpha1.SandboxReusingReasonFailed,
			expectDeleted:    true,
		},
		{
			name: "reuse failed with negative retain-on-failure duration - sandbox deleted",
			reuser: &mockSandboxReuser{
				completeErr: assert.AnError,
			},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuseRetainOnFailure: "-5m",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxReusingReasonStarted,
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectCondReason: agentsv1alpha1.SandboxReusingReasonFailed,
			expectDeleted:    true,
		},
		{
			name:             "fallthrough with GracePeriod == 0 - immediately succeeds",
			reuser: &mockSandboxReuser{
				completeVal: true,
			},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 0,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxReusingReasonStarted,
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectPhase:      agentsv1alpha1.SandboxRunning,
			expectReuseCount: 1,
			expectCondReason: agentsv1alpha1.SandboxReusingReasonSucceeded,
		},
		{
			name: "first entry - Reuse returns non-retriable error - reuse failed",
			reuser: &mockSandboxReuser{
				reuseErr: assert.AnError,
			},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuse:        "true",
						agentsv1alpha1.AnnotationReuseEnabled: "true",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus:        &agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxReusing},
			expectCondReason: agentsv1alpha1.SandboxReusingReasonFailed,
			expectDeleted:    true,
		},
		{
			name: "first entry - Reuse returns retriable error - retried",
			reuser: &mockSandboxReuser{
				reuseErr: &RetriableError{Err: assert.AnError},
			},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuse:        "true",
						agentsv1alpha1.AnnotationReuseEnabled: "true",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus:        &agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxReusing},
			expectError:      "assert.AnError general error for testing",
			expectCondReason: agentsv1alpha1.SandboxReusingReasonStarted,
		},
		{
			name:             "unknown condition reason - no-op",
			reuser:           &mockSandboxReuser{},
			reuseTimeout:     60 * time.Second,
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             "UnknownReason",
						LastTransitionTime: metav1.Now(),
					},
				},
			},
		},
		{
			name:             "grace period not yet elapsed - returns remaining time",
			reuser:           &mockSandboxReuser{},
			reuseGracePeriod: 10 * time.Second,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
				},
			},
			pod: readyPod,
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxReusing,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReusing),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxReusingReasonCompleted,
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectRequeue:    true,
			expectCondReason: agentsv1alpha1.SandboxReusingReasonCompleted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{tt.box}
			if tt.sbs != nil {
				objs = append(objs, tt.sbs)
			}
			control, fakeClient := newTestReuseControl(t, objs, tt.reuser, tt.reuseTimeout, tt.reuseGracePeriod)

			args := EnsureFuncArgs{Pod: tt.pod, Box: tt.box, NewStatus: tt.newStatus}
			requeue, err := control.ensureSandboxReused(context.TODO(), args)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
			}

			if tt.expectRequeue {
				assert.Greater(t, requeue, time.Duration(0))
			}

			cond := utils.GetSandboxCondition(tt.newStatus, string(agentsv1alpha1.SandboxConditionReusing))
			if tt.expectCondReason != "" {
				require.NotNil(t, cond)
				assert.Equal(t, tt.expectCondReason, cond.Reason)
			}

			if tt.expectPhase != "" {
				assert.Equal(t, tt.expectPhase, tt.newStatus.Phase)
			}

			if tt.expectReuseCount > 0 {
				assert.Equal(t, tt.expectReuseCount, tt.newStatus.ReuseCount)
			}
		
			if tt.expectShutdownTime {
				assert.NotNil(t, tt.box.Spec.ShutdownTime)
			}
		
			if tt.expectDeleted {
				err := fakeClient.Get(context.TODO(), types.NamespacedName{Name: tt.box.Name, Namespace: tt.box.Namespace}, &agentsv1alpha1.Sandbox{})
				assert.True(t, apierrors.IsNotFound(err), "expected sandbox to be deleted")
			}
		})
	}
}

func TestResetForPool(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name              string
		box               *agentsv1alpha1.Sandbox
		sbs               *agentsv1alpha1.SandboxSet
		expectError       string
		expectLabels      map[string]string
		expectAnnotations map[string]string
	}{
		{
			name: "no updated metadata - clears spec times, restores ownerRef, removes reuse annotation",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuse: "true",
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &now,
					PauseTime:    &now,
				},
			},
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
		},
		{
			name: "deletes user-specified metadata and fixed claim fields",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool:      "test-pool",
						agentsv1alpha1.LabelSandboxIsClaimed: "true",
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
						"user-label":                         "user-value",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuse:                "true",
						agentsv1alpha1.AnnotationReuseRetainOnFailure: "true",
						agentsv1alpha1.AnnotationLock:                 "some-lock",
						agentsv1alpha1.AnnotationOwner:                "some-user",
						agentsv1alpha1.AnnotationClaimTime:            "2026-06-17T00:00:00Z",
						agentsv1alpha1.AnnotationInitRuntimeRequest:   "{}",
						agentsv1alpha1.AnnotationRuntimeAccessToken:   "token",
						agentsv1alpha1.AnnotationCSIVolumeConfig:      `{"mountOptionList":[]}`,
						agentsv1alpha1.SandboxAnnotationPriority:      "100",
						identity.AgentKeyTokenRefreshStatus:           `{"accessTokenExpiration":"2026-06-18T00:00:00Z"}`,
						agentsv1alpha1.AnnotationEnvdAccessToken:      "legacy-envd-token",
						agentsv1alpha1.AnnotationEnvdURL:              "http://legacy-envd.example.com",
						agentsv1alpha1.AnnotationRuntimeURL:           "http://runtime.example.com",
						"user-anno": "user-value",
						agentsv1alpha1.AnnotationUpdatedMetadataInClaim: mustMarshal(agentsv1alpha1.UpdatedMetadataInClaim{
							Labels:      []string{"user-label"},
							Annotations: []string{"user-anno"},
						}),
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &now,
					PauseTime:    &now,
				},
			},
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			expectLabels: map[string]string{
				agentsv1alpha1.LabelSandboxPool:      "test-pool",
				agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.False,
			},
		},
		{
			name: "invalid updated-metadata-in-claim JSON - returns error",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "test-pool",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationUpdatedMetadataInClaim: "invalid-json",
					},
				},
			},
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{Replicas: 1},
			},
			expectError: "failed to unmarshal updated-metadata-in-claim",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{tt.box}
			if tt.sbs != nil {
				objs = append(objs, tt.sbs)
			}
			control, fakeClient := newTestReuseControl(t, objs, nil, 0, 0)

			err := control.resetMetadataForPool(context.TODO(), tt.box, tt.sbs)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)

			updated := &agentsv1alpha1.Sandbox{}
			err = fakeClient.Get(context.TODO(), types.NamespacedName{Name: tt.box.Name, Namespace: tt.box.Namespace}, updated)
			require.NoError(t, err)

			assert.Nil(t, updated.Spec.ShutdownTime)
			assert.Nil(t, updated.Spec.PauseTime)
			assert.Len(t, updated.OwnerReferences, 1)
			assert.Equal(t, tt.sbs.Name, updated.OwnerReferences[0].Name)
			assert.Equal(t, agentsv1alpha1.False, updated.Labels[agentsv1alpha1.LabelSandboxIsClaimed])
			assert.Empty(t, updated.Labels[agentsv1alpha1.LabelSandboxClaimName])
			assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationReuse])
			assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationReuseRetainOnFailure])
			assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationLock])
			assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationOwner])
			assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationClaimTime])
			assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest])
			assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken])
			assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationCSIVolumeConfig])
			assert.Empty(t, updated.Annotations[agentsv1alpha1.SandboxAnnotationPriority])
			assert.Empty(t, updated.Annotations[identity.AgentKeyTokenRefreshStatus])
			assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationEnvdAccessToken])
			assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationEnvdURL])
			assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationRuntimeURL])
			assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationUpdatedMetadataInClaim])
			if tt.expectLabels != nil {
				assert.Equal(t, tt.expectLabels, updated.Labels)
			}
			if tt.expectAnnotations != nil {
				assert.Equal(t, tt.expectAnnotations, updated.Annotations)
			}
		})
	}
}

func TestNoopSandboxReuser(t *testing.T) {
	reuser := &noopSandboxReuser{}

	err := reuser.Reuse(context.TODO(), &agentsv1alpha1.Sandbox{}, &corev1.Pod{})
	assert.NoError(t, err)

	complete, err := reuser.IsReuseComplete(context.TODO(), &agentsv1alpha1.Sandbox{}, &corev1.Pod{})
	assert.NoError(t, err)
	assert.True(t, complete)
}

func newFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}

func TestPodResetReuser_Reuse(t *testing.T) {
	tests := []struct {
		name        string
		sandbox     *agentsv1alpha1.Sandbox
		pod         *corev1.Pod
		expectError string
	}{
		{
			name: "patches pod with reset-request annotation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
				Status:     agentsv1alpha1.SandboxStatus{ReuseCount: 2},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{tt.pod}
			reuser := NewPodResetReuser(newFakeClient(objs...))

			err := reuser.Reuse(context.TODO(), tt.sandbox, tt.pod)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)

			updated := &corev1.Pod{}
			reuserImpl := reuser.(*PodResetReuser)
			err = reuserImpl.client.Get(context.TODO(), client.ObjectKeyFromObject(tt.sandbox), updated)
			require.NoError(t, err)

			raw := updated.Annotations[annotationResetRequest]
			require.NotEmpty(t, raw)

			var req ResetRequest
			require.NoError(t, json.Unmarshal([]byte(raw), &req))
			assert.Equal(t, "3", req.ResetID)
			assert.NotEmpty(t, req.RequestTime)
		})
	}
}

func TestPodResetReuser_IsReuseComplete(t *testing.T) {
	resetRequest := mustMarshal(ResetRequest{ResetID: "5", RequestTime: "2026-06-11T10:00:00Z"})

	tests := []struct {
		name           string
		pod            *corev1.Pod
		expectComplete bool
		expectError    string
	}{
		{
			name: "no condition - not complete",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "sbx-1", Namespace: "default",
					Annotations: map[string]string{annotationResetRequest: resetRequest},
				},
			},
		},
		{
			name: "stale condition with different resetID - not complete",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "sbx-1", Namespace: "default",
					Annotations: map[string]string{annotationResetRequest: resetRequest},
				},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   PodConditionResetComplete,
							Status: corev1.ConditionTrue,
							Reason: PodConditionResetReasonSucceeded,
							Message: mustMarshal(ResetResult{
								ResetID: "4", StartTime: "t1", FinishTime: "t2",
							}),
						},
					},
				},
			},
		},
		{
			name: "condition status True with matching resetID - complete",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "sbx-1", Namespace: "default",
					Annotations: map[string]string{annotationResetRequest: resetRequest},
				},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   PodConditionResetComplete,
							Status: corev1.ConditionTrue,
							Reason: PodConditionResetReasonSucceeded,
							Message: mustMarshal(ResetResult{
								ResetID: "5", StartTime: "t1", FinishTime: "t2",
							}),
						},
					},
				},
			},
			expectComplete: true,
		},
		{
			name: "condition status False with ResetFailed - returns error",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "sbx-1", Namespace: "default",
					Annotations: map[string]string{annotationResetRequest: resetRequest},
				},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   PodConditionResetComplete,
							Status: corev1.ConditionFalse,
							Reason: PodConditionResetReasonFailed,
							Message: mustMarshal(ResetResult{
								ResetID: "5", StartTime: "t1", FinishTime: "t2", Error: "disk full",
							}),
						},
					},
				},
			},
			expectError: "disk full",
		},
		{
			name: "condition status False with ResetTimeout - returns error",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "sbx-1", Namespace: "default",
					Annotations: map[string]string{annotationResetRequest: resetRequest},
				},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   PodConditionResetComplete,
							Status: corev1.ConditionFalse,
							Reason: PodConditionResetReasonTimeout,
							Message: mustMarshal(ResetResult{
								ResetID: "5", StartTime: "t1", FinishTime: "t2", Error: "timed out after 30s",
							}),
						},
					},
				},
			},
			expectError: "timed out after 30s",
		},
		{
			name: "invalid condition message JSON - not complete",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "sbx-1", Namespace: "default",
					Annotations: map[string]string{annotationResetRequest: resetRequest},
				},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:    PodConditionResetComplete,
							Status:  corev1.ConditionTrue,
							Reason:  PodConditionResetReasonSucceeded,
							Message: "not-valid-json",
						},
					},
				},
			},
		},
		{
			name: "missing reset-request annotation - not complete",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "sbx-1", Namespace: "default",
				},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   PodConditionResetComplete,
							Status: corev1.ConditionTrue,
							Reason: PodConditionResetReasonSucceeded,
							Message: mustMarshal(ResetResult{
								ResetID: "5", StartTime: "t1", FinishTime: "t2",
							}),
						},
					},
				},
			},
		},
		{
			name: "invalid annotation JSON - not complete",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "sbx-1", Namespace: "default",
					Annotations: map[string]string{annotationResetRequest: "bad-json"},
				},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   PodConditionResetComplete,
							Status: corev1.ConditionTrue,
							Reason: PodConditionResetReasonSucceeded,
							Message: mustMarshal(ResetResult{
								ResetID: "5", StartTime: "t1", FinishTime: "t2",
							}),
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
			}
			reuser := NewPodResetReuser(newFakeClient())

			complete, err := reuser.IsReuseComplete(context.TODO(), sandbox, tt.pod)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.False(t, complete)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expectComplete, complete)
		})
	}
}

func mustMarshal(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func TestNewSandboxReuseControl(t *testing.T) {
	t.Run("nil reuser defaults to noopSandboxReuser", func(t *testing.T) {
		control := NewSandboxReuseControl(newFakeClient(), record.NewFakeRecorder(10), SandboxReuseConfig{})
		require.NotNil(t, control.config.Reuser)
		complete, err := control.config.Reuser.IsReuseComplete(context.TODO(), nil, nil)
		assert.NoError(t, err)
		assert.True(t, complete)
	})

	t.Run("zero FailureShutdownGrace defaults to 5m", func(t *testing.T) {
		control := NewSandboxReuseControl(newFakeClient(), record.NewFakeRecorder(10), SandboxReuseConfig{})
		assert.Equal(t, defaultFailureShutdownGrace, control.config.FailureShutdownGrace)
	})

	t.Run("explicit values are preserved", func(t *testing.T) {
		reuser := &mockSandboxReuser{}
		control := NewSandboxReuseControl(newFakeClient(), record.NewFakeRecorder(10), SandboxReuseConfig{
			Reuser:               reuser,
			Timeout:              30 * time.Second,
			GracePeriod:          5 * time.Second,
			FailureShutdownGrace: 10 * time.Second,
		})
		assert.Equal(t, reuser, control.config.Reuser)
		assert.Equal(t, 30*time.Second, control.config.Timeout)
		assert.Equal(t, 5*time.Second, control.config.GracePeriod)
		assert.Equal(t, 10*time.Second, control.config.FailureShutdownGrace)
	})
}

func TestHandleReuseFailed(t *testing.T) {
	tests := []struct {
		name               string
		box                *agentsv1alpha1.Sandbox
		err                error
		patchFails         bool
		expectError        string
		expectRequeue      time.Duration
		expectDeleted      bool
		expectShutdownTime bool
		expectCondReason   string
	}{
		{
			name: "annotation not set - delete immediately",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			},
			err:              fmt.Errorf("some failure"),
			expectDeleted:    true,
			expectCondReason: agentsv1alpha1.SandboxReusingReasonFailed,
		},
		{
			name: "annotation valid duration - ShutdownTime set",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuseRetainOnFailure: "5m",
					},
				},
			},
			err:                fmt.Errorf("some failure"),
			expectRequeue:      5 * time.Minute,
			expectShutdownTime: true,
			expectCondReason:   agentsv1alpha1.SandboxReusingReasonFailed,
		},
		{
			name: "annotation invalid string - delete immediately",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuseRetainOnFailure: "not-a-duration",
					},
				},
			},
			err:              fmt.Errorf("some failure"),
			expectDeleted:    true,
			expectCondReason: agentsv1alpha1.SandboxReusingReasonFailed,
		},
		{
			name: "annotation negative duration - delete immediately",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuseRetainOnFailure: "-5m",
					},
				},
			},
			err:              fmt.Errorf("some failure"),
			expectDeleted:    true,
			expectCondReason: agentsv1alpha1.SandboxReusingReasonFailed,
		},
		{
			name: "annotation zero duration - delete immediately",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuseRetainOnFailure: "0s",
					},
				},
			},
			err:              fmt.Errorf("some failure"),
			expectDeleted:    true,
			expectCondReason: agentsv1alpha1.SandboxReusingReasonFailed,
		},
		{
			name: "timeout error - reason Timeout, sandbox deleted",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			},
			err:              &reuseTimeoutError{timeout: 30 * time.Second},
			expectDeleted:    true,
			expectCondReason: agentsv1alpha1.SandboxReusingReasonTimeout,
		},
		{
			name: "patch fails - error returned",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationReuseRetainOnFailure: "5m",
					},
				},
			},
			err:              fmt.Errorf("some failure"),
			patchFails:       true,
			expectError:      "failed to set shutdownTime on reuse failure",
			expectCondReason: agentsv1alpha1.SandboxReusingReasonFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{tt.box}
			var fakeClient client.Client
			if tt.patchFails {
				scheme := runtime.NewScheme()
				_ = clientgoscheme.AddToScheme(scheme)
				_ = agentsv1alpha1.AddToScheme(scheme)
				fakeClient = fake.NewClientBuilder().
					WithScheme(scheme).
					WithObjects(objs...).
					WithInterceptorFuncs(interceptor.Funcs{
						Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
							return fmt.Errorf("patch denied")
						},
					}).Build()
			} else {
				fakeClient = newFakeClient(objs...)
			}

			control := NewSandboxReuseControl(fakeClient, record.NewFakeRecorder(10), SandboxReuseConfig{
				Reuser: &noopSandboxReuser{},
			})

			newStatus := &agentsv1alpha1.SandboxStatus{}
			requeue, err := control.handleReuseFailed(context.TODO(), tt.box, newStatus, tt.err)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, tt.expectRequeue, requeue)

			cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReusing))
			require.NotNil(t, cond)
			assert.Equal(t, tt.expectCondReason, cond.Reason)

			if tt.expectDeleted {
				getErr := fakeClient.Get(context.TODO(), types.NamespacedName{Name: tt.box.Name, Namespace: tt.box.Namespace}, &agentsv1alpha1.Sandbox{})
				assert.True(t, apierrors.IsNotFound(getErr), "expected sandbox to be deleted")
			}

			if tt.expectShutdownTime {
				assert.NotNil(t, tt.box.Spec.ShutdownTime)
			}
		})
	}
}

func TestHandleReuseGracePeriod_ResetError(t *testing.T) {
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxPool: "test-pool",
			},
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationUpdatedMetadataInClaim: "invalid-json",
			},
		},
	}
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
	}

	control, _ := newTestReuseControl(t, []client.Object{box, sbs}, nil, 0, 1*time.Second)

	reuseCond := &metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionReusing),
		Status:             metav1.ConditionFalse,
		Reason:             agentsv1alpha1.SandboxReusingReasonCompleted,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * time.Second)),
	}
	newStatus := &agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxReusing}
	utils.SetSandboxCondition(newStatus, *reuseCond)

	args := EnsureFuncArgs{Box: box, NewStatus: newStatus}
	requeue, err := control.handleReuseGracePeriod(context.TODO(), args, reuseCond, sbs)

	require.Error(t, err)
	assert.True(t, IsRetriable(err))
	assert.Contains(t, err.Error(), "failed to unmarshal updated-metadata-in-claim")
	assert.Equal(t, time.Duration(0), requeue)
}

func TestDoReuse_SandboxSetGetError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxPool: "test-pool",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(box).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				if _, ok := obj.(*agentsv1alpha1.SandboxSet); ok {
					return fmt.Errorf("get unavailable")
				}
				return nil
			},
		}).Build()

	control := NewSandboxReuseControl(fakeClient, record.NewFakeRecorder(10), SandboxReuseConfig{
		Reuser: &noopSandboxReuser{},
	})

	args := EnsureFuncArgs{
		Box:       box,
		Pod:       &corev1.Pod{},
		NewStatus: &agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxReusing},
	}

	requeue, err := control.doReuse(context.TODO(), args)

	require.Error(t, err)
	assert.True(t, IsRetriable(err))
	assert.Contains(t, err.Error(), "failed to get SandboxSet")
	assert.Equal(t, time.Duration(0), requeue)
}

func TestPodResetReuser_Reuse_PatchError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return fmt.Errorf("patch denied")
			},
		}).Build()

	reuser := NewPodResetReuser(fakeClient)
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
	}

	err := reuser.Reuse(context.TODO(), sandbox, pod)

	require.Error(t, err)
	assert.True(t, IsRetriable(err))
	assert.Contains(t, err.Error(), "failed to patch pod with reset request")
}

func TestReuseTimeoutError(t *testing.T) {
	e := &reuseTimeoutError{timeout: 30 * time.Second}
	assert.Contains(t, e.Error(), "30s")
	assert.Equal(t, agentsv1alpha1.SandboxReusingReasonTimeout, e.Reason())
}
