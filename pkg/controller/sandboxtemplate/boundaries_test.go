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

package sandboxtemplate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func TestExecutionIdentityFences(t *testing.T) {
	for _, tt := range []struct {
		name           string
		mutate         func(*api.SandboxTemplate, *api.Sandbox)
		missingVersion bool
		missingSandbox bool
		canceled       bool
		allowed        bool
	}{
		{name: "original identities", allowed: true},
		{name: "missing Version", missingVersion: true},
		{name: "missing Sandbox", missingSandbox: true},
		{name: "cancellation", canceled: true},
		{name: "replaced Version", mutate: func(v *api.SandboxTemplate, _ *api.Sandbox) { v.UID = "replacement" }},
		{name: "deleting Version", mutate: func(v *api.SandboxTemplate, _ *api.Sandbox) {
			v.DeletionTimestamp = ptr.To(metav1.Now())
			v.Finalizers = []string{"test/retain"}
		}},
		{name: "failed Version", mutate: func(v *api.SandboxTemplate, _ *api.Sandbox) {
			v.Status.Phase = api.SandboxTemplatePhaseFailed
		}},
		{name: "absent logical name", mutate: func(v *api.SandboxTemplate, _ *api.Sandbox) { v.Labels = nil }},
		{name: "removed logical name", mutate: func(v *api.SandboxTemplate, _ *api.Sandbox) { delete(v.Labels, api.LabelTemplateName) }},
		{name: "empty logical name", mutate: func(v *api.SandboxTemplate, _ *api.Sandbox) { v.Labels[api.LabelTemplateName] = "" }},
		{name: "replaced Sandbox", mutate: func(_ *api.SandboxTemplate, box *api.Sandbox) { box.UID = "replacement" }},
		{name: "removed Version ownership", mutate: func(_ *api.SandboxTemplate, box *api.Sandbox) { box.OwnerReferences = nil }},
		{name: "deleting Sandbox", mutate: func(_ *api.SandboxTemplate, box *api.Sandbox) {
			box.DeletionTimestamp = ptr.To(metav1.Now())
			box.Finalizers = []string{"test/retain"}
		}},
		{name: "paused Sandbox", mutate: func(_ *api.SandboxTemplate, box *api.Sandbox) { box.Spec.Paused = true }},
		{name: "terminated Sandbox", mutate: func(_ *api.SandboxTemplate, box *api.Sandbox) {
			box.Status.Phase = api.SandboxSucceeded
		}},
		{name: "replaced Pod record", mutate: func(_ *api.SandboxTemplate, box *api.Sandbox) {
			box.Status.PodInfo.PodUID = "replacement"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, version, box, pod := executionFixture(t)
			if tt.mutate != nil {
				tt.mutate(version, box)
			}
			objects := []client.Object{pod}
			if !tt.missingVersion {
				objects = append(objects, version)
			}
			if !tt.missingSandbox {
				objects = append(objects, box)
			}
			r := modelReconciler(t, objects...)
			session := &executionSession{versionName: version.Name, versionUID: "version-uid", namespace: "default", sandboxUID: "sandbox-uid", podUID: "pod-uid"}
			ctx := t.Context()
			if tt.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			current, err := r.readExecutionSandbox(ctx, session)
			if tt.allowed {
				require.NoError(t, err)
				require.Equal(t, box.UID, current.UID)
			} else {
				require.Error(t, err)
				require.Nil(t, current)
			}
		})
	}
}

func TestExecutionSnapshotBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name       string
		mutate     func(*corev1.Pod)
		missingPod bool
		unboundPod bool
		interrupt  bool
	}{
		{name: "waiting for first Pod", missingPod: true, unboundPod: true},
		{name: "lost original Pod", missingPod: true, interrupt: true},
		{name: "replaced Pod", interrupt: true, mutate: func(p *corev1.Pod) { p.UID = "replacement" }},
		{name: "wrong Sandbox owner", interrupt: true, mutate: func(p *corev1.Pod) { p.OwnerReferences[0].UID = "replacement" }},
		{name: "completed Pod", interrupt: true, mutate: func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }},
		{name: "container not ready", mutate: func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Ready = false }},
		{name: "container missing", mutate: func(p *corev1.Pod) { p.Status.ContainerStatuses = nil }},
		{name: "revision still converging", mutate: func(p *corev1.Pod) { p.Labels[api.PodLabelTemplateHash] = "old" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, version, box, pod := executionFixture(t)
			if tt.mutate != nil {
				tt.mutate(pod)
			}
			objects := []client.Object{version, box}
			if !tt.missingPod {
				objects = append(objects, pod)
			}
			r := modelReconciler(t, objects...)
			session := &executionSession{versionName: version.Name, versionUID: version.UID, namespace: version.Namespace, sandboxUID: box.UID, podUID: "pod-uid"}
			if tt.unboundPod {
				session.podUID = ""
			}
			_, ready, err := r.executionSnapshot(t.Context(), session, version.Spec.Template.Spec.Containers[0].Image, box.Status.UpdateRevision)
			require.False(t, ready)
			if tt.interrupt {
				require.ErrorIs(t, err, errExecutionInterrupted)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestExecutionStatusIdentityAndTerminalStates(t *testing.T) {
	for _, tt := range []struct {
		name      string
		mutate    func(*api.SandboxTemplate)
		interrupt bool
	}{
		{name: "replaced UID", interrupt: true, mutate: func(v *api.SandboxTemplate) { v.UID = "replacement" }},
		{name: "changed generation", interrupt: true, mutate: func(v *api.SandboxTemplate) { v.Generation++ }},
		{name: "terminal failure is retained", mutate: func(v *api.SandboxTemplate) {
			v.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseFailed, ObservedGeneration: v.Generation}
		}},
		{name: "terminal success is retained", mutate: func(v *api.SandboxTemplate) {
			v.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseReady, ObservedGeneration: v.Generation}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			version := modelFixtures()
			expected := version.DeepCopy()
			tt.mutate(version)
			r := modelReconciler(t, version)
			err := r.executionStatus(t.Context(), expected, api.SandboxTemplatePhaseBuilding, metav1.ConditionFalse, buildReasonProvisioned, "known result")
			if tt.interrupt {
				require.ErrorIs(t, err, errExecutionInterrupted)
			} else {
				require.NoError(t, err)
				current := &api.SandboxTemplate{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
				require.Equal(t, version.Status.Phase, current.Status.Phase)
			}
		})
	}
}

func TestBuildCleanupRetainsPublishedArtifacts(t *testing.T) {
	for _, tt := range []struct {
		name                string
		phase               api.SandboxTemplateBuildPhase
		replacement         bool
		wrongOwner          bool
		replaceDuringDelete bool
		keep                bool
	}{
		{name: "Ready retains capture", phase: api.SandboxTemplatePhaseReady, keep: true},
		{name: "Failed removes partial capture", phase: api.SandboxTemplatePhaseFailed},
		{name: "Ready reclaims replacement owned by Version", phase: api.SandboxTemplatePhaseReady, replacement: true, keep: true},
		{name: "Failed reclaims replacement owned by Version", phase: api.SandboxTemplatePhaseFailed, replacement: true},
		{name: "another Version owns the Sandbox", phase: api.SandboxTemplatePhaseReady, wrongOwner: true, keep: true},
		{name: "replacement during deletion is protected", phase: api.SandboxTemplatePhaseReady, replaceDuringDelete: true, keep: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, version, box, _ := executionFixture(t)
			version.Status = api.SandboxTemplateStatus{Phase: tt.phase, ObservedGeneration: version.Generation}
			checkpoint := &api.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: version.Name + "-checkpoint", Namespace: version.Namespace, UID: "checkpoint-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: version.Name, UID: version.UID, Controller: ptr.To(true)}}}}
			if tt.replacement {
				box.UID = "replacement"
			}
			if tt.wrongOwner {
				box.OwnerReferences[0].UID = "other-version"
			}
			r := modelReconciler(t, version, box, checkpoint)
			if tt.replaceDuringDelete {
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if current, ok := obj.(*api.Sandbox); ok {
						replacement := current.DeepCopy()
						require.NoError(t, c.Delete(ctx, current))
						replacement.UID, replacement.ResourceVersion = "replacement", ""
						require.NoError(t, c.Create(ctx, replacement))
					}
					return c.Delete(ctx, obj, opts...)
				}})
			}
			for i := 0; i < 3; i++ {
				_, err := r.reconcileBuildCleanup(t.Context(), version)
				if tt.replaceDuringDelete {
					require.True(t, apierrors.IsConflict(err), "%v", err)
					break
				}
				require.NoError(t, err)
			}
			currentBox := &api.Sandbox{}
			err := r.Get(t.Context(), client.ObjectKeyFromObject(box), currentBox)
			if tt.wrongOwner || tt.replaceDuringDelete {
				require.NoError(t, err)
				if tt.replaceDuringDelete {
					require.EqualValues(t, "replacement", currentBox.UID)
				}
			} else {
				require.True(t, apierrors.IsNotFound(err))
			}
			err = r.Get(t.Context(), client.ObjectKeyFromObject(checkpoint), &api.Checkpoint{})
			if tt.keep {
				require.NoError(t, err)
			} else {
				require.True(t, apierrors.IsNotFound(err))
			}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), &api.SandboxTemplate{}))
		})
	}
}

func TestMissingVersionRequestStopsOnlyTargetWorker(t *testing.T) {
	version := modelFixtures()
	r := modelReconciler(t, version)
	require.True(t, r.NeedLeaderElection())
	current := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
	require.NoError(t, r.Delete(t.Context(), current))
	targetCtx, cancelTarget := context.WithCancel(t.Context())
	defer cancelTarget()
	siblingCtx, cancelSibling := context.WithCancel(t.Context())
	defer cancelSibling()
	r.workers = map[types.UID]*buildWorker{
		version.UID:   {key: client.ObjectKeyFromObject(version), cancel: cancelTarget},
		"sibling-uid": {key: client.ObjectKey{Namespace: version.Namespace, Name: "sibling"}, cancel: cancelSibling},
	}
	result, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.ErrorIs(t, targetCtx.Err(), context.Canceled)
	require.NoError(t, siblingCtx.Err())
}

func TestExecutionIdentityBinding(t *testing.T) {
	for _, tt := range []struct {
		name    string
		change  func(*api.SandboxTemplate, *api.Sandbox)
		allowed bool
	}{
		{name: "bind original Sandbox", allowed: true},
		{name: "replaced Version", change: func(v *api.SandboxTemplate, _ *api.Sandbox) { v.UID = "replacement" }},
		{name: "deleting Version", change: func(v *api.SandboxTemplate, _ *api.Sandbox) {
			v.DeletionTimestamp = ptr.To(metav1.Now())
			v.Finalizers = []string{"test/retain"}
		}},
		{name: "absent logical name", change: func(v *api.SandboxTemplate, _ *api.Sandbox) { v.Labels = nil }},
		{name: "empty logical name", change: func(v *api.SandboxTemplate, _ *api.Sandbox) { v.Labels[api.LabelTemplateName] = "" }},
		{name: "failed Version", change: func(v *api.SandboxTemplate, _ *api.Sandbox) { v.Status.Phase = api.SandboxTemplatePhaseFailed }},
		{name: "replacement execution Sandbox", change: func(_ *api.SandboxTemplate, box *api.Sandbox) { box.UID = "replacement" }},
		{name: "missing Version owner", change: func(_ *api.SandboxTemplate, box *api.Sandbox) { box.OwnerReferences = nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, version, box, pod := executionFixture(t)
			if tt.change != nil {
				tt.change(version, box)
			}
			r := modelReconciler(t, version, box, pod)
			session := &executionSession{versionName: version.Name, versionUID: "version-uid", namespace: "default"}
			err := r.bindExecutionSandbox(t.Context(), session)
			if tt.allowed {
				require.NoError(t, err)
				require.EqualValues(t, "sandbox-uid", session.sandboxUID)
				require.EqualValues(t, "pod-uid", session.podUID)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestExecutionIdentityRecording(t *testing.T) {
	for _, tt := range []struct {
		name      string
		change    func(*api.SandboxTemplate, *executionIdentity)
		interrupt bool
		conflict  bool
		writes    int
	}{
		{name: "record execution container", writes: 1},
		{name: "unchanged identity skips patch", change: func(_ *api.SandboxTemplate, identity *executionIdentity) {
			identity.ContainerID = "containerd://original"
		}},
		{name: "metadata conflict preserves edit", conflict: true, writes: 2},
		{name: "Version replaced", interrupt: true, change: func(v *api.SandboxTemplate, _ *executionIdentity) { v.UID = "replacement" }},
		{name: "Version deleting", interrupt: true, change: func(v *api.SandboxTemplate, _ *executionIdentity) {
			v.DeletionTimestamp = ptr.To(metav1.Now())
			v.Finalizers = []string{"test/retain"}
		}},
		{name: "Version failed", interrupt: true, change: func(v *api.SandboxTemplate, _ *executionIdentity) { v.Status.Phase = api.SandboxTemplatePhaseFailed }},
		{name: "absent logical name", interrupt: true, change: func(v *api.SandboxTemplate, _ *executionIdentity) { v.Labels = nil }},
		{name: "empty logical name", interrupt: true, change: func(v *api.SandboxTemplate, _ *executionIdentity) { v.Labels[api.LabelTemplateName] = "" }},
		{name: "Sandbox replaced", interrupt: true, change: func(_ *api.SandboxTemplate, identity *executionIdentity) { identity.SandboxUID = "replacement" }},
		{name: "Pod replaced", interrupt: true, change: func(_ *api.SandboxTemplate, identity *executionIdentity) { identity.PodUID = "replacement" }},
		{name: "container replaced", interrupt: true, change: func(_ *api.SandboxTemplate, identity *executionIdentity) { identity.ContainerID = "replacement" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, version, box, pod := executionFixture(t)
			identity, err := parseExecutionIdentity(version.Annotations[executionIdentityAnnotation])
			require.NoError(t, err)
			if tt.change != nil {
				tt.change(version, &identity)
			}
			data, err := json.Marshal(identity)
			require.NoError(t, err)
			version.Annotations[executionIdentityAnnotation] = string(data)
			r := modelReconciler(t, version, box, pod)
			base := r.Client.(client.WithWatch)
			reads, writes := 0, 0
			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					if current, ok := obj.(*api.SandboxTemplate); ok && tt.conflict {
						reads++
						if reads == 1 {
							edited := current.DeepCopy()
							edited.Labels["editor"] = "winner"
							return c.Update(ctx, edited)
						}
					}
					return nil
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					require.IsType(t, &api.SandboxTemplate{}, obj)
					writes++
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			session := &executionSession{versionName: version.Name, versionUID: "version-uid", namespace: "default", sandboxUID: "sandbox-uid", podUID: "pod-uid", containerID: "containerd://original"}
			err = r.recordExecutionIdentity(t.Context(), session)
			if tt.interrupt {
				require.ErrorIs(t, err, errExecutionInterrupted)
			} else {
				require.NoError(t, err)
				current := &api.SandboxTemplate{}
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(version), current))
				recorded, err := parseExecutionIdentity(current.Annotations[executionIdentityAnnotation])
				require.NoError(t, err)
				require.Equal(t, executionIdentity{SandboxUID: "sandbox-uid", PodUID: "pod-uid", ContainerID: "containerd://original"}, recorded)
				if tt.conflict {
					require.Equal(t, "winner", current.Labels["editor"])
				}
			}
			require.Equal(t, tt.writes, writes)
			currentVersion := &api.SandboxTemplate{}
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(version), currentVersion))
			require.Equal(t, version.Labels[api.LabelTemplateName], currentVersion.Labels[api.LabelTemplateName])
			require.Empty(t, currentVersion.OwnerReferences)
		})
	}
}

func TestBuildStepErrorMessage(t *testing.T) {
	err := &buildStepError{index: 1, cause: context.Canceled}
	require.Equal(t, "build step 2 failed", err.Error())
	require.ErrorIs(t, err, context.Canceled)
}

func TestExecutionRequiresBusinessContainer(t *testing.T) {
	for _, missingTemplate := range []bool{false, true} {
		t.Run(map[bool]string{false: "business container required", true: "template required"}[missingTemplate], func(t *testing.T) {
			r, version, _, _ := executionFixture(t)
			if missingTemplate {
				version.Spec.Template = nil
			} else {
				version.Spec.Template.Spec.Containers[0].Name = "other"
			}
			_, err := r.reconcileExecution(t.Context(), version)
			require.NoError(t, err)
			current := &api.SandboxTemplate{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
			require.Equal(t, api.SandboxTemplatePhaseFailed, current.Status.Phase)
			require.Equal(t, buildReasonInvalidDefinition, meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition).Reason)
		})
	}
}

func TestControllerStorageFailures(t *testing.T) {
	for _, tt := range []struct {
		name      string
		operation string
		invoke    func(*Reconciler, *api.SandboxTemplate, *api.Sandbox) error
	}{
		{name: "reservation binding read", operation: "get", invoke: func(r *Reconciler, v *api.SandboxTemplate, _ *api.Sandbox) error {
			return r.bindExecutionSandbox(t.Context(), &executionSession{versionName: v.Name, versionUID: v.UID, namespace: v.Namespace})
		}},
		{name: "execution identity read", operation: "get", invoke: func(r *Reconciler, v *api.SandboxTemplate, box *api.Sandbox) error {
			return r.recordExecutionIdentity(t.Context(), &executionSession{versionName: v.Name, versionUID: v.UID, namespace: v.Namespace, sandboxUID: box.UID, podUID: "pod-uid", containerID: "containerd://original"})
		}},
		{name: "execution identity patch", operation: "patch", invoke: func(r *Reconciler, v *api.SandboxTemplate, box *api.Sandbox) error {
			return r.recordExecutionIdentity(t.Context(), &executionSession{versionName: v.Name, versionUID: v.UID, namespace: v.Namespace, sandboxUID: box.UID, podUID: "pod-uid", containerID: "containerd://original"})
		}},
		{name: "Version read", operation: "get", invoke: func(r *Reconciler, v *api.SandboxTemplate, _ *api.Sandbox) error {
			_, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(v)})
			return err
		}},
		{name: "execution result read", operation: "get", invoke: func(r *Reconciler, v *api.SandboxTemplate, _ *api.Sandbox) error {
			return r.executionStatus(t.Context(), v, api.SandboxTemplatePhaseBuilding, metav1.ConditionFalse, buildReasonProvisioned, "result")
		}},
		{name: "capture read", operation: "get", invoke: func(r *Reconciler, v *api.SandboxTemplate, _ *api.Sandbox) error {
			_, err := r.reconcileCapture(t.Context(), v)
			return err
		}},
		{name: "cleanup Sandbox read", operation: "get", invoke: func(r *Reconciler, v *api.SandboxTemplate, _ *api.Sandbox) error {
			_, err := r.reconcileBuildCleanup(t.Context(), v)
			return err
		}},
		{name: "draft expiry deletion", operation: "delete", invoke: func(r *Reconciler, v *api.SandboxTemplate, _ *api.Sandbox) error {
			_, err := r.requestDeletion(t.Context(), v, metav1.DeletePropagationForeground)
			return err
		}},
		{name: "checkpoint creation", operation: "create", invoke: func(r *Reconciler, v *api.SandboxTemplate, _ *api.Sandbox) error {
			_, err := r.reconcileCapture(t.Context(), v)
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, version, box, pod := executionFixture(t)
			version.Status.Conditions = []metav1.Condition{{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: buildReasonProvisioned, LastTransitionTime: metav1.Now()}}
			require.NoError(t, r.Status().Update(t.Context(), version))
			identity, err := parseExecutionIdentity(version.Annotations[executionIdentityAnnotation])
			require.NoError(t, err)
			if tt.name != "execution identity patch" {
				identity.ContainerID = pod.Status.ContainerStatuses[0].ContainerID
			}
			data, err := json.Marshal(identity)
			require.NoError(t, err)
			version.Annotations[executionIdentityAnnotation] = string(data)
			require.NoError(t, r.Update(t.Context(), version))
			failure := errors.New("storage unavailable")
			funcs := interceptor.Funcs{}
			switch tt.operation {
			case "get":
				funcs.Get = func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return failure
				}
			case "list":
				funcs.List = func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return failure }
			case "delete":
				funcs.Delete = func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return failure }
			case "patch":
				funcs.Patch = func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					return failure
				}
			case "create":
				funcs.Create = func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return failure }
			}
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), funcs)
			require.ErrorIs(t, tt.invoke(r, version, box), failure)
		})
	}
}

func TestCaptureWaitsForProgressAndBudget(t *testing.T) {
	for _, progress := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing progress", true: "identity still converging"}[progress], func(t *testing.T) {
			r, version, _, _ := executionFixture(t)
			if progress {
				version.Status.Conditions = []metav1.Condition{{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: buildReasonProvisioned, LastTransitionTime: metav1.Now()}}
			}
			require.NoError(t, r.Status().Update(t.Context(), version))
			_, err := r.reconcileCapture(t.Context(), version)
			require.NoError(t, err)
			current := &api.SandboxTemplate{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
			if progress {
				require.NotEqual(t, api.SandboxTemplatePhaseReady, current.Status.Phase)
			} else {
				require.Equal(t, buildReasonInterrupted, meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition).Reason)
			}
		})
	}
}
