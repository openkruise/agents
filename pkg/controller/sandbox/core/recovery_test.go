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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

const recoveryTestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func recoveryTestSandbox(t *testing.T) *agentsv1alpha1.Sandbox {
	t.Helper()
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "recover", Namespace: "default", UID: "sandbox-uid"},
		Spec: agentsv1alpha1.SandboxSpec{
			PauseStrategy:  &agentsv1alpha1.PauseStrategy{Type: agentsv1alpha1.PauseStrategyStop},
			RecoveryPolicy: &agentsv1alpha1.SandboxRecoveryPolicy{Type: "OnEvicted", BackoffSeconds: 1},
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "test:v1"}}},
			}},
		},
		Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRecovering},
	}
	hash, err := RecoveryTemplateHash(box)
	require.NoError(t, err)
	now := metav1.Now()
	box.Status.Recovery = &agentsv1alpha1.SandboxRecoveryStatus{PodUID: "old-pod", NodeName: "node", TemplateHash: hash, StartedAt: &now}
	box.Status.Recovery.Images = map[string]string{"containers/main": "docker.io/library/test@" + recoveryTestDigest}
	t.Cleanup(func() { ScaleExpectation.DeleteExpectations(GetControllerKey(box)) })
	return box
}

func recoveryTestPod(box *agentsv1alpha1.Sandbox, replacement bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: box.Name, Namespace: box.Namespace, UID: "old-pod",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(box, sandboxControllerKind)}},
		Spec:   corev1.PodSpec{NodeName: "node", Containers: []corev1.Container{{Name: "main", Image: "test:v1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"},
	}
	if replacement {
		pod.UID = "new-pod"
		pod.Annotations = map[string]string{recoveryAttemptAnnotation: recoveryAttemptID(&box.Status)}
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.2",
			ContainerStatuses: []corev1.ContainerStatus{{Name: "main", ImageID: "docker.io/library/test@" + recoveryTestDigest}},
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	}
	return pod
}

func TestEnsureSandboxRecovered(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*agentsv1alpha1.Sandbox, **corev1.Pod, *corev1.Node)
		gateDisabled bool
		initError    bool
		wantPhase    agentsv1alpha1.SandboxPhase
		wantCreate   bool
		wantDelete   bool
		wantInit     int
	}{
		{name: "delete evicted source with UID precondition", wantDelete: true},
		{name: "wait for actual deletion", mutate: func(_ *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) {
			now := metav1.Now()
			(*p).DeletionTimestamp = &now
			(*p).Finalizers = []string{"test/finalizer"}
		}},
		{name: "reserve attempt without creating", mutate: func(_ *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) { *p = nil }},
		{name: "create reserved attempt", wantCreate: true, mutate: func(b *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) {
			*p = nil
			b.Status.Recovery.Attempts = 1
			past := metav1.NewTime(time.Now().Add(-time.Minute))
			b.Status.Recovery.RetryAfter = &past
		}},
		{name: "wait through backoff", mutate: func(b *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) {
			*p = nil
			b.Status.Recovery.Attempts = 1
			future := metav1.NewTime(time.Now().Add(time.Minute))
			b.Status.Recovery.RetryAfter = &future
		}},
		{name: "wait for pressure to clear", mutate: func(b *agentsv1alpha1.Sandbox, p **corev1.Pod, n *corev1.Node) {
			*p = nil
			b.Status.Recovery.Attempts = 1
			past := metav1.NewTime(time.Now().Add(-time.Minute))
			b.Status.Recovery.RetryAfter = &past
			n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue})
		}},
		{name: "record replacement UID before initialization", mutate: func(b *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) { *p = recoveryTestPod(b, true) }},
		{name: "publish only after new runtime initialization", wantPhase: agentsv1alpha1.SandboxRunning, wantInit: 1,
			mutate: func(b *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) {
				*p = recoveryTestPod(b, true)
				b.Status.Recovery.ReplacementPodUID = (*p).UID
			}},
		{name: "initializer failure retains identity but withdraws endpoint", wantInit: 1, initError: true,
			mutate: func(b *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) {
				*p = recoveryTestPod(b, true)
				b.Status.Recovery.ReplacementPodUID = (*p).UID
			}},
		{name: "repeated eviction preserves budget and deadline", mutate: func(b *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) {
			*p = recoveryTestPod(b, true)
			b.Status.Recovery.ReplacementPodUID = (*p).UID
			(*p).Status.Phase = corev1.PodFailed
			(*p).Status.Reason = "Evicted"
		}},
		{name: "ordinary replacement failure is terminal", wantPhase: agentsv1alpha1.SandboxFailed,
			mutate: func(b *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) {
				*p = recoveryTestPod(b, true)
				b.Status.Recovery.ReplacementPodUID = (*p).UID
				(*p).Status.Phase = corev1.PodFailed
			}},
		{name: "unknown Pod cannot be adopted", wantPhase: agentsv1alpha1.SandboxFailed,
			mutate: func(b *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) {
				*p = recoveryTestPod(b, true)
				(*p).Annotations = nil
			}},
		{name: "foreign owner cannot be deleted", wantPhase: agentsv1alpha1.SandboxFailed,
			mutate: func(_ *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) {
				(*p).OwnerReferences[0].UID = "other-sandbox"
			}},
		{name: "missing replacement is not another eviction", wantPhase: agentsv1alpha1.SandboxFailed,
			mutate: func(b *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) {
				*p = nil
				b.Status.Recovery.ReplacementPodUID = "lost-pod"
			}},
		{name: "budget exhausted", wantPhase: agentsv1alpha1.SandboxFailed,
			mutate: func(b *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) {
				*p = nil
				b.Status.Recovery.Attempts = 3
			}},
		{name: "deadline exhausted", wantPhase: agentsv1alpha1.SandboxFailed,
			mutate: func(b *agentsv1alpha1.Sandbox, _ **corev1.Pod, _ *corev1.Node) {
				past := metav1.NewTime(time.Now().Add(-time.Hour))
				b.Status.Recovery.StartedAt = &past
			}},
		{name: "template changes do not upgrade during recovery", wantPhase: agentsv1alpha1.SandboxFailed,
			mutate: func(b *agentsv1alpha1.Sandbox, _ **corev1.Pod, _ *corev1.Node) {
				b.Spec.Template.Spec.Containers[0].Image = "test:v2"
			}},
		{name: "admission cannot silently change recovered image", wantPhase: agentsv1alpha1.SandboxFailed,
			mutate: func(b *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) {
				*p = recoveryTestPod(b, true)
				b.Status.Recovery.ReplacementPodUID = (*p).UID
				(*p).Status.ContainerStatuses[0].ImageID = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			}},
		{name: "feature disabled", gateDisabled: true, wantPhase: agentsv1alpha1.SandboxFailed},
		{name: "pause deletes source first", wantDelete: true,
			mutate: func(b *agentsv1alpha1.Sandbox, _ **corev1.Pod, _ *corev1.Node) { b.Spec.Paused = true }},
		{name: "pause completes without recreating", wantPhase: agentsv1alpha1.SandboxPaused,
			mutate: func(b *agentsv1alpha1.Sandbox, p **corev1.Pod, _ *corev1.Node) { b.Spec.Paused = true; *p = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SandboxEvictionRecoveryGate, !tt.gateDisabled)
			box := recoveryTestSandbox(t)
			pod := recoveryTestPod(box, false)
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
			if tt.mutate != nil {
				tt.mutate(box, &pod, node)
			}
			objects := []client.Object{node}
			if pod != nil {
				objects = append(objects, pod)
			}
			deletes := 0
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					options := &client.DeleteOptions{}
					options.ApplyOptions(opts)
					require.NotNil(t, options.Preconditions)
					require.NotNil(t, options.Preconditions.UID)
					assert.Equal(t, pod.UID, *options.Preconditions.UID)
					assert.Nil(t, options.GracePeriodSeconds, "must not force delete")
					deletes++
					return c.Delete(ctx, obj, opts...)
				},
			}).Build()
			initializer := &mockSandboxInitializer{}
			if tt.initError {
				initializer.err = fmt.Errorf("runtime unavailable")
			}
			recorder := record.NewFakeRecorder(20)
			control := &commonControl{Client: cli, podControl: NewPodControl(cli, recorder, GeneratePodFromSandbox), initializer: initializer, syncStatusFromPod: defaultCommonSyncStatusFromPod}
			status := box.Status.DeepCopy()
			status.PodInfo.PodIP, status.SandboxIp = "10.0.0.1", "10.0.0.1"
			_, err := control.EnsureSandboxRecovered(t.Context(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: status})
			if tt.initError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			wantPhase := tt.wantPhase
			if wantPhase == "" {
				wantPhase = agentsv1alpha1.SandboxRecovering
			}
			assert.Equal(t, wantPhase, status.Phase)
			assert.Equal(t, tt.wantDelete, deletes > 0)
			assert.Equal(t, tt.wantInit, initializer.called)
			if wantPhase == agentsv1alpha1.SandboxRunning {
				assert.Equal(t, "10.0.0.2", status.PodInfo.PodIP)
				assert.Equal(t, "10.0.0.2", initializer.initializedStatus.PodInfo.PodIP)
			} else {
				assert.Empty(t, status.PodInfo.PodIP)
				assert.Empty(t, status.SandboxIp)
				assert.False(t, utils.IsSandboxReady(&agentsv1alpha1.Sandbox{Status: *status}))
			}
			if tt.wantCreate {
				created := &corev1.Pod{}
				require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(box), created))
				assert.Equal(t, recoveryAttemptID(status), created.Annotations[recoveryAttemptAnnotation])
				// Replaying the same persisted reservation against a delayed cache
				// sees AlreadyExists without consuming another generation.
				_, err = control.EnsureSandboxRecovered(t.Context(), EnsureFuncArgs{Box: box, NewStatus: status})
				require.NoError(t, err)
				assert.EqualValues(t, 1, status.Recovery.Attempts)
			} else if pod == nil {
				assert.True(t, apierrors.IsNotFound(cli.Get(t.Context(), client.ObjectKeyFromObject(box), &corev1.Pod{})))
			}
		})
	}
}

func TestRecoveryVolumes(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*corev1.PersistentVolumeClaim, *corev1.PersistentVolume)
		missing   bool
		wantError bool
	}{
		{name: "original binding"},
		{name: "missing original PVC", missing: true, wantError: true},
		{name: "same name replacement PVC", wantError: true, mutate: func(p *corev1.PersistentVolumeClaim, v *corev1.PersistentVolume) {
			p.UID = "replacement"
			v.Spec.ClaimRef.UID = p.UID
		}},
		{name: "same name replacement PV", wantError: true, mutate: func(_ *corev1.PersistentVolumeClaim, v *corev1.PersistentVolume) { v.UID = "replacement" }},
		{name: "wrong PVC owner", wantError: true, mutate: func(p *corev1.PersistentVolumeClaim, _ *corev1.PersistentVolume) { p.OwnerReferences[0].UID = "other" }},
		{name: "PVC not bound", wantError: true, mutate: func(p *corev1.PersistentVolumeClaim, _ *corev1.PersistentVolume) {
			p.Status.Phase = corev1.ClaimPending
		}},
		{name: "PV rebound", wantError: true, mutate: func(_ *corev1.PersistentVolumeClaim, v *corev1.PersistentVolume) { v.Spec.ClaimRef.UID = "other" }},
		{name: "PVC deleting", wantError: true, mutate: func(p *corev1.PersistentVolumeClaim, _ *corev1.PersistentVolume) {
			now := metav1.Now()
			p.DeletionTimestamp = &now
			p.Finalizers = []string{"test/hold"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := recoveryTestSandbox(t)
			box.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"}}}
			box.Status.Recovery.Volumes = []agentsv1alpha1.SandboxRecoveryVolume{{Name: "data-recover", UID: "pvc-uid", VolumeName: "pv", VolumeUID: "pv-uid"}}
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-recover", Namespace: box.Namespace, UID: "pvc-uid", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(box, sandboxControllerKind)}}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
			pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv", UID: "pv-uid"}, Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{Name: pvc.Name, Namespace: pvc.Namespace, UID: pvc.UID}}}
			if tt.mutate != nil {
				tt.mutate(pvc, pv)
			}
			objects := []client.Object{pv}
			if !tt.missing {
				objects = append(objects, pvc)
			}
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			volumes, err := RecoveryVolumes(t.Context(), cli, box)
			if tt.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, box.Status.Recovery.Volumes, volumes)
			}
			if tt.missing {
				assert.True(t, apierrors.IsNotFound(cli.Get(t.Context(), client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{})))
			}
		})
	}
}
