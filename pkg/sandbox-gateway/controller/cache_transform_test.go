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

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils"
	proxyutils "github.com/openkruise/agents/pkg/utils/proxyutils"
)

func TestProjectSandboxForGatewayCachePreservesRouteAndState(t *testing.T) {
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	restore := utils.SetNowFuncForTesting(func() time.Time { return now })
	defer restore()

	tests := []struct {
		name               string
		mutate             func(*agentsv1alpha1.Sandbox)
		expectedState      string
		expectedRouteState string
	}{
		{
			name:          "running and ready",
			expectedState: agentsv1alpha1.SandboxStateRunning,
		},
		{
			name: "running and paused",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Spec.Paused = true
			},
			expectedState: agentsv1alpha1.SandboxStatePaused,
		},
		{
			name: "running and not ready",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Status.Conditions[0].Status = metav1.ConditionFalse
			},
			expectedState: agentsv1alpha1.SandboxStateDead,
		},
		{
			name: "running with unknown readiness",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Status.Conditions[0].Status = metav1.ConditionUnknown
			},
			expectedState: agentsv1alpha1.SandboxStateDead,
		},
		{
			name: "pending",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Status.Phase = agentsv1alpha1.SandboxPending
			},
			expectedState: agentsv1alpha1.SandboxStateCreating,
		},
		{
			name: "paused phase",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Status.Phase = agentsv1alpha1.SandboxPaused
			},
			expectedState: agentsv1alpha1.SandboxStatePaused,
		},
		{
			name: "resuming phase",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Status.Phase = agentsv1alpha1.SandboxResuming
			},
			expectedState: agentsv1alpha1.SandboxStatePaused,
		},
		{
			name: "upgrading phase",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Status.Phase = agentsv1alpha1.SandboxUpgrading
			},
			expectedState: agentsv1alpha1.SandboxStatePaused,
		},
		{
			name: "recycling phase",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Status.Phase = agentsv1alpha1.SandboxRecycling
			},
			expectedState: agentsv1alpha1.SandboxStatePaused,
		},
		{
			name: "empty phase",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Status.Phase = ""
			},
			expectedState: agentsv1alpha1.SandboxStatePaused,
		},
		{
			name: "succeeded",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Status.Phase = agentsv1alpha1.SandboxSucceeded
			},
			expectedState: agentsv1alpha1.SandboxStateDead,
		},
		{
			name: "failed",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Status.Phase = agentsv1alpha1.SandboxFailed
			},
			expectedState: agentsv1alpha1.SandboxStateDead,
		},
		{
			name: "terminating",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Status.Phase = agentsv1alpha1.SandboxTerminating
			},
			expectedState: agentsv1alpha1.SandboxStateDead,
		},
		{
			name: "sandbox set ready",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(
					&agentsv1alpha1.SandboxSet{},
					agentsv1alpha1.SandboxSetControllerKind,
				)}
			},
			expectedState: agentsv1alpha1.SandboxStateAvailable,
		},
		{
			name: "sandbox set not ready",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(
					&agentsv1alpha1.SandboxSet{},
					agentsv1alpha1.SandboxSetControllerKind,
				)}
				sandbox.Status.Conditions[0].Status = metav1.ConditionFalse
			},
			expectedState: agentsv1alpha1.SandboxStateCreating,
		},
		{
			name: "shutdown time reached",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				shutdownTime := metav1.NewTime(now.Add(-time.Minute))
				sandbox.Spec.ShutdownTime = &shutdownTime
			},
			expectedState: agentsv1alpha1.SandboxStateDead,
		},
		{
			name: "deleting",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				deletionTime := metav1.NewTime(now)
				sandbox.DeletionTimestamp = &deletionTime
			},
			expectedState: agentsv1alpha1.SandboxStateDead,
		},
		{
			name: "without pod IP",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Status.PodInfo.PodIP = ""
			},
			expectedState:      agentsv1alpha1.SandboxStateRunning,
			expectedRouteState: agentsv1alpha1.SandboxStateCreating,
		},
		{
			name: "with IPv6 pod IP",
			mutate: func(sandbox *agentsv1alpha1.Sandbox) {
				sandbox.Status.PodInfo.PodIP = "2001:db8::10"
			},
			expectedState: agentsv1alpha1.SandboxStateRunning,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := completeSandbox()
			if tt.mutate != nil {
				tt.mutate(sandbox)
			}

			transformed, err := projectSandboxForGatewayCache(sandbox)
			require.NoError(t, err)
			projected := transformed.(*agentsv1alpha1.Sandbox)

			originalState, originalReason := utils.GetSandboxState(sandbox)
			projectedState, projectedReason := utils.GetSandboxState(projected)
			assert.Equal(t, tt.expectedState, originalState)
			assert.Equal(t, originalState, projectedState)
			assert.Equal(t, originalReason, projectedReason)
			originalRoute := proxyutils.DefaultGetRouteFunc(sandbox)
			projectedRoute := proxyutils.DefaultGetRouteFunc(projected)
			assert.Equal(t, originalRoute, projectedRoute)
			expectedRouteState := tt.expectedRouteState
			if expectedRouteState == "" {
				expectedRouteState = tt.expectedState
			}
			assert.Equal(t, expectedRouteState, originalRoute.State)
		})
	}
}

func TestProjectSandboxForGatewayCacheDropsUnusedFields(t *testing.T) {
	sandbox := completeSandbox()

	transformed, err := projectSandboxForGatewayCache(sandbox)
	require.NoError(t, err)
	projected := transformed.(*agentsv1alpha1.Sandbox)

	assert.Equal(t, sandbox.TypeMeta, projected.TypeMeta)
	assert.Equal(t, sandbox.Namespace, projected.Namespace)
	assert.Equal(t, sandbox.Name, projected.Name)
	assert.Equal(t, sandbox.UID, projected.UID)
	assert.Equal(t, sandbox.ResourceVersion, projected.ResourceVersion)
	assert.Equal(t, sandbox.OwnerReferences, projected.OwnerReferences)
	assert.Equal(t, map[string]string{
		agentsv1alpha1.AnnotationOwner:              "owner-a",
		agentsv1alpha1.AnnotationRuntimeAccessToken: "runtime-token",
		identity.AnnotationEnableJwtAuth:            agentsv1alpha1.True,
	}, projected.Annotations)
	assert.Equal(t, sandbox.Spec.Paused, projected.Spec.Paused)
	assert.Equal(t, sandbox.Spec.ShutdownTime, projected.Spec.ShutdownTime)
	assert.Equal(t, sandbox.Status.Phase, projected.Status.Phase)
	assert.Equal(t, []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}, projected.Status.Conditions)
	assert.Equal(t, sandbox.Status.PodInfo.PodIP, projected.Status.PodInfo.PodIP)

	assert.Nil(t, projected.Labels)
	assert.Nil(t, projected.Finalizers)
	assert.Nil(t, projected.ManagedFields)
	assert.Nil(t, projected.Spec.Template)
	assert.Nil(t, projected.Spec.VolumeClaimTemplates)
	assert.Empty(t, projected.Spec.PersistentContents)
	assert.Empty(t, projected.Status.Message)
	assert.Empty(t, projected.Status.PodInfo.Annotations)
	assert.Empty(t, projected.Status.PodInfo.Labels)
	assert.Empty(t, projected.Status.PodInfo.NodeName)
	assert.Empty(t, projected.Status.PodInfo.PodUID)
	assert.Empty(t, projected.Status.NodeName)
	assert.Empty(t, projected.Status.SandboxIp)
	assert.Empty(t, projected.Status.UpdateRevision)
}

func TestProjectSandboxForGatewayCacheCopiesReferencedData(t *testing.T) {
	sandbox := completeSandbox()
	deletionTime := metav1.NewTime(time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC))
	sandbox.DeletionTimestamp = &deletionTime
	expectedDeletionTime := deletionTime.DeepCopy()

	transformed, err := projectSandboxForGatewayCache(sandbox)
	require.NoError(t, err)
	projected := transformed.(*agentsv1alpha1.Sandbox)

	require.NotSame(t, sandbox.DeletionTimestamp, projected.DeletionTimestamp)
	require.NotSame(t, sandbox.Spec.ShutdownTime, projected.Spec.ShutdownTime)
	require.NotSame(t, sandbox.OwnerReferences[0].Controller, projected.OwnerReferences[0].Controller)

	sandbox.Annotations[agentsv1alpha1.AnnotationOwner] = "changed"
	sandbox.Status.Conditions[0].Status = metav1.ConditionFalse
	sandbox.OwnerReferences[0].Kind = "Changed"
	*sandbox.OwnerReferences[0].Controller = false
	sandbox.DeletionTimestamp.Time = sandbox.DeletionTimestamp.Add(time.Hour)
	sandbox.Spec.ShutdownTime.Time = sandbox.Spec.ShutdownTime.Add(time.Hour)

	assert.Equal(t, "owner-a", projected.Annotations[agentsv1alpha1.AnnotationOwner])
	assert.Equal(t, metav1.ConditionTrue, projected.Status.Conditions[0].Status)
	assert.Equal(t, "OwnerKind", projected.OwnerReferences[0].Kind)
	assert.True(t, *projected.OwnerReferences[0].Controller)
	assert.Equal(t, expectedDeletionTime, projected.DeletionTimestamp)
	assert.NotEqual(t, sandbox.Spec.ShutdownTime, projected.Spec.ShutdownTime)
}

func TestProjectSandboxForGatewayCacheIsIdempotent(t *testing.T) {
	first, err := projectSandboxForGatewayCache(completeSandbox())
	require.NoError(t, err)
	second, err := projectSandboxForGatewayCache(first)
	require.NoError(t, err)

	assert.Equal(t, first, second)
}

func TestProjectSandboxForGatewayCachePassesThroughOtherTypes(t *testing.T) {
	pod := &corev1.Pod{}

	transformed, err := projectSandboxForGatewayCache(pod)
	require.NoError(t, err)

	assert.Same(t, pod, transformed)
}

func completeSandbox() *agentsv1alpha1.Sandbox {
	controller := true
	shutdownTime := metav1.NewTime(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	return &agentsv1alpha1.Sandbox{
		TypeMeta: metav1.TypeMeta{
			APIVersion: agentsv1alpha1.GroupVersion.String(),
			Kind:       "Sandbox",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "sandbox-a",
			UID:             types.UID("sandbox-uid"),
			ResourceVersion: "123",
			Labels:          map[string]string{"large-label": "unused"},
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner:                "owner-a",
				agentsv1alpha1.AnnotationRuntimeAccessToken:   "runtime-token",
				identity.AnnotationEnableJwtAuth:              agentsv1alpha1.True,
				"agents.kruise.io/unrelated-large-annotation": "unused",
			},
			Finalizers: []string{"unused-finalizer"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "example.io/v1",
				Kind:       "OwnerKind",
				Name:       "owner-a",
				UID:        types.UID("owner-uid"),
				Controller: &controller,
			}},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "unused-manager"}},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			Paused:             false,
			PersistentContents: []string{agentsv1alpha1.PersistentContentFilesystem},
			ShutdownTime:       &shutdownTime,
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "large-container", Image: "example/image:latest"}},
					},
				},
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{}},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			ObservedGeneration: 10,
			Phase:              agentsv1alpha1.SandboxRunning,
			Message:            "unused status message",
			Conditions: []metav1.Condition{
				{
					Type:    string(agentsv1alpha1.SandboxConditionReady),
					Status:  metav1.ConditionTrue,
					Reason:  "unused reason",
					Message: "unused condition message",
				},
				{
					Type:   string(agentsv1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionFalse,
				},
			},
			PodInfo: agentsv1alpha1.PodInfo{
				Annotations: map[string]string{"unused": "annotation"},
				Labels:      map[string]string{"unused": "label"},
				NodeName:    "unused-node",
				PodIP:       "10.0.0.10",
				PodUID:      types.UID("unused-pod-uid"),
			},
			NodeName:       "unused-node",
			SandboxIp:      "unused-sandbox-ip",
			UpdateRevision: "unused-revision",
		},
	}
}
