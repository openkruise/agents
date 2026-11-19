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

package sandbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

func TestReconcileEvictionRecovery(t *testing.T) {
	tests := []struct {
		name            string
		mutate          func(*agentsv1alpha1.Sandbox, **corev1.Pod)
		gateDisabled    bool
		failStatusWrite bool
		wantPhase       agentsv1alpha1.SandboxPhase
		wantDeleted     bool
	}{
		{name: "opted in eviction records intent", wantPhase: agentsv1alpha1.SandboxRecovering},
		{name: "feature disabled keeps terminal semantics", gateDisabled: true, wantPhase: agentsv1alpha1.SandboxFailed},
		{name: "omitted policy keeps terminal semantics", wantPhase: agentsv1alpha1.SandboxFailed, mutate: func(b *agentsv1alpha1.Sandbox, _ **corev1.Pod) { b.Spec.RecoveryPolicy = nil }},
		{name: "ordinary application failure", wantPhase: agentsv1alpha1.SandboxFailed, mutate: func(_ *agentsv1alpha1.Sandbox, p **corev1.Pod) { (*p).Status.Reason = "Error" }},
		{name: "completed workload", wantPhase: agentsv1alpha1.SandboxSucceeded, mutate: func(_ *agentsv1alpha1.Sandbox, p **corev1.Pod) { (*p).Status.Phase = corev1.PodSucceeded }},
		{name: "Pod missing is not eviction", wantPhase: agentsv1alpha1.SandboxFailed, mutate: func(_ *agentsv1alpha1.Sandbox, p **corev1.Pod) { *p = nil }},
		{name: "no baseline cannot adopt old volumes", wantPhase: agentsv1alpha1.SandboxFailed, mutate: func(b *agentsv1alpha1.Sandbox, _ **corev1.Pod) { b.Status.Recovery = nil }},
		{name: "Failed is not resurrected", wantPhase: agentsv1alpha1.SandboxFailed, mutate: func(b *agentsv1alpha1.Sandbox, _ **corev1.Pod) { b.Status.Phase = agentsv1alpha1.SandboxFailed }},
		{name: "foreign Pod owner", wantPhase: agentsv1alpha1.SandboxFailed, mutate: func(_ *agentsv1alpha1.Sandbox, p **corev1.Pod) { (*p).OwnerReferences[0].UID = "other" }},
		{name: "template changed", wantPhase: agentsv1alpha1.SandboxFailed, mutate: func(b *agentsv1alpha1.Sandbox, _ **corev1.Pod) { b.Spec.Template.Spec.Containers[0].Image = "test:v2" }},
		{name: "failed intent write never deletes the Pod", failStatusWrite: true, wantPhase: agentsv1alpha1.SandboxRunning},
		{name: "shutdown wins over eviction", wantDeleted: true, mutate: func(b *agentsv1alpha1.Sandbox, _ **corev1.Pod) {
			past := metav1.NewTime(time.Now().Add(-time.Minute))
			b.Spec.ShutdownTime = &past
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SandboxEvictionRecoveryGate, !tt.gateDisabled)
			scheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(scheme))
			require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "recover", Namespace: "default", UID: "sandbox-uid"},
				Spec:       agentsv1alpha1.SandboxSpec{PauseStrategy: &agentsv1alpha1.PauseStrategy{Type: agentsv1alpha1.PauseStrategyStop}, RecoveryPolicy: &agentsv1alpha1.SandboxRecoveryPolicy{Type: "OnEvicted"}, EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "test:v1"}}}}}},
				Status:     agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning, PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.1", PodUID: "old-pod"}, Conditions: []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "PodReady", LastTransitionTime: metav1.Now()}}},
			}
			hash, err := core.RecoveryTemplateHash(box)
			require.NoError(t, err)
			box.Status.Recovery = &agentsv1alpha1.SandboxRecoveryStatus{PodUID: "old-pod", NodeName: "node", TemplateHash: hash}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: box.Name, Namespace: box.Namespace, UID: "old-pod", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(box, sandboxControllerKind)}}, Spec: corev1.PodSpec{NodeName: "node"}, Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}}
			if tt.mutate != nil {
				tt.mutate(box, &pod)
			}
			t.Cleanup(func() {
				core.ScaleExpectation.DeleteExpectations(utils.GetControllerKey(box))
				core.ResourceVersionExpectations.Delete(box)
			})
			objects := []client.Object{box}
			if pod != nil {
				objects = append(objects, pod)
			}
			cli := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if tt.failStatusWrite {
						return fmt.Errorf("status unavailable")
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			}).Build()
			r := &SandboxReconciler{Client: cli, Scheme: scheme, recorder: record.NewFakeRecorder(20)}
			_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(box)})
			if tt.failStatusWrite {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			got := &agentsv1alpha1.Sandbox{}
			err = cli.Get(t.Context(), client.ObjectKeyFromObject(box), got)
			if tt.wantDeleted {
				require.True(t, apierrors.IsNotFound(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPhase, got.Status.Phase)
			if !tt.failStatusWrite {
				assert.False(t, utils.IsSandboxReady(got))
				assert.Empty(t, got.Status.PodInfo.PodIP)
			}
			if pod != nil {
				require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{}), "intent write must precede deletion")
			}
			if got.Status.Phase == agentsv1alpha1.SandboxRecovering {
				require.NotNil(t, got.Status.Recovery.StartedAt)
				assert.Zero(t, got.Status.Recovery.Attempts)
			}
		})
	}
}

func TestRecoveryDoesNotReprovisionMissingPVC(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SandboxEvictionRecoveryGate, true)
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	now := metav1.Now()
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-volume", Namespace: "default", UID: "sandbox-uid"},
		Spec:       agentsv1alpha1.SandboxSpec{PauseStrategy: &agentsv1alpha1.PauseStrategy{Type: agentsv1alpha1.PauseStrategyStop}, RecoveryPolicy: &agentsv1alpha1.SandboxRecoveryPolicy{Type: "OnEvicted"}, EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{Template: &corev1.PodTemplateSpec{}, VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"}}}}},
		Status:     agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRecovering, Recovery: &agentsv1alpha1.SandboxRecoveryStatus{PodUID: "old", NodeName: "node", StartedAt: &now, Volumes: []agentsv1alpha1.SandboxRecoveryVolume{{Name: "data-missing-volume", UID: "original", VolumeName: "pv", VolumeUID: "pv-uid"}}}},
	}
	hash, err := core.RecoveryTemplateHash(box)
	require.NoError(t, err)
	box.Status.Recovery.TemplateHash = hash
	t.Cleanup(func() {
		core.ScaleExpectation.DeleteExpectations(utils.GetControllerKey(box))
		core.ResourceVersionExpectations.Delete(box)
	})
	cli := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).WithObjects(box).Build()
	recorder := record.NewFakeRecorder(20)
	r := &SandboxReconciler{Client: cli, Scheme: scheme, recorder: recorder, controls: core.NewSandboxControl(core.SandboxControlArgs{Client: cli, Recorder: recorder})}
	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(box)})
	require.NoError(t, err)
	assert.Positive(t, result.RequeueAfter)
	require.True(t, apierrors.IsNotFound(cli.Get(t.Context(), client.ObjectKey{Namespace: box.Namespace, Name: "data-missing-volume"}, &corev1.PersistentVolumeClaim{})))
	require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(box), box))
	assert.Equal(t, agentsv1alpha1.SandboxRecovering, box.Status.Phase)
	assert.Contains(t, box.Status.Message, "original PVC")
}

func TestRecoveryStatusPatch(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%t", stale), func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
			now := metav1.Now()
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "patch", Namespace: "default", UID: "sandbox-uid"},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRecovering, SandboxIp: "10.0.0.1",
					PodInfo:  agentsv1alpha1.PodInfo{PodIP: "10.0.0.1"},
					Recovery: &agentsv1alpha1.SandboxRecoveryStatus{StartedAt: &now, RetryAfter: &now, ReplacementPodUID: "replacement", Attempts: 1},
				},
			}
			cli := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(box).WithObjects(box).Build()
			require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(box), box))
			t.Cleanup(func() { core.ResourceVersionExpectations.Delete(box) })
			if stale {
				concurrent := box.DeepCopy()
				concurrent.Spec.Paused = true
				require.NoError(t, cli.Update(t.Context(), concurrent))
			}
			status := box.Status.DeepCopy()
			status.Phase = agentsv1alpha1.SandboxRunning
			status.SandboxIp, status.PodInfo.PodIP = "", ""
			status.Recovery.StartedAt, status.Recovery.RetryAfter = nil, nil
			status.Recovery.ReplacementPodUID = ""
			r := &SandboxReconciler{Client: cli}
			err := r.updateSandboxStatus(t.Context(), *status, box)
			got := &agentsv1alpha1.Sandbox{}
			require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(box), got))
			if stale {
				require.True(t, apierrors.IsConflict(err), "stale recovery must not publish readiness")
				assert.Equal(t, agentsv1alpha1.SandboxRecovering, got.Status.Phase)
				assert.NotNil(t, got.Status.Recovery.StartedAt)
				assert.True(t, got.Spec.Paused)
			} else {
				require.NoError(t, err)
				assert.Equal(t, agentsv1alpha1.SandboxRunning, got.Status.Phase)
				assert.Empty(t, got.Status.SandboxIp)
				assert.Empty(t, got.Status.PodInfo.PodIP)
				assert.Nil(t, got.Status.Recovery.StartedAt)
				assert.Nil(t, got.Status.Recovery.RetryAfter)
				assert.Empty(t, got.Status.Recovery.ReplacementPodUID)
				assert.EqualValues(t, 1, got.Status.Recovery.Attempts)
			}
		})
	}
}

func TestRecoveryBudgetHealthyInterval(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SandboxEvictionRecoveryGate, true)
	for _, tt := range []struct {
		name         string
		healthyFor   time.Duration
		ready        bool
		wantAttempts int32
	}{
		{name: "brief readiness retains budget", healthyFor: time.Second, ready: true, wantAttempts: 2},
		{name: "sustained readiness resets budget", healthyFor: 2 * time.Minute, ready: true},
		{name: "unready breaks healthy interval", healthyFor: 2 * time.Minute, wantAttempts: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "budget", Namespace: "default", UID: "sandbox-uid"},
				Spec: agentsv1alpha1.SandboxSpec{
					PauseStrategy:           &agentsv1alpha1.PauseStrategy{Type: agentsv1alpha1.PauseStrategyStop},
					RecoveryPolicy:          &agentsv1alpha1.SandboxRecoveryPolicy{Type: "OnEvicted", HealthyResetSeconds: 60},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{Template: &corev1.PodTemplateSpec{}},
				},
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
			}
			since := metav1.NewTime(time.Now().Add(-tt.healthyFor))
			box.Status.Recovery = &agentsv1alpha1.SandboxRecoveryStatus{PodUID: "pod", Attempts: 2, HealthySince: &since}
			ready := metav1.ConditionFalse
			if tt.ready {
				ready = metav1.ConditionTrue
			}
			utils.SetSandboxCondition(&box.Status, metav1.Condition{Type: string(agentsv1alpha1.SandboxConditionReady), Status: ready, Reason: "Test", LastTransitionTime: metav1.Now()})
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{UID: "pod", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(box, sandboxControllerKind)}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "test:v1"}}},
				Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "main", ImageID: "test@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}},
			}
			r := &SandboxReconciler{Client: fake.NewClientBuilder().Build()}
			status := box.Status.DeepCopy()
			wait, err := r.recordRecoveryBaseline(t.Context(), core.EnsureFuncArgs{Box: box, Pod: pod, NewStatus: status})
			require.NoError(t, err)
			assert.Equal(t, tt.wantAttempts, status.Recovery.Attempts)
			if !tt.ready {
				assert.Nil(t, status.Recovery.HealthySince)
			} else if tt.wantAttempts > 0 {
				assert.Positive(t, wait)
			}
		})
	}
}
