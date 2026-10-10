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

package sandboxcr

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func TestColdStartClaim(t *testing.T) {
	for _, tt := range []struct {
		name, namespace, scope, selector, wantNS string
		wantError                                bool
	}{
		{name: "ordinary namespace", namespace: "team-a", wantNS: "team-a"},
		{name: "admin default namespace", wantNS: "default"},
		{name: "admin configured namespace", scope: "sandboxes", wantNS: "sandboxes"},
		{name: "outside watch namespace", namespace: "other", scope: "sandboxes", wantError: true},
		{name: "outside watch selector", selector: "team=other", wantError: true},
		{name: "claim label selector", selector: v1alpha1.LabelSandboxIsClaimed + "=true", wantNS: "default"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend, fc := NewTestInfra(t, config.SandboxManagerOptions{SandboxNamespace: tt.scope, SandboxLabelSelector: tt.selector})
			original := DefaultCreateSandbox
			DefaultCreateSandbox = func(ctx context.Context, sbx *v1alpha1.Sandbox, c client.Client) (*v1alpha1.Sandbox, error) {
				created, err := original(ctx, sbx, c)
				if err == nil {
					markSandboxReadyForTest(t, ctx, c, created, "10.0.0.2")
				}
				return created, err
			}
			t.Cleanup(func() { DefaultCreateSandbox = original })
			input := &infra.ColdStartOptions{
				Image: "python:3.11", Command: []string{"python3", "-c", "print('hello world')", "two words"},
				EnvVars:          map[string]string{"EXAMPLE": "initial-value"},
				ResourceLimits:   map[string]string{"cpu": "750m", "memory": "384Mi"},
				ResourceRequests: map[string]string{"cpu": "150m", "memory": "192Mi"},
				OS:               "linux", Architecture: "arm64",
			}
			opts := infra.ClaimSandboxOptions{
				Namespace: tt.namespace, User: "key-id", ColdStart: input,
				RuntimeConfig: []v1alpha1.RuntimeConfig{{Name: v1alpha1.RuntimeConfigForInjectAgentRuntime}},
				ClaimTimeout:  time.Second, WaitReadyTimeout: time.Second,
				ReserveFailedSandboxFor: ptr.To(time.Duration(0)),
			}
			sbx, metrics, err := backend.ClaimSandbox(t.Context(), opts)
			if tt.wantError {
				require.Error(t, err)
				require.Nil(t, sbx)
				var existing v1alpha1.SandboxList
				require.NoError(t, fc.List(t.Context(), &existing))
				assert.Empty(t, existing.Items)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, sbx)
			assert.Equal(t, infra.LockTypeCreate, metrics.LockType)
			var actual v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), client.ObjectKey{Namespace: sbx.GetNamespace(), Name: sbx.GetName()}, &actual))
			assert.Equal(t, tt.wantNS, actual.Namespace)
			assert.Equal(t, "key-id", actual.Annotations[v1alpha1.AnnotationOwner])
			assert.Equal(t, "key-id", actual.Spec.Template.Labels[v1alpha1.AnnotationOwner])
			assert.Equal(t, v1alpha1.True, actual.Labels[v1alpha1.LabelSandboxIsClaimed])
			assert.Empty(t, actual.Labels[v1alpha1.LabelSandboxPool])
			assert.Empty(t, actual.OwnerReferences)
			container := actual.Spec.Template.Spec.Containers[0]
			assert.Equal(t, input.Image, container.Image)
			assert.Equal(t, input.Command, container.Command)
			assert.Equal(t, []corev1.EnvVar{{Name: "EXAMPLE", Value: "initial-value"}}, container.Env)
			assert.Equal(t, resource.MustParse("750m"), container.Resources.Limits[corev1.ResourceCPU])
			assert.Equal(t, resource.MustParse("150m"), container.Resources.Requests[corev1.ResourceCPU])
			assert.Equal(t, resource.MustParse("384Mi"), container.Resources.Limits[corev1.ResourceMemory])
			assert.Equal(t, resource.MustParse("192Mi"), container.Resources.Requests[corev1.ResourceMemory])
			assert.Equal(t, map[string]string{corev1.LabelOSStable: "linux", corev1.LabelArchStable: "arm64"}, actual.Spec.Template.Spec.NodeSelector)
			assert.Equal(t, opts.RuntimeConfig, actual.Spec.Runtimes)
			// Each request gets a fresh instance without a shared source object.
			second, _, err := backend.ClaimSandbox(t.Context(), opts)
			require.NoError(t, err)
			assert.NotEqual(t, sbx.GetName(), second.GetName())
			var templates v1alpha1.SandboxSetList
			require.NoError(t, fc.List(t.Context(), &templates))
			assert.Empty(t, templates.Items)
		})
	}
}

func TestColdStartAdmissionAndCleanup(t *testing.T) {
	for _, tt := range []struct {
		name             string
		capacity         int
		ready            bool
		invalidResources bool
		wantCreates      int
		wantLive         int
		wantRelease      int
	}{
		{name: "admitted", capacity: 1, ready: true, wantCreates: 1, wantLive: 1},
		{name: "denied before create", capacity: 0},
		{name: "invalid quantity before admission", capacity: 1, invalidResources: true},
		{name: "readiness failure releases quota", capacity: 1, wantCreates: 1, wantRelease: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend, fc := NewTestInfra(t)
			tracker := newAdmissionQuotaTracker(t, tt.capacity)
			admission := tracker.admission()
			originalAcquire := admission.Acquire
			admission.Acquire = func(ctx context.Context, lock string, r infra.SandboxResource) error {
				assert.Equal(t, int64(750), r.Limits.CPUMilli)
				return originalAcquire(ctx, lock, r)
			}
			originalCreate := DefaultCreateSandbox
			creates := 0
			DefaultCreateSandbox = func(ctx context.Context, sbx *v1alpha1.Sandbox, c client.Client) (*v1alpha1.Sandbox, error) {
				creates++
				created, err := originalCreate(ctx, sbx, c)
				if err == nil && tt.ready {
					markSandboxReadyForTest(t, ctx, c, created, "10.0.0.2")
				}
				return created, err
			}
			t.Cleanup(func() { DefaultCreateSandbox = originalCreate })
			cpu := "750m"
			if tt.invalidResources {
				cpu = "invalid"
			}
			opts, err := ValidateAndInitClaimOptions(infra.ClaimSandboxOptions{
				Namespace: "default", User: "key-id", ColdStart: &infra.ColdStartOptions{Image: "python:3.11", ResourceLimits: map[string]string{"cpu": cpu}},
				Admission: admission, WaitReadyTimeout: 50 * time.Millisecond, ReserveFailedSandboxFor: ptr.To(time.Duration(0)),
			})
			require.NoError(t, err)
			_, _, err = TryClaimSandbox(t.Context(), opts, &backend.pickCache, backend.Cache, backend.claimLockChannel, backend.createLimiter)
			if tt.ready {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if tt.capacity == 0 {
				assert.Equal(t, managererrors.ErrorQuotaExceeded, managererrors.GetErrCode(err))
			}
			if tt.invalidResources {
				assert.Equal(t, managererrors.ErrorBadRequest, managererrors.GetErrCode(err))
				assert.Empty(t, tracker.acquireCalls())
			}
			assert.Equal(t, tt.wantCreates, creates)
			assert.Equal(t, tt.wantLive, tracker.liveCount())
			assert.Len(t, tracker.releaseCalls(), tt.wantRelease)
			var objects v1alpha1.SandboxList
			require.NoError(t, fc.List(t.Context(), &objects))
			if !tt.ready {
				assert.Empty(t, objects.Items)
			}
		})
	}
}

func TestColdStartExistingVolumes(t *testing.T) {
	for _, tt := range []struct {
		name      string
		volume    infra.ExistingVolumeMount
		duplicate bool
		wantError bool
	}{
		{name: "existing subpath", volume: infra.ExistingVolumeMount{Name: "data", VolumeName: "shared", MountPath: "/mnt/data", SubPath: "poc"}},
		{name: "read only", volume: infra.ExistingVolumeMount{Name: "data", VolumeName: "shared", MountPath: "/mnt/data", ReadOnly: true}},
		{name: "missing claim", volume: infra.ExistingVolumeMount{Name: "data", VolumeName: "missing", MountPath: "/mnt/data"}, wantError: true},
		{name: "foreign claim", volume: infra.ExistingVolumeMount{Name: "data", VolumeName: "foreign", MountPath: "/mnt/data"}, wantError: true},
		{name: "duplicate", volume: infra.ExistingVolumeMount{Name: "data", VolumeName: "shared", MountPath: "/mnt/data"}, duplicate: true, wantError: true},
		{name: "relative mount", volume: infra.ExistingVolumeMount{Name: "data", VolumeName: "shared", MountPath: "relative"}, wantError: true},
		{name: "parent traversal", volume: infra.ExistingVolumeMount{Name: "data", VolumeName: "shared", MountPath: "/mnt/data", SubPath: "ok/../secret"}, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend, fc := NewTestInfra(t)
			existing := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "shared"}}
			foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "foreign"}}
			require.NoError(t, fc.Create(t.Context(), existing))
			require.NoError(t, fc.Create(t.Context(), foreign))
			before := existing.DeepCopy()
			inputs := []infra.ExistingVolumeMount{tt.volume}
			if tt.duplicate {
				inputs = append(inputs, tt.volume)
			}
			sbx, _, err := newColdSandbox(t.Context(), infra.ClaimSandboxOptions{Namespace: "default", ColdStart: &infra.ColdStartOptions{Image: "python:3.11", Volumes: inputs}}, backend.Cache)
			if tt.wantError {
				require.Error(t, err)
				assert.Equal(t, managererrors.ErrorBadRequest, managererrors.GetErrCode(err))
				require.Nil(t, sbx)
			} else {
				require.NoError(t, err)
				pod := sbx.Spec.Template.Spec
				require.Len(t, pod.Volumes, 1)
				assert.Equal(t, &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared", ReadOnly: tt.volume.ReadOnly}, pod.Volumes[0].PersistentVolumeClaim)
				assert.Equal(t, []corev1.VolumeMount{{Name: "data", MountPath: "/mnt/data", SubPath: tt.volume.SubPath, ReadOnly: tt.volume.ReadOnly}}, pod.Containers[0].VolumeMounts)
			}
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(existing), existing))
			assert.Equal(t, before, existing, "existing claim must never be mutated or acquired")
			var sandboxes v1alpha1.SandboxList
			require.NoError(t, fc.List(t.Context(), &sandboxes))
			assert.Empty(t, sandboxes.Items)
		})
	}
}
